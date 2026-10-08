package worker

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
)

// levelLogger records each line with its level, so a test can count the
// warnings and infos a component wrote.
type levelLogger struct {
	mu    sync.Mutex
	lines []string // "LEVEL msg"
}

func (l *levelLogger) add(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg)
}
func (l *levelLogger) Debug(msg string, _ ...any) { l.add("DEBUG", msg) }
func (l *levelLogger) Info(msg string, _ ...any)  { l.add("INFO", msg) }
func (l *levelLogger) Warn(msg string, _ ...any)  { l.add("WARN", msg) }
func (l *levelLogger) Error(msg string, _ ...any) { l.add("ERROR", msg) }

// count is how many lines at level contain substr.
func (l *levelLogger) count(level, substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.HasPrefix(line, level+" ") && strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// fakeDisk is a disk the test fills and empties; reads counts the readings.
type fakeDisk struct {
	critical bool
	err      error
	reads    int
}

func (d *fakeDisk) read() (diskReading, error) {
	d.reads++
	if d.err != nil {
		return diskReading{dir: "/out"}, d.err
	}
	return diskReading{dir: "/out", free: 1 << 30, usedPct: 50, critical: d.critical}, nil
}

// queueBacklog adds n Queued backlog VODs, each with its feed_items partner.
func queueBacklog(t *testing.T, db *database.Database, ch string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		addSchedJob(t, db, &ch, id, database.StatusQueued, 1)
		addFeedItemRow(t, db, ch, id, "2026-07-10T00:00:00Z")
	}
}

// TestSchedulerHoldsBacklogWhileDiskCritical is D-disk's gate: a sweep admits
// no backlog VOD while the volume the jobs write to reads at or past the
// critical threshold, and the first sweep after it reads below admits them.
//
// Mutant: drop the diskGateClosed check from sweep — both jobs are admitted
// onto the full disk.
func TestSchedulerHoldsBacklogWhileDiskCritical(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 2)
	d := &fakeDisk{critical: true}
	s.readDisk = d.read
	queueBacklog(t, db, "UC_full", "full_a", "full_b")

	s.sweep()
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d backlog jobs onto a full disk, want 0", n)
	}
	for _, id := range []string{"full_a", "full_b"} {
		if row, _ := db.GetJob(id); row.Status != database.StatusQueued {
			t.Fatalf("%s: status = %s while the disk is full, want Queued", id, row.Status)
		}
	}

	d.critical = false
	s.sweep()
	if n := log.enqueueCount(); n != 2 {
		t.Errorf("admitted %d once the disk had room, want 2", n)
	}
}

// TestSchedulerDiskGateLogsOnceEachWay: the gate closing and reopening are
// each one line, however many sweeps the disk stays full or free for.
//
// Mutant: drop the critical != diskHeld comparison, logging the reading's
// verdict on every sweep — three warnings and two infos.
func TestSchedulerDiskGateLogsOnceEachWay(t *testing.T) {
	s, db, _ := testSchedulerSetup(t, 1)
	lg := &levelLogger{}
	s.log = lg
	d := &fakeDisk{critical: true}
	s.readDisk = d.read
	// The first reopened sweep admits one job; the other keeps a backlog
	// for the second to read the disk for.
	queueBacklog(t, db, "UC_log", "log_a", "log_b")

	for range 3 {
		s.sweep()
	}
	d.critical = false
	s.sweep()
	s.sweep()
	if d.reads != 5 {
		t.Fatalf("disk readings = %d over five sweeps with a backlog, want 5", d.reads)
	}

	if n := lg.count("WARN", "critical threshold"); n != 1 {
		t.Errorf("gate-closed warnings = %d over three full sweeps, want 1", n)
	}
	if n := lg.count("INFO", "backlog admission resumes"); n != 1 {
		t.Errorf("gate-open infos = %d over two free sweeps, want 1", n)
	}
}

// TestSchedulerDiskGateHoldsThroughFailedReading: a reading that fails leaves
// the gate where the last good one put it — closed stays closed, and the next
// good reading decides.
//
// Mutant: answer false (open) when the reading fails — the full disk's
// backlog is admitted on no evidence the space came back.
func TestSchedulerDiskGateHoldsThroughFailedReading(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	d := &fakeDisk{critical: true}
	s.readDisk = d.read
	queueBacklog(t, db, "UC_fail", "fail_a")

	s.sweep()
	d.err = errors.New("statfs: input/output error")
	s.sweep()
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d after a failed reading of a full disk, want 0", n)
	}
	d.err, d.critical = nil, false
	s.sweep()
	if n := log.enqueueCount(); n != 1 {
		t.Errorf("admitted %d once a good reading showed room, want 1", n)
	}
}

// TestSchedulerReadsDiskOnlyWithBacklog: a sweep with nothing Queued takes no
// disk reading — the heartbeat would otherwise query the volume once a
// minute on every install, backlog or none.
//
// Mutant: drop the len(channels) > 0 condition — the idle sweep reads.
func TestSchedulerReadsDiskOnlyWithBacklog(t *testing.T) {
	s, _, _ := testSchedulerSetup(t, 1)
	d := &fakeDisk{}
	s.readDisk = d.read
	s.sweep()
	if d.reads != 0 {
		t.Errorf("an idle sweep took %d disk readings, want 0", d.reads)
	}
}

// TestReadOutputDiskJudgesTheAlertsReading pins what the gate decides on: the
// disk alerts' reading — the configured output directory — judged by the
// critical threshold AT OR ABOVE it, and never while the threshold is 0.
//
// Mutants: read the staging directory instead of the output directory — the
// queried path is wrong; make AtCritical strict (>) — a volume exactly at the
// threshold reads below it; drop AtCritical's CriticalPercent > 0 guard — a
// zero threshold reads every volume critical.
func TestReadOutputDiskJudgesTheAlertsReading(t *testing.T) {
	w, _ := testWorkerSetup(t)
	out := filepath.Join(t.TempDir(), "archive")
	w.cfg.Paths.OutputDirectory = out
	w.cfg.Disk.CriticalPercent = 95
	var asked string
	used := 0.0
	w.diskSpace = func(path string) (*disk.DiskSpace, error) {
		asked = path
		return &disk.DiskSpace{Free: 5 << 30, Total: 100 << 30, UsedPct: used}, nil
	}

	for _, tc := range []struct {
		used float64
		want bool
	}{{94.9, false}, {95, true}, {99.5, true}} {
		used = tc.used
		r, err := w.readOutputDisk()
		if err != nil {
			t.Fatal(err)
		}
		if asked != out {
			t.Fatalf("read %q, want the output directory %q the disk alerts read", asked, out)
		}
		if r.critical != tc.want {
			t.Errorf("%.1f%% used against 95: critical = %v, want %v", tc.used, r.critical, tc.want)
		}
	}

	w.cfg.Disk.CriticalPercent = 0
	used = 100
	if r, _ := w.readOutputDisk(); r.critical {
		t.Errorf("a zero threshold read a full volume as critical")
	}
}

// TestWorkerWiresTheDiskGate is the gate through the worker's own scheduler:
// a Queued backlog VOD stays Queued while the output volume reads critical,
// and is admitted once it does not.
//
// Mutant: drop the readDisk wiring from NewDownloadWorker — the job is
// admitted onto the full disk.
func TestWorkerWiresTheDiskGate(t *testing.T) {
	w, db := testWorkerSetup(t)
	w.cfg.Disk.CriticalPercent = 95
	used := 99.0
	w.diskSpace = func(string) (*disk.DiskSpace, error) {
		return &disk.DiskSpace{Free: 1 << 30, Total: 100 << 30, UsedPct: used}, nil
	}
	w.scheduler.resolveSlots = func(string) int { return 1 }
	queueBacklog(t, db, "UC_wired", "wired_a")

	w.scheduler.sweep()
	if row, _ := db.GetJob("wired_a"); row.Status != database.StatusQueued {
		t.Fatalf("status = %s with the output volume at 99%%, want Queued", row.Status)
	}
	used = 50
	w.scheduler.sweep()
	if row, _ := db.GetJob("wired_a"); row.Status != database.StatusUpcoming {
		t.Errorf("status = %s once the volume had room, want Upcoming", row.Status)
	}
}
