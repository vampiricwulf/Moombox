package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// diskNotifyCooldown is how long the same warning LEVEL waits before repeating.
// A level change (warn -> critical) never waits it out.
const diskNotifyCooldown = 30 * time.Minute

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

	// thresholds are the configured warn/critical percentages, refreshed
	// before every reading (setThresholds), that an open alert's recovery
	// margin (config.DiskRecoveryMargin) is measured from. Zero disables the
	// hold for that level.
	thresholds config.DiskConfig

	// readFailing/readFailCount track the monitoring-failure streak, and
	// readFailNotified says whether that streak was reported — the first
	// failure never is.
	readFailing      bool
	readFailCount    int
	readFailNotified bool

	// state is where the open alerts above are persisted (restoreFrom), so
	// one open across a restart still gets its close. nil = memory only.
	state *openAlerts
}

func newDiskAlerts(notify notifications.Sender, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *diskAlerts {
	return &diskAlerts{notify: notify, log: log}
}

// restoreFrom seeds the alerter with the disk alerts a previous process left
// open and persists every later open and close to st. Called before the first
// reading: a space alert comes back at its level with its repeat cooldown
// running from when it was sent, so the first reading that clears the margin
// sends "Disk Space Recovered"; an open "Disk Monitoring Failed" comes back as
// a reported failure streak, so the first reading that succeeds sends
// "Disk Monitoring Recovered" — each exactly as the same readings would have
// without the restart.
func (d *diskAlerts) restoreFrom(st *openAlerts) {
	d.state = st
	open := st.snapshot().Disk
	if open == nil {
		return
	}
	if open.Level == "warn" || open.Level == "critical" {
		d.lastLevel, d.lastNotify = open.Level, open.NotifiedAt
	}
	if open.MonitoringFailed {
		d.readFailing = true
		d.readFailCount = diskReadFailuresBeforeAlert
		d.readFailNotified = true
	}
}

// restoreDiskGate starts the backlog admission gate closed when the previous
// process left a disk_critical alert open. The gate closes and reopens on the
// alert's own rules (config.DiskConfig.AtCritical, ClearOfCritical) but keeps
// its close in memory only, so a restart — an update, a crash, Stop/Start —
// opened it again: at 94% against 95 the new gate admitted backlog while the
// restored alert held critical on the same reading. Seeded from the alert's
// persisted level, the two still agree after the restart, and the first
// reading clear of the threshold ends both. Called before the worker starts.
func restoreDiskGate(st *openAlerts, gate interface{ RestoreDiskHold() }) {
	if open := st.snapshot().Disk; open != nil && open.Level == "critical" {
		gate.RestoreDiskHold()
	}
}

// persist writes the open set to the state store. A no-op without one, and
// a write only when it changed (openAlerts.update).
func (d *diskAlerts) persist() {
	d.state.update(func(doc *openAlertsDoc) {
		st := openDiskAlert{MonitoringFailed: d.readFailNotified}
		if d.lastLevel != "" {
			st.Level, st.NotifiedAt = d.lastLevel, d.lastNotify
		}
		doc.setDisk(st)
	})
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
	d.thresholds = config.DiskConfig{WarnPercent: warnPct, CriticalPercent: critPct}
}

// heldOpen reports whether a reading whose level is below the open alert's
// should still count as inside that incident: usage has not yet fallen
// config.DiskRecoveryMargin below the open level's threshold. The critical
// half is the rule the backlog admission gate reopens on (ClearOfCritical),
// so the alert steps down on the reading that resumes the backlog.
func (d *diskAlerts) heldOpen(ds *routes.DiskStatus) bool {
	switch {
	case d.lastLevel == "critical" && ds.WarnLevel != "critical":
		return !d.thresholds.ClearOfCritical(ds.UsedPct)
	case d.lastLevel == "warn" && ds.WarnLevel == "ok":
		return !d.thresholds.ClearOfWarn(ds.UsedPct)
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
	defer d.persist()
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
	defer d.persist()
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
