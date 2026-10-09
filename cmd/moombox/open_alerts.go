package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// openAlertsFileName is the open-alert state file, kept beside the database.
const openAlertsFileName = "open-alerts.json"

// openAlertsPath is where the open-alert state lives for a database path: the
// same directory, so a Docker volume or a moved install carries both.
func openAlertsPath(databasePath string) string {
	return filepath.Join(filepath.Dir(databasePath), openAlertsFileName)
}

// openAlertsDoc is the file's whole content: every alert that has been sent
// and whose close has not, one field per family.
type openAlertsDoc struct {
	// Disk is the disk family (diskAlerts): an open space warning or
	// critical, and an open "Disk Monitoring Failed". Nil when neither is.
	Disk *openDiskAlert `json:"disk,omitempty"`
	// SidecarDown is an open "BotGuard Sidecar Down" (sidecarAlerts).
	SidecarDown bool `json:"sidecarDown,omitempty"`
	// Channels holds, per platform, the channels whose "Channel Not
	// Responding" is open (channelIncidents).
	Channels map[string][]string `json:"channels,omitempty"`
	// Auth holds, per platform, when its auth failure was last announced
	// (withAuthFailureCooldown) — the stamp is both the open record and the
	// 30-minute repeat cooldown, so it carries the time.
	Auth map[string]time.Time `json:"auth,omitempty"`
}

// openDiskAlert is diskAlerts' open state.
type openDiskAlert struct {
	// Level is the open space alert's level, "warn" or "critical"; "" when
	// none is open. NotifiedAt is when it was last sent, which the repeat
	// cooldown runs from.
	Level      string    `json:"level,omitempty"`
	NotifiedAt time.Time `json:"notifiedAt,omitzero"`
	// MonitoringFailed is an open "Disk Monitoring Failed".
	MonitoringFailed bool `json:"monitoringFailed,omitempty"`
}

// openAlerts persists which alerts are open, so an alert open across a
// restart still gets its close.
//
// The four alerters each decide on their own when an alert opens and when it
// closes, and each kept that decision in memory only: the process that sent
// "Disk Space Critical" was the only one that could ever send "Disk Space
// Recovered", and a restart in between — an update, a settings change, a
// crash — left the alert open in every channel it went to, for good. Each
// alerter now writes its open set here on every open and close, and is seeded
// from it at boot, before anything can observe the system healthy, so the
// first healthy observation after the restart sends the close exactly as it
// would have without the restart.
//
// Written whole and atomically (utils.WriteFileAtomic) on every change, under
// the mutex so two writes land in the order they were made. A write that
// fails is logged and the in-memory state stands: the cost is a close that a
// LATER restart may miss, never a wrong one. All methods are nil-safe, which
// is what lets an alerter built without a store (tests, the first-run path)
// behave exactly as it did before.
type openAlerts struct {
	path string
	log  interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	mu  sync.Mutex
	doc openAlertsDoc
	// written is the last content written (or read), so an update that
	// changes nothing writes nothing.
	written []byte
}

// loadOpenAlerts reads the state file at path. A missing or unreadable file
// means nothing is open, said with one Warn, and is replaced at once by an
// empty one so the next start does not say it again.
func loadOpenAlerts(path string, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *openAlerts {
	a := &openAlerts{path: path, log: log}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		log.Warn("open-alert state: no state file yet, so no alert is carried over from a previous run", "path", path)
	case err != nil:
		log.Warn("open-alert state: the state file could not be read; treating no alert as open", "path", path, "err", err)
	default:
		if jerr := json.Unmarshal(raw, &a.doc); jerr != nil {
			a.doc = openAlertsDoc{}
			log.Warn("open-alert state: the state file is corrupt; treating no alert as open", "path", path, "err", jerr)
		} else {
			a.written = raw
			return a
		}
	}
	a.mu.Lock()
	a.persistLocked()
	a.mu.Unlock()
	return a
}

// update applies fn to the state and writes the file when it changed.
func (a *openAlerts) update(fn func(d *openAlertsDoc)) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.doc)
	a.persistLocked()
}

// persistLocked writes the current state unless it is what was last written.
// Caller holds a.mu.
func (a *openAlerts) persistLocked() {
	data, err := json.MarshalIndent(a.doc, "", "  ")
	if err != nil {
		a.log.Warn("open-alert state: could not encode the state", "err", err)
		return
	}
	data = append(data, '\n')
	if a.written != nil && string(data) == string(a.written) {
		return
	}
	if err := utils.WriteFileAtomic(a.path, data, 0o644); err != nil {
		a.log.Warn("open-alert state: could not write the state file; an alert open now may get no close after a restart",
			"path", a.path, "err", err)
		return
	}
	a.written = data
}

// snapshot returns a copy of the current state.
func (a *openAlerts) snapshot() openAlertsDoc {
	if a == nil {
		return openAlertsDoc{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	d := openAlertsDoc{SidecarDown: a.doc.SidecarDown}
	if a.doc.Disk != nil {
		disk := *a.doc.Disk
		d.Disk = &disk
	}
	if len(a.doc.Channels) > 0 {
		d.Channels = make(map[string][]string, len(a.doc.Channels))
		for p, ids := range a.doc.Channels {
			d.Channels[p] = slices.Clone(ids)
		}
	}
	if len(a.doc.Auth) > 0 {
		d.Auth = make(map[string]time.Time, len(a.doc.Auth))
		for p, at := range a.doc.Auth {
			d.Auth[p] = at
		}
	}
	return d
}

// authPlatforms lists the platforms whose auth failure is open, sorted.
func (a *openAlerts) authPlatforms() []string {
	var out []string
	for platform := range a.snapshot().Auth {
		out = append(out, platform)
	}
	slices.Sort(out)
	return out
}

// The setters below are on the DOCUMENT, not the store, so an alerter can
// read its own state inside update's callback: the value written is then the
// one current when the write happens, and of two racing transitions the one
// that writes last writes the newer state. The lock order is always the
// store's mutex, then the alerter's — no alerter calls into the store while
// holding its own.

// setDisk records diskAlerts' open state; the zero value closes the family.
func (d *openAlertsDoc) setDisk(st openDiskAlert) {
	if st == (openDiskAlert{}) {
		d.Disk = nil
		return
	}
	d.Disk = &st
}

// setChannel records one channel's "Channel Not Responding" as open or
// closed.
func (d *openAlertsDoc) setChannel(platform, channelID string, open bool) {
	ids := d.Channels[platform]
	i := slices.Index(ids, channelID)
	switch {
	case open && i < 0:
		ids = append(slices.Clone(ids), channelID)
		slices.Sort(ids)
	case !open && i >= 0:
		ids = slices.Delete(slices.Clone(ids), i, i+1)
	default:
		return
	}
	if d.Channels == nil {
		d.Channels = map[string][]string{}
	}
	if len(ids) == 0 {
		delete(d.Channels, platform)
	} else {
		d.Channels[platform] = ids
	}
	if len(d.Channels) == 0 {
		d.Channels = nil
	}
}

// setAuth records a platform's auth failure as announced at `at`, or closed
// when `at` is zero.
func (d *openAlertsDoc) setAuth(platform string, at time.Time) {
	if at.IsZero() {
		delete(d.Auth, platform)
		if len(d.Auth) == 0 {
			d.Auth = nil
		}
		return
	}
	if d.Auth == nil {
		d.Auth = map[string]time.Time{}
	}
	d.Auth[platform] = at.UTC()
}

// dropUnmonitored removes the entries nothing in this configuration can ever
// close, logging each: a channel no longer configured (or disabled — no
// monitor checks it), and the sidecar's when the sidecar is off. Called once
// at boot, after loading, before any alerter is seeded.
func (a *openAlerts) dropUnmonitored(cfg *config.MoomboxConfig) {
	if a == nil || cfg == nil {
		return
	}
	monitored := map[string]map[string]bool{"youtube": {}, "twitch": {}}
	for _, ch := range cfg.Channels {
		if ch.Enabled != nil && !*ch.Enabled {
			continue
		}
		platform := "youtube"
		if ch.Platform == "twitch" {
			platform = "twitch"
		}
		monitored[platform][ch.ID] = true
	}
	a.update(func(d *openAlertsDoc) {
		for platform, ids := range d.Channels {
			kept := ids[:0:0]
			for _, id := range ids {
				if monitored[platform][id] {
					kept = append(kept, id)
					continue
				}
				a.log.Info("open-alert state: dropped a channel alert; the channel is no longer monitored",
					"platform", platform, "channel", id)
			}
			if len(kept) == 0 {
				delete(d.Channels, platform)
			} else {
				d.Channels[platform] = kept
			}
		}
		if len(d.Channels) == 0 {
			d.Channels = nil
		}
		if d.SidecarDown && !cfg.Bgutils.UseSidecar {
			d.SidecarDown = false
			a.log.Info("open-alert state: dropped the sidecar-down alert; the sidecar is turned off")
		}
	})
}
