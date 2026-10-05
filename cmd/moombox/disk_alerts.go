package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// diskNotifyCooldown is how long the same warning LEVEL waits before repeating.
// A level change (warn -> critical) never waits it out.
const diskNotifyCooldown = 30 * time.Minute

// diskRecoveryMargin is how far, in percentage points, usage must fall below
// a threshold before an open alert at that level closes or steps down. Without
// it a volume sitting on the line — 90.0% one reading, 89.9% the next — sent a
// Warning and a Recovered on every six-minute check, ten an hour, because the
// cooldown only spaces repeats of the SAME level and an ok reading reset it.
const diskRecoveryMargin = 2.0

// diskReadFailuresBeforeAlert is how many consecutive failed readings the
// low-disk safety net must miss before the operator hears about it. The second
// failure is roughly twelve minutes in, which rides out a transient SMB blip or
// a flapping network volume without hiding a volume that has actually gone.
const diskReadFailuresBeforeAlert = 2

// diskAlerts owns the disk notification decision.
//
// It is a struct rather than four locals inside the stats ticker's closure so
// the decision can be tested: the all-clear this type adds is unreachable from
// any test while it lives inside a 190-line goroutine that also reads runtime
// memory stats and talks to the sidecar. Everything about the WARNING path is
// the behaviour that closure already had, moved.
//
// Not goroutine-safe: the stats ticker is its only caller and it is a single
// goroutine.
type diskAlerts struct {
	notify notifications.Sender
	log    interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// lastNotify and lastLevel implement the repeat cooldown. lastLevel is
	// non-empty exactly when a warning or critical alert HAS been sent and not
	// yet closed, which is what makes the all-clear conditional.
	lastNotify time.Time
	lastLevel  string

	// warnPct and critPct are the configured thresholds, refreshed before
	// every reading (setThresholds), that diskRecoveryMargin is measured
	// from. Zero disables the hold for that level.
	warnPct, critPct float64

	// readFailing/readFailCount track the monitoring-failure streak, and
	// readFailNotified says whether that streak was reported — the first
	// failure never is.
	readFailing      bool
	readFailCount    int
	readFailNotified bool
}

func newDiskAlerts(notify notifications.Sender, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *diskAlerts {
	return &diskAlerts{notify: notify, log: log}
}

// absOutputDir names the directory an operator can act on. An operator with
// several machines (or several volumes) cannot act on "output drive" alone,
// and the config default is the relative "./output".
func absOutputDir(outputDir string) string {
	if abs, err := filepath.Abs(outputDir); err == nil {
		return abs
	}
	return outputDir
}

// setThresholds records the configured warn/critical percentages the next
// reading's recovery margin is measured from. Called before every onReading,
// so a threshold edited in Settings applies from the next check.
func (d *diskAlerts) setThresholds(warnPct, critPct int) {
	d.warnPct, d.critPct = float64(warnPct), float64(critPct)
}

// heldOpen reports whether a reading whose level is below the open alert's
// should still count as inside that incident: usage has not yet fallen
// diskRecoveryMargin below the open level's threshold.
func (d *diskAlerts) heldOpen(ds *routes.DiskStatus) bool {
	switch {
	case d.lastLevel == "critical" && ds.WarnLevel != "critical":
		return d.critPct > 0 && ds.UsedPct > d.critPct-diskRecoveryMargin
	case d.lastLevel == "warn" && ds.WarnLevel == "ok":
		return d.warnPct > 0 && ds.UsedPct > d.warnPct-diskRecoveryMargin
	}
	return false
}

// onReading feeds one successful disk reading in.
//
// A reading can close TWO incidents at once: monitoring that had been reported
// as failing, and a space warning that was open before it failed. Both closes
// are sent, in that order — they are separate incidents with separate alerts,
// and collapsing them would leave one of the two alerts hanging.
func (d *diskAlerts) onReading(ds *routes.DiskStatus, outputDir string, now time.Time) {
	if d.readFailing {
		d.readFailing = false
		d.readFailCount = 0
		if d.readFailNotified {
			d.readFailNotified = false
			d.notify.Send("Disk Monitoring Recovered",
				"Disk space checks are answering again — the gauge and the low-disk alerts are live",
				notifications.TypeSuccess,
				[]notifications.Field{{Name: "Output Directory", Value: absOutputDir(outputDir)}},
				notifications.SendOptions{Event: "disk_ok"},
			)
		}
	}

	// Just below the open alert's threshold: still the same incident. Neither
	// a step down nor a recovery is announced until usage clears the margin.
	if d.heldOpen(ds) {
		return
	}

	if ds.WarnLevel != "ok" {
		if d.lastLevel == ds.WarnLevel && now.Sub(d.lastNotify) < diskNotifyCooldown {
			return
		}
		freeGB := float64(ds.Free) / (1024 * 1024 * 1024)
		level, ntype, event := "Warning", notifications.TypeWarning, "disk_warning"
		if ds.WarnLevel == "critical" {
			// Critical gets its own event so targets can route it separately
			// (e.g. a high-priority channel); eventAliases keeps plain
			// "disk_warning" filters receiving it too.
			level, ntype, event = "Critical", notifications.TypeError, "disk_critical"
		}
		d.notify.Send(
			fmt.Sprintf("Disk Space %s", level),
			fmt.Sprintf("%.1f%% used — %.1f GB free on output drive", ds.UsedPct, freeGB),
			ntype,
			[]notifications.Field{{Name: "Output Directory", Value: absOutputDir(outputDir)}},
			notifications.SendOptions{Event: event},
		)
		d.lastNotify = now
		d.lastLevel = ds.WarnLevel
		return
	}

	// Back to ok. Every warning and critical was an open incident with no end,
	// and the 30-minute repeat made the missing resolved line more visible,
	// not less (audit A2). lastLevel is non-empty only when an alert was
	// actually sent, so a healthy install never announces a recovery from
	// nothing.
	if d.lastLevel == "" {
		return
	}
	d.lastLevel = ""
	freeGB := float64(ds.Free) / (1024 * 1024 * 1024)
	d.notify.Send("Disk Space Recovered",
		fmt.Sprintf("%.1f%% used — %.1f GB free on output drive", ds.UsedPct, freeGB),
		notifications.TypeSuccess,
		[]notifications.Field{{Name: "Output Directory", Value: absOutputDir(outputDir)}},
		notifications.SendOptions{Event: "disk_ok"},
	)
}

// onReadFailure feeds one failed disk reading in (volume offline, I/O error).
func (d *diskAlerts) onReadFailure(outputDir string) {
	d.readFailCount++
	if !d.readFailing {
		d.log.Warn("[Disk] disk space check failed; gauge and low-disk alerts frozen until it recovers",
			"outputDir", outputDir)
		d.readFailing = true
	}
	// The safety net dying is itself alert-worthy: with monitoring frozen, the
	// drive can fill unnoticed. Once per streak, on the second consecutive
	// failure, to ride out a transient blip.
	if d.readFailCount == diskReadFailuresBeforeAlert {
		d.readFailNotified = true
		d.notify.Send("Disk Monitoring Failed",
			"Disk space checks are failing (volume offline or I/O error) — low-disk alerts are suspended until monitoring recovers",
			notifications.TypeError,
			[]notifications.Field{{Name: "Output Directory", Value: absOutputDir(outputDir)}},
			notifications.SendOptions{Event: "disk_warning"},
		)
	}
}
