package worker

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
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

// fakeDisk is a disk the test fills and empties, judged against a critical
// threshold of 95 as readOutputDisk judges the real one; reads counts the
// readings.
type fakeDisk struct {
	used  float64
	err   error
	reads int
}

func (d *fakeDisk) read() (diskReading, error) {
	d.reads++
	if d.err != nil {
		return diskReading{dir: "/out"}, d.err
	}
	return judgeDisk("/out", &disk.DiskSpace{Free: 1 << 30, Total: 100 << 30, UsedPct: d.used},
		config.DiskConfig{CriticalPercent: 95}), nil
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
	d := &fakeDisk{used: 99}
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

	d.used = 50
	s.sweep()
	if n := log.enqueueCount(); n != 2 {
		t.Errorf("admitted %d once the disk had room, want 2", n)
	}
}

// TestSchedulerDiskGateLogsOnceEachWay: the gate closing and reopening are
// each one line, however many sweeps the disk stays full or free for.
//
// Mutant: drop the closed != diskHeld comparison, logging the gate's
// verdict on every sweep — three warnings and two infos.
func TestSchedulerDiskGateLogsOnceEachWay(t *testing.T) {
	s, db, _ := testSchedulerSetup(t, 1)
	lg := &levelLogger{}
	s.log = lg
	d := &fakeDisk{used: 99}
	s.readDisk = d.read
	// The first reopened sweep admits one job; the other keeps a backlog
	// for the second to read the disk for.
	queueBacklog(t, db, "UC_log", "log_a", "log_b")

	for range 3 {
		s.sweep()
	}
	d.used = 50
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
	d := &fakeDisk{used: 99}
	s.readDisk = d.read
	queueBacklog(t, db, "UC_fail", "fail_a")

	s.sweep()
	d.err = errors.New("statfs: input/output error")
	s.sweep()
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d after a failed reading of a full disk, want 0", n)
	}
	d.err, d.used = nil, 50
	s.sweep()
	if n := log.enqueueCount(); n != 1 {
		t.Errorf("admitted %d once a good reading showed room, want 1", n)
	}
}

// TestSchedulerReadsDiskOnlyWithBacklog: a sweep with nothing Queued takes no
// disk reading while the gate is open — the heartbeat would otherwise query
// the volume once a minute on every install, backlog or none.
//
// Mutant: read on every sweep (len(channels) > 0 || true) — the idle sweep
// reads.
func TestSchedulerReadsDiskOnlyWithBacklog(t *testing.T) {
	s, _, _ := testSchedulerSetup(t, 1)
	d := &fakeDisk{}
	s.readDisk = d.read
	s.sweep()
	if d.reads != 0 {
		t.Errorf("an idle sweep took %d disk readings, want 0", d.reads)
	}
}

// TestSchedulerClosedDiskGateReadsWithoutBacklog: a closed gate reads the
// disk on every sweep, backlog or none, so it reopens on the reading that
// clears the threshold even when nothing waits for it. Read only with a
// backlog, a close outlived its incident: the operator cancelled the queued
// backlog and freed space, the disk alert closed, and the backlog a later
// scan queued at 94% — inside the margin, with no critical reading since the
// volume cleared — waited on the close from before. Once reopened, an idle
// sweep reads nothing again.
//
// Mutant: read only with a backlog (drop `|| s.diskHeld`) — the idle sweeps
// take no reading and the new backlog is held at 94%.
func TestSchedulerClosedDiskGateReadsWithoutBacklog(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	d := &fakeDisk{used: 99}
	s.readDisk = d.read
	queueBacklog(t, db, "UC_idle", "idle_a")
	s.sweep() // 99%: closes
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d at 99%%, want 0", n)
	}

	// The operator cancels the backlog and frees space.
	if !db.UpdateJobFieldsIf("idle_a", database.StatusQueued, map[string]any{"status": database.StatusCancelled}) {
		t.Fatal("cancel of the queued backlog did not apply")
	}
	d.used = 50
	reads := d.reads
	s.sweep()
	if d.reads != reads+1 {
		t.Errorf("the closed gate took %d readings in a sweep with no backlog, want 1", d.reads-reads)
	}
	reads = d.reads
	s.sweep()
	if d.reads != reads {
		t.Errorf("the reopened gate took %d readings in a sweep with no backlog, want 0", d.reads-reads)
	}

	// Usage climbs back to 94%, below the threshold, and a scan queues more.
	d.used = 94
	queueBacklog(t, db, "UC_idle", "idle_b")
	s.sweep()
	if n := log.enqueueCount(); n != 1 {
		t.Errorf("admitted %d at 94%% after the volume had cleared to 50%%, want 1: the gate held on a close from an incident that ended", n)
	}
}

// TestSchedulerRestoredDiskHoldKeepsTheGateClosed: a process whose previous
// run left a disk_critical alert open starts its gate closed
// (RestoreDiskHold), so the margin holds across a restart. The gate keeps its
// close in memory, and a new process's gate started open: its first sweep
// read 94% against 95 as neither critical nor clear, kept the open state, and
// admitted backlog onto the disk the last run had stopped admitting to, while
// the restored alert held critical on the same reading.
//
// Mutant: RestoreDiskHold leaves diskHeld false — 94% admits.
func TestSchedulerRestoredDiskHoldKeepsTheGateClosed(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 2)
	lg := &levelLogger{}
	s.log = lg
	d := &fakeDisk{used: 94}
	s.readDisk = d.read
	queueBacklog(t, db, "UC_restart", "restart_a", "restart_b")

	s.RestoreDiskHold()
	s.sweep()
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d at 94%% after a restart with disk_critical open, want 0: the gate reopened inside the recovery margin", n)
	}
	d.used = 93
	s.sweep()
	if n := log.enqueueCount(); n != 2 {
		t.Errorf("admitted %d at 93.0%%, 2 points below the threshold, want 2", n)
	}
	if n := lg.count("WARN", "disk_critical alert was still open"); n != 1 {
		t.Errorf("restored-hold warnings = %d, want 1", n)
	}
	if n := lg.count("INFO", "backlog admission resumes"); n != 1 {
		t.Errorf("gate-open infos = %d, want 1", n)
	}
}

// TestSchedulerDiskGateReopensOnlyPastTheRecoveryMargin: the gate closes at
// the critical threshold and reopens only once usage is DiskRecoveryMargin
// points below it, the reading an open disk_critical alert steps down on.
// Reopened on the first reading under the threshold, a volume sitting on the
// line admitted a backlog VOD every time it dipped under it. An open gate
// still closes at the threshold, not at the margin.
//
// Mutants: reopen on any reading below the threshold (closed = r.critical) —
// 94.9% admits; close on a reading inside the margin (closed = !r.clear) — the
// open gate holds at 94%; make ClearOfCritical strict (<) — 93.0% stays
// closed.
func TestSchedulerDiskGateReopensOnlyPastTheRecoveryMargin(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	lg := &levelLogger{}
	s.log = lg
	d := &fakeDisk{used: 94}
	s.readDisk = d.read
	queueBacklog(t, db, "UC_margin", "margin_a", "margin_b", "margin_c")

	s.sweep() // inside the margin, but the gate was never closed
	if n := log.enqueueCount(); n != 1 {
		t.Fatalf("admitted %d at 94%% with the gate open, want 1: it closes at the threshold, not the margin", n)
	}
	s.resolveSlots = func(string) int { return 3 } // room for the rest

	for _, used := range []float64{95, 94.9, 93.1} {
		d.used = used
		s.sweep()
		if n := log.enqueueCount(); n != 1 {
			t.Fatalf("admitted %d in all at %.1f%% after the disk reached 95%%, want still 1", n, used)
		}
	}
	d.used = 93
	s.sweep()
	if n := log.enqueueCount(); n != 3 {
		t.Errorf("admitted %d in all at 93.0%%, 2 points below the threshold, want 3", n)
	}
	if n := lg.count("WARN", "critical threshold"); n != 1 {
		t.Errorf("gate-closed warnings = %d, want 1", n)
	}
	if n := lg.count("INFO", "backlog admission resumes"); n != 1 {
		t.Errorf("gate-open infos = %d, want 1", n)
	}
}

// TestReadOutputDiskJudgesTheAlertsReading pins what the gate decides on: the
// disk alerts' reading — the configured output directory — judged critical AT
// OR ABOVE the threshold and clear DiskRecoveryMargin points below it, never
// critical and always clear while the threshold is 0.
//
// Mutants: read the staging directory instead of the output directory — the
// queried path is wrong; make AtCritical strict (>) — a volume exactly at the
// threshold reads below it; drop AtCritical's CriticalPercent > 0 guard — a
// zero threshold reads every volume critical; measure clear from the
// threshold itself (no margin) — 94.9% reads clear.
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
		used            float64
		critical, clear bool
	}{{50, false, true}, {93, false, true}, {93.1, false, false}, {94.9, false, false}, {95, true, false}, {99.5, true, false}} {
		used = tc.used
		r, err := w.readOutputDisk()
		if err != nil {
			t.Fatal(err)
		}
		if asked != out {
			t.Fatalf("read %q, want the output directory %q the disk alerts read", asked, out)
		}
		if r.critical != tc.critical || r.clear != tc.clear {
			t.Errorf("%.1f%% used against 95: critical, clear = %v, %v, want %v, %v",
				tc.used, r.critical, r.clear, tc.critical, tc.clear)
		}
	}

	w.cfg.Disk.CriticalPercent = 0
	used = 100
	if r, _ := w.readOutputDisk(); r.critical || !r.clear {
		t.Errorf("a zero threshold read a full volume as critical (%v) or not clear (%v)", r.critical, r.clear)
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
