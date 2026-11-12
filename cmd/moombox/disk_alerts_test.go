package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

func diskReading(level string, usedPct float64) *routes.DiskStatus {
	return &routes.DiskStatus{Free: 40 << 30, Total: 1000 << 30, UsedPct: usedPct, WarnLevel: level}
}

// TestDiskAllClearClosesAWarningThatWasSent is audit A2. Every warning was an
// open incident with no end, and the 30-minute repeat made the missing
// resolved line more visible, not less.
//
// Mutants this kill:
//   - sending the all-clear unconditionally: a healthy install announces a
//     recovery on its first reading, and again after every ok reading.
//   - not clearing lastLevel: the second warning episode never closes.
func TestDiskAllClearClosesAWarningThatWasSent(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()

	// An ok reading with no warning behind it says nothing.
	d.onReading(diskReading("ok", 40), "./output", now)
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("an ok reading recorded %d calls, want 0", got)
	}

	d.onReading(diskReading("warn", 91), "./output", now)
	if got := len(rec.ByEvent("disk_warning")); got != 1 {
		t.Fatalf("recorded %d disk_warning calls, want 1", got)
	}
	rec.Reset()

	d.onReading(diskReading("ok", 40), "./output", now.Add(time.Minute))
	calls := rec.ByEvent("disk_ok")
	if len(calls) != 1 {
		t.Fatalf("recorded %d disk_ok calls, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeSuccess {
		t.Errorf("type = %v, want TypeSuccess", calls[0].Type)
	}

	rec.Reset()
	d.onReading(diskReading("ok", 40), "./output", now.Add(2*time.Minute))
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a second ok reading recorded %d calls, want 0", got)
	}
}

// TestDiskWarningCooldownIsUnchanged guards the behaviour this task LIFTS but
// must not alter: a level change sends immediately, the same level waits 30
// minutes.
func TestDiskWarningCooldownIsUnchanged(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()

	d.onReading(diskReading("warn", 91), "./output", now)
	d.onReading(diskReading("warn", 92), "./output", now.Add(5*time.Minute))
	if got := len(rec.Calls()); got != 1 {
		t.Fatalf("recorded %d calls inside the cooldown, want 1", got)
	}
	d.onReading(diskReading("critical", 97), "./output", now.Add(6*time.Minute))
	if got := len(rec.ByEvent("disk_critical")); got != 1 {
		t.Fatalf("a level change recorded %d disk_critical calls, want 1 — it must not wait out the cooldown", got)
	}
	d.onReading(diskReading("critical", 98), "./output", now.Add(40*time.Minute))
	if got := len(rec.ByEvent("disk_critical")); got != 2 {
		t.Fatalf("recorded %d disk_critical calls after the cooldown expired, want 2", got)
	}
}

// TestDiskMonitoringRecoveredClosesAReadFailure.
//
// Mutants this kill:
//   - announcing a recovery after the FIRST failed read, which never alerted
//     (the alert is on the second, ~12 minutes in).
//   - not resetting the counter: the next failure streak never alerts.
func TestDiskMonitoringRecoveredClosesAReadFailure(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()

	d.onReadFailure("./output")
	d.onReading(diskReading("ok", 40), "./output", now)
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("one failed read then a recovery recorded %d calls, want 0 — nothing had been reported", got)
	}

	d.onReadFailure("./output")
	d.onReadFailure("./output")
	if got := len(rec.Calls()); got != 1 {
		t.Fatalf("recorded %d calls for a two-read failure streak, want 1", got)
	}
	rec.Reset()

	d.onReading(diskReading("ok", 40), "./output", now.Add(time.Minute))
	calls := rec.ByEvent("disk_ok")
	if len(calls) != 1 {
		t.Fatalf("recorded %d disk_ok calls after monitoring recovered, want 1", len(calls))
	}
	if calls[0].Title != "Disk Monitoring Recovered" {
		t.Errorf("title = %q", calls[0].Title)
	}
}

// TestDiskCompositeCloseSendsBothIncidents: one reading can end TWO incidents —
// monitoring that was reported as failing, and a space warning that was open
// before it failed. Both are separate alerts, so both get their own close.
//
// Mutants this kills:
//   - a `return` after the monitoring close: the space alert hangs forever.
//   - swapping the two blocks: the closes arrive in the wrong order.
func TestDiskCompositeCloseSendsBothIncidents(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()

	d.onReading(diskReading("warn", 91), "./output", now) // opens the space incident
	d.onReadFailure("./output")                           // 1st failed read: silent
	d.onReadFailure("./output")                           // 2nd: "Disk Monitoring Failed"
	rec.Reset()

	d.onReading(diskReading("ok", 40), "./output", now.Add(20*time.Minute))
	got := rec.Calls()
	if len(got) != 2 {
		t.Fatalf("a reading that closes both incidents recorded %d embeds, want 2", len(got))
	}
	if got[0].Title != "Disk Monitoring Recovered" || got[1].Title != "Disk Space Recovered" {
		t.Errorf("order = %q, %q — the monitoring close comes first", got[0].Title, got[1].Title)
	}
}

// TestDiskAlertsNameTheAbsoluteOutputDirectory: every disk alert names the
// directory the operator can act on. The failure alert alone carried the raw
// config value, so one incident read "./output" when it opened and an
// absolute path when it closed.
func TestDiskAlertsNameTheAbsoluteOutputDirectory(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()
	want := absOutputDir("./output")
	if !filepath.IsAbs(want) {
		t.Fatalf("absOutputDir(%q) = %q, not absolute", "./output", want)
	}

	d.onReading(diskReading("warn", 91), "./output", now)
	d.onReadFailure("./output")
	d.onReadFailure("./output")
	d.onReading(diskReading("ok", 40), "./output", now.Add(time.Minute))

	calls := rec.Calls()
	if len(calls) != 4 {
		t.Fatalf("recorded %d calls, want 4 (warning, monitoring failed, monitoring recovered, space recovered)", len(calls))
	}
	for _, c := range calls {
		if got, ok := c.Field("Output Directory"); !ok || got != want {
			t.Errorf("%q: Output Directory = %q (present %v), want %q", c.Title, got, ok, want)
		}
	}
}

// TestDiskAlertsHoldAnIncidentAcrossAThresholdFlap: the cooldown spaces only
// repeats of the SAME level, and an ok reading reset it, so a volume sitting
// on the warn line (90.0% then 89.9%) sent a Warning and a Recovered on every
// six-minute check, and one on the critical line alternated Critical and
// Warning. An open alert now holds until usage falls config.DiskRecoveryMargin below
// its threshold.
//
// Mutants: dropping the heldOpen check — the flaps alert every reading;
// measuring the margin from the wrong threshold — the step-down or the
// recovery arrives at the wrong reading.
func TestDiskAlertsHoldAnIncidentAcrossAThresholdFlap(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	d.setThresholds(90, 95)
	now := time.Now()
	tick := func(level string, pct float64) {
		now = now.Add(6 * time.Minute)
		d.onReading(diskReading(level, pct), "./output", now)
	}
	events := func() []string {
		var out []string
		for _, c := range rec.Calls() {
			out = append(out, c.Opts.Event)
		}
		return out
	}

	for range 5 { // an hour on the warn line
		tick("warn", 90.0)
		tick("ok", 89.9)
	}
	// The opening Warning and its one 30-minute repeat; never a Recovered.
	if got := events(); len(got) != 2 || got[0] != "disk_warning" || got[1] != "disk_warning" {
		t.Fatalf("an hour of warn-line flap sent %v, want [disk_warning disk_warning]", got)
	}
	rec.Reset()
	tick("ok", 88.1)
	if got := events(); len(got) != 0 {
		t.Fatalf("88.1%% is inside the 2-point margin, but %v was sent", got)
	}
	tick("ok", 87.9)
	if got := events(); len(got) != 1 || got[0] != "disk_ok" {
		t.Fatalf("clearing the margin sent %v, want a disk_ok", got)
	}

	rec.Reset()
	tick("critical", 95.0)
	for range 2 { // under half an hour on the critical line
		tick("warn", 94.9)
		tick("critical", 95.0)
	}
	if got := events(); len(got) != 1 || got[0] != "disk_critical" {
		t.Fatalf("critical-line flap sent %v, want one disk_critical", got)
	}
	tick("warn", 92.5)
	if got := events(); len(got) != 2 || got[1] != "disk_warning" {
		t.Fatalf("stepping down past the critical margin sent %v, want a disk_warning", got)
	}
}

// TestDiskAlertsJudgeTheFullerVolumeAndNameIt: with staging on a volume of
// its own, the fuller of the two volumes opens the one alert and is named in
// it; the recovery reports the volume the alert named; a tie, or an emptier
// staging volume, is the output directory's as before.
//
// Mutants: judge the output reading alone — a full staging volume sends
// nothing; name the output directory in a staging alert; report the output
// volume in the staging alert's recovery; send one alert per volume.
func TestDiskAlertsJudgeTheFullerVolumeAndNameIt(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	d.setThresholds(90, 95)
	now := time.Now()
	stg := filepath.Join(t.TempDir(), "scratch")

	d.onVolumes(diskReading("ok", 50), "./output", diskReading("critical", 97), stg, now)
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Title != "Disk Space Critical" || calls[0].Opts.Event != "disk_critical" {
		t.Fatalf("calls = %+v, want one Disk Space Critical", calls)
	}
	if got, _ := calls[0].Field("Staging Directory"); got != stg {
		t.Errorf("Staging Directory field = %q, want %q", got, stg)
	}
	if _, ok := calls[0].Field("Output Directory"); ok {
		t.Error("an alert for the staging volume named the output directory")
	}
	if want := "97.0% used — 40.0 GB free on staging drive"; calls[0].Description != want {
		t.Errorf("description = %q, want %q", calls[0].Description, want)
	}
	rec.Reset()

	// 94 against 95 is inside the recovery margin: still the same incident.
	d.onVolumes(diskReading("ok", 50), "./output", diskReading("warn", 94), stg, now.Add(time.Minute))
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a staging reading inside the margin recorded %d calls, want 0", got)
	}

	d.onVolumes(diskReading("ok", 50), "./output", diskReading("ok", 10), stg, now.Add(2*time.Minute))
	calls = rec.Calls()
	if len(calls) != 1 || calls[0].Title != "Disk Space Recovered" {
		t.Fatalf("calls = %+v, want one Disk Space Recovered", calls)
	}
	if got, _ := calls[0].Field("Staging Directory"); got != stg {
		t.Errorf("the recovery's Staging Directory field = %q, want the volume the alert named (%q)", got, stg)
	}
	if want := "10.0% used — 40.0 GB free on staging drive"; calls[0].Description != want {
		t.Errorf("recovery description = %q, want %q", calls[0].Description, want)
	}
	rec.Reset()

	// The output volume fuller, or level with staging: the alert is the
	// output directory's, worded as it always was.
	d.onVolumes(diskReading("warn", 91), "./output", diskReading("warn", 91), stg, now.Add(3*time.Minute))
	calls = rec.Calls()
	if len(calls) != 1 || calls[0].Title != "Disk Space Warning" {
		t.Fatalf("calls = %+v, want one Disk Space Warning", calls)
	}
	if _, ok := calls[0].Field("Output Directory"); !ok {
		t.Error("a tie did not name the output directory")
	}
	if want := "91.0% used — 40.0 GB free on output drive"; calls[0].Description != want {
		t.Errorf("description = %q, want %q", calls[0].Description, want)
	}
}

// TestDiskAlertsReadStagingOnlyOnAnotherVolume: the second reading is taken
// only when staging is on another volume, judged by the alerts' thresholds,
// and a staging volume that cannot be read leaves the output reading to
// decide alone, with one warning per streak.
func TestDiskAlertsReadStagingOnlyOnAnotherVolume(t *testing.T) {
	log := &countingLogger{}
	d := newDiskAlerts(notificationtest.New(), log)
	d.setThresholds(90, 95)
	split := true
	var readErr error
	d.sameVolume = func(a, b string) bool { return !split }
	d.diskSpace = func(string) (*disk.DiskSpace, error) {
		if readErr != nil {
			return nil, readErr
		}
		return &disk.DiskSpace{Free: 1 << 30, Total: 100 << 30, UsedPct: 96}, nil
	}

	if st := d.readStaging("./output", "./staging"); st == nil || st.WarnLevel != "critical" || st.UsedPct != 96 {
		t.Fatalf("staging on its own volume at 96%%: %+v, want a critical reading", st)
	}
	readErr = errors.New("offline")
	if st := d.readStaging("./output", "./staging"); st != nil {
		t.Errorf("an unreadable staging volume gave %+v, want nil", st)
	}
	d.readStaging("./output", "./staging")
	if log.warns != 1 {
		t.Errorf("two failed staging readings logged %d warnings, want 1 for the streak", log.warns)
	}
	readErr = nil
	split = false
	if st := d.readStaging("./output", "./staging"); st != nil {
		t.Errorf("one volume gave a second reading %+v, want nil", st)
	}
}

// TestFailedBootReadingCountsAsTheFirstFailure: with the output volume
// absent at start, the boot reading is the streak's first failure, so the
// ticker's first failed check sends "Disk Monitoring Failed" — not its
// second, six minutes later. A boot reading that succeeded is judged like
// any other.
//
// Mutant: ignore a nil boot reading — the first ticker failure sends nothing.
func TestFailedBootReadingCountsAsTheFirstFailure(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	d.setThresholds(90, 95)
	d.onBootReading(nil, "./output", "./staging", time.Now())
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("the failed boot reading alone recorded %d calls, want 0", got)
	}
	d.onReadFailure("./output")
	if calls := rec.Calls(); len(calls) != 1 || calls[0].Title != "Disk Monitoring Failed" {
		t.Fatalf("calls after the ticker's first failed check = %+v, want Disk Monitoring Failed", calls)
	}

	rec = notificationtest.New()
	d = newDiskAlerts(rec, &nopLogger{})
	d.setThresholds(90, 95)
	d.sameVolume = func(a, b string) bool { return true }
	d.onBootReading(diskReading("critical", 97), "./output", "./staging", time.Now())
	if got := len(rec.ByEvent("disk_critical")); got != 1 {
		t.Errorf("a full volume at boot sent %d criticals, want 1", got)
	}
}

// countingLogger counts Warn lines.
type countingLogger struct {
	nopLogger
	warns int
}

func (l *countingLogger) Warn(string, ...any) { l.warns++ }
