package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

const (
	editHookA = "https://discord.com/api/webhooks/1/aaa"
	editHookB = "https://discord.com/api/webhooks/2/bbb"
	editHookC = "https://discord.com/api/webhooks/3/ccc"
	editHookD = "https://discord.com/api/webhooks/4/ddd"
)

// pageOver is a dashboard page over a store holding parallel downloads 3 and
// targets A, B, C: the fixture, and the target list as the page read it
// from GET /api/config.
func pageOver(t *testing.T) (*configRoutesFixture, []any) {
	t.Helper()
	f := newConfigRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Downloader.NumParallelDownloads = 3
		c.Notifications = []config.NotificationConfig{{URL: editHookA}, {URL: editHookB}, {URL: editHookC}}
	}); err != nil {
		t.Fatal(err)
	}
	return f, pageTargets(t, f)
}

// pageTargets is the target list a page reads from GET /api/config, as JSON
// values.
func pageTargets(t *testing.T, f *configRoutesFixture) []any {
	t.Helper()
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	list, _ := resp["notifications"].([]any)
	return list
}

// edited is a deep copy of a page's list with fn applied to it.
func edited(list []any, fn func(l []any) []any) []any {
	raw, _ := json.Marshal(list)
	var out []any
	_ = json.Unmarshal(raw, &out)
	return fn(out)
}

// editsBody is notification_edits for a page that edited in place (from:
// each entry its own index) and appended past base's end (from -1).
func editsBody(base, now []any) map[string]any {
	from := make([]any, len(now))
	for i := range now {
		if i < len(base) {
			from[i] = i
		} else {
			from[i] = -1
		}
	}
	return map[string]any{"notification_edits": map[string]any{"base": base, "edited": now, "from": from}}
}

func liveTargets(f *configRoutesFixture) []config.NotificationConfig {
	var out []config.NotificationConfig
	f.store.Read(func(c *config.MoomboxConfig) { out = slices.Clone(c.Notifications) })
	return out
}

func targetByURL(t *testing.T, list []config.NotificationConfig, url string) config.NotificationConfig {
	t.Helper()
	for _, n := range list {
		if n.URL == url {
			return n
		}
	}
	t.Fatalf("no target %s in %v", url, list)
	return config.NotificationConfig{}
}

func urlsOf(list []config.NotificationConfig) []string {
	out := make([]string, len(list))
	for i, n := range list {
		out[i] = n.URL
	}
	return out
}

// onDiskConfig reads config.toml back.
func onDiskConfig(t *testing.T, f *configRoutesFixture) *config.MoomboxConfig {
	t.Helper()
	c, err := config.Load(f.store.SavePath())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestDashboardSaveKeepsTUIChanges is the owner decision: a dashboard save
// writes only what the page changed, so a change the TUI saved while the page
// was open survives it. The page sends the one field it edited — the server
// applies each key on its own — and its target edits as notification_edits,
// merged into the live list by identity; it used to send every field and the
// whole target list, putting its load-time copy of each back.
//
// Mutants killed: the merge replaced by `cfgCopy.Notifications =
// edits.edited` (B unmuted, C back, D gone); mergeNotificationEdits merging
// against base instead of the live list (the same); savedResponse without the
// live list (the page's next edit starts from its stale copy).
func TestDashboardSaveKeepsTUIChanges(t *testing.T) {
	f, page := pageOver(t)
	muted := false
	if err := f.store.Update(func(c *config.MoomboxConfig) { // the TUI's save
		c.Downloader.NumParallelDownloads = 7
		c.Notifications = []config.NotificationConfig{
			{URL: editHookA}, {URL: editHookB, Enabled: &muted}, {URL: editHookD},
		}
	}); err != nil {
		t.Fatal(err)
	}

	putConfig(t, f, map[string]any{"logs": map[string]any{"log_level": "DEBUG"}})
	rec := putConfigExpect(t, f, editsBody(page, edited(page, func(l []any) []any {
		l[0].(map[string]any)["mention"] = "@here"
		return l
	})))
	if rec.Code != http.StatusOK {
		t.Fatalf("target edit: %d %s", rec.Code, rec.Body.String())
	}

	for name, c := range map[string]*config.MoomboxConfig{"live": f.store.Snapshot(), "config.toml": onDiskConfig(t, f)} {
		if c.Downloader.NumParallelDownloads != 7 || c.Logs.LogLevel != "DEBUG" {
			t.Errorf("%s: parallel %d, log level %q; want the TUI's 7 and the page's DEBUG",
				name, c.Downloader.NumParallelDownloads, c.Logs.LogLevel)
		}
		if got := urlsOf(c.Notifications); !slices.Equal(got, []string{editHookA, editHookB, editHookD}) {
			t.Errorf("%s: targets %q, want A, B, D — the TUI's removal of C and addition of D kept", name, got)
		}
		if a := targetByURL(t, c.Notifications, editHookA); a.Mention != "@here" {
			t.Errorf("%s: A's mention = %q, want the page's @here", name, a.Mention)
		}
		if b := targetByURL(t, c.Notifications, editHookB); b.IsEnabled() {
			t.Errorf("%s: B was unmuted by a page that never touched it", name)
		}
	}

	var resp struct {
		Notifications []config.NotificationConfig `json:"notifications"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := urlsOf(resp.Notifications); !slices.Equal(got, []string{editHookA, editHookB, editHookD}) {
		t.Errorf("the answer's targets %q, want the live list, for the page's next edit", got)
	}
}

// TestDashboardSaveLastWriterWinsOnTheSameField: the merge spares only what
// the page did not touch. A field both changed — a scalar, a target's
// mention — takes the page's value, its save being the later write.
//
// Mutant killed: notifications' mergeTargetFields keeping live's value for a
// field both changed.
func TestDashboardSaveLastWriterWinsOnTheSameField(t *testing.T) {
	f, page := pageOver(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Downloader.NumParallelDownloads = 7
		ns := slices.Clone(c.Notifications)
		ns[0].Mention = "@everyone"
		c.Notifications = ns
	}); err != nil {
		t.Fatal(err)
	}
	putConfig(t, f, map[string]any{"downloader": map[string]any{"num_parallel_downloads": 5}})
	putConfig(t, f, editsBody(page, edited(page, func(l []any) []any {
		l[0].(map[string]any)["mention"] = "@here"
		return l
	})))
	live := f.store.Snapshot()
	if live.Downloader.NumParallelDownloads != 5 {
		t.Errorf("parallel = %d, want the page's 5", live.Downloader.NumParallelDownloads)
	}
	if got := targetByURL(t, live.Notifications, editHookA).Mention; got != "@here" {
		t.Errorf("A's mention = %q, want the page's @here", got)
	}
}

// TestDashboardTargetAddDeleteMergeByIdentity: a page's add and delete land
// on the live list, not the page's copy: deleting a target the TUI muted
// meanwhile removes it, an added one is appended after the target the TUI
// added, and a delete of one already gone changes nothing — nothing is
// written and the manager is not reloaded, which would flush every target's
// open batch window.
//
// Mutants killed: from ignored, every edited entry read as an in-place edit
// of its own index (the delete rewrites B with C's fields); the reload fired
// on the presence of notification_edits rather than on a changed list (the
// last save reloads the manager).
func TestDashboardTargetAddDeleteMergeByIdentity(t *testing.T) {
	f, page := pageOver(t)
	muted := false
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Notifications = []config.NotificationConfig{
			{URL: editHookA}, {URL: editHookB, Enabled: &muted}, {URL: editHookC, Mention: "@here"}, {URL: editHookD},
		}
	}); err != nil {
		t.Fatal(err)
	}
	// Delete B: from skips base[1].
	putConfig(t, f, map[string]any{"notification_edits": map[string]any{
		"base": page, "edited": []any{page[0], page[2]}, "from": []any{0, 2},
	}})
	live := liveTargets(f)
	if got := urlsOf(live); !slices.Equal(got, []string{editHookA, editHookC, editHookD}) {
		t.Fatalf("after the delete: %q, want A, C, D", got)
	}
	if targetByURL(t, live, editHookC).Mention != "@here" {
		t.Error("the delete rewrote C")
	}

	// From the same stale page: B deleted again, and E added.
	const editHookE = "https://discord.com/api/webhooks/5/eee"
	putConfig(t, f, map[string]any{"notification_edits": map[string]any{
		"base": page, "edited": []any{page[0], page[2], map[string]any{"url": editHookE}}, "from": []any{0, 2, -1},
	}})
	if got := urlsOf(liveTargets(f)); !slices.Equal(got, []string{editHookA, editHookC, editHookD, editHookE}) {
		t.Fatalf("after the add: %q, want A, C, D, E", got)
	}

	// The delete alone again: B is gone already.
	f.notifs.Store(false)
	rec := putConfigExpect(t, f, map[string]any{"notification_edits": map[string]any{
		"base": page, "edited": []any{page[0], page[2]}, "from": []any{0, 2},
	}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Errorf("a delete already made: %d %s, want 200 changed:false", rec.Code, rec.Body.String())
	}
	if f.notifs.Load() {
		t.Error("a save that changed no target reloaded the notification manager")
	}
	// And beside a change of something else, which is written.
	putConfig(t, f, map[string]any{
		"logs":               map[string]any{"log_level": "DEBUG"},
		"notification_edits": map[string]any{"base": page, "edited": []any{page[0], page[2]}, "from": []any{0, 2}},
	})
	if f.notifs.Load() {
		t.Error("a save whose target edits changed nothing reloaded the notification manager")
	}
}

// TestNotificationEditsLeaveUntouchedFieldsAsStored: base and edited are
// normalised alike, so a field the page did not touch compares equal to its
// own base copy and keeps its stored value — here an event filter naming an
// event outside the vocabulary, which normalising only the edited copy
// stripped from the live target behind a mute.
//
// Mutant killed: decodeNotificationEdits normalising edited and not base.
func TestNotificationEditsLeaveUntouchedFieldsAsStored(t *testing.T) {
	f := newConfigRoutesFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Notifications = []config.NotificationConfig{{URL: editHookA, Events: []string{"finished", "not_an_event"}}}
	}); err != nil {
		t.Fatal(err)
	}
	page := pageTargets(t, f)
	putConfig(t, f, editsBody(page, edited(page, func(l []any) []any {
		l[0].(map[string]any)["enabled"] = false
		return l
	})))
	a := targetByURL(t, liveTargets(f), editHookA)
	if a.IsEnabled() {
		t.Error("the mute was not saved")
	}
	if got := strings.Join(a.Events, ","); got != "finished,not_an_event" {
		t.Errorf("events = %q, want the stored filter untouched by a mute", got)
	}
}

// TestClearFilterOnAnEveryEventFilter: the dashboard's "Clear filter" on a
// target whose stored filter names every vocabulary event — what its old
// Filter... chip stored — sends the target with no filter, and the save
// clears it. The merge used to read the two as one value, answer
// changed:false and leave the filter, chips and all.
//
// Mutant killed: notifications' targetFormEvents reading a filter that names
// every vocabulary event as no filter.
func TestClearFilterOnAnEveryEventFilter(t *testing.T) {
	f := newConfigRoutesFixture(t)
	var every []string
	for _, g := range notifications.EventGroups {
		every = append(every, g.Events...)
	}
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Notifications = []config.NotificationConfig{{URL: editHookA, Events: every}}
	}); err != nil {
		t.Fatal(err)
	}
	page := pageTargets(t, f)
	rec := putConfigExpect(t, f, editsBody(page, edited(page, func(l []any) []any {
		delete(l[0].(map[string]any), "events")
		return l
	})))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":true`) {
		t.Fatalf("Clear filter: %d %s, want 200 changed:true", rec.Code, rec.Body.String())
	}
	for name, c := range map[string]*config.MoomboxConfig{"live": f.store.Snapshot(), "config.toml": onDiskConfig(t, f)} {
		if got := c.Notifications[0].Events; len(got) != 0 {
			t.Errorf("%s: events = %d names after Clear filter, want none", name, len(got))
		}
	}
	if !f.notifs.Load() {
		t.Error("the cleared filter did not reload the notification manager")
	}
}

// TestPutRefusesAWholeNotificationList: PUT /api/config never applies a
// whole-list notifications — undocumented, and the very shape that reverted a
// target change made elsewhere. A body whose list would change the targets is
// a 400 naming it, and nothing is written.
//
// Mutant killed: dropping the refusal (the list replaces A, B, C).
func TestPutRefusesAWholeNotificationList(t *testing.T) {
	f, _ := pageOver(t)
	rec := putConfigExpect(t, f, map[string]any{
		"logs":          map[string]any{"log_level": "DEBUG"},
		"notifications": []any{map[string]any{"url": editHookD}},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "notification_edits") {
		t.Fatalf("got %d %s, want a 400 pointing at notification_edits", rec.Code, rec.Body.String())
	}
	live := f.store.Snapshot()
	if got := urlsOf(live.Notifications); !slices.Equal(got, []string{editHookA, editHookB, editHookC}) || live.Logs.LogLevel == "DEBUG" {
		t.Errorf("a refused save wrote: targets %q, log level %q", got, live.Logs.LogLevel)
	}
}

// TestPutTakesAnOlderDashboardsSave: a dashboard tab still running the build
// before notification_edits — app.js defers the reload after a self-update
// while the form is dirty, and asks the operator to save first — sends every
// field and, beside them, its target list as last read, enabled and mode
// spelled out the way its cards wrote them. Such a list holds the live
// targets as they stand, so it is dropped and the rest of the save applied:
// refusing the key whole left the operator's edits nowhere to go, the reload
// they were told to make discarding them. A list the TUI has changed since
// the tab read it would put the tab's copy back, so that save is refused,
// and writes nothing.
//
// Mutants killed: refusing every whole list (the first save is a 400);
// dropping every whole list, or comparing a list by its length alone (the
// stale one saves); the matching list left in the body (applyConfigUpdates
// writes it whole: the spelled-out defaults reload the notification manager
// over no change).
func TestPutTakesAnOlderDashboardsSave(t *testing.T) {
	f, page := pageOver(t)
	spelled := edited(page, func(l []any) []any {
		for _, n := range l {
			n.(map[string]any)["enabled"] = true
			n.(map[string]any)["mode"] = "separate"
		}
		return l
	})
	rec := putConfigExpect(t, f, map[string]any{
		"logs":          map[string]any{"log_level": "DEBUG"},
		"downloader":    map[string]any{"num_parallel_downloads": float64(3)},
		"notifications": spelled,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("the older dashboard's save: %d %s, want 200", rec.Code, rec.Body.String())
	}
	live := f.store.Snapshot()
	if live.Logs.LogLevel != "DEBUG" {
		t.Errorf("log level = %q, want the save's DEBUG", live.Logs.LogLevel)
	}
	if got := urlsOf(live.Notifications); !slices.Equal(got, []string{editHookA, editHookB, editHookC}) ||
		live.Notifications[0].Enabled != nil || live.Notifications[0].Mode != "" {
		t.Errorf("targets = %q (A enabled %v, mode %q), want A, B, C as stored", got, live.Notifications[0].Enabled, live.Notifications[0].Mode)
	}
	if f.notifs.Load() {
		t.Error("a list that changed nothing reloaded the notification manager")
	}

	// The TUI mutes B; the tab's list still has it delivering.
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		muted := false
		c.Notifications[1].Enabled = &muted
	}); err != nil {
		t.Fatal(err)
	}
	rec = putConfigExpect(t, f, map[string]any{
		"logs":          map[string]any{"log_level": "WARN"},
		"notifications": page,
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "notification_edits") {
		t.Fatalf("a stale list: %d %s, want a 400 pointing at notification_edits", rec.Code, rec.Body.String())
	}
	live = f.store.Snapshot()
	if live.Logs.LogLevel != "DEBUG" || live.Notifications[1].IsEnabled() {
		t.Errorf("a refused save wrote: log level %q, B delivering %v", live.Logs.LogLevel, live.Notifications[1].IsEnabled())
	}
}

// TestNotificationEditsRefusals: a malformed notification_edits is a 400
// naming what is wrong, and writes nothing. A URL the merge brings in must
// parse; one the live list already carries — a legacy one, hand-edited — is
// kept as it is, so a mute of that target saves.
//
// Mutants killed: notificationEditsFromError's length, range, integer or
// duplicate check dropped; the URL check skipping new URLs, or applied to
// known ones. (Dropping the edited entries' mention, mode and decode checks
// is killed by TestConfigPutRejectsBadMention and
// TestConfigPutRejectsATypeMismatchedEntry, and dropping their normalisation
// by TestConfigPutStripsUnknownEvents, which drive those through
// notification_edits.)
func TestNotificationEditsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edits map[string]any
		key   string
	}{
		{"not an object", map[string]any{"notification_edits": []any{}}, "notification_edits"},
		{"from too short", map[string]any{"notification_edits": map[string]any{
			"base": []any{}, "edited": []any{map[string]any{"url": editHookD}}, "from": []any{},
		}}, "notification_edits.from"},
		{"from past base", map[string]any{"notification_edits": map[string]any{
			"base": []any{}, "edited": []any{map[string]any{"url": editHookD}}, "from": []any{0},
		}}, "notification_edits.from"},
		{"from twice", map[string]any{"notification_edits": map[string]any{
			"base":   []any{map[string]any{"url": editHookA}},
			"edited": []any{map[string]any{"url": editHookA}, map[string]any{"url": editHookA}},
			"from":   []any{0, 0},
		}}, "notification_edits.from"},
		{"from fractional", map[string]any{"notification_edits": map[string]any{
			"base": []any{map[string]any{"url": editHookA}}, "edited": []any{map[string]any{"url": editHookA}}, "from": []any{0.5},
		}}, "notification_edits.from"},
		{"bad mention", addedTargets(map[string]any{"url": editHookD, "mention": "@ops"}), "notifications[0].mention"},
		{"unparseable new URL", addedTargets(map[string]any{"url": "https://example.com/hook"}), "notifications[0].url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := pageOver(t)
			rec := putConfigExpect(t, f, tc.edits)
			var resp struct {
				Details map[string]string `json:"details"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if rec.Code != http.StatusBadRequest || resp.Details[tc.key] == "" {
				t.Fatalf("got %d %s, want a 400 naming %s", rec.Code, rec.Body.String(), tc.key)
			}
			if got := urlsOf(liveTargets(f)); !slices.Equal(got, []string{editHookA, editHookB, editHookC}) {
				t.Errorf("a refused save wrote targets %q", got)
			}
		})
	}

	t.Run("a legacy URL already stored", func(t *testing.T) {
		f := newConfigRoutesFixture(t)
		legacy := "https://example.com/legacy-hook"
		if err := f.store.Update(func(c *config.MoomboxConfig) {
			c.Notifications = []config.NotificationConfig{{URL: legacy}}
		}); err != nil {
			t.Fatal(err)
		}
		page := pageTargets(t, f)
		putConfig(t, f, editsBody(page, edited(page, func(l []any) []any {
			l[0].(map[string]any)["enabled"] = false
			return l
		})))
		if n := targetByURL(t, liveTargets(f), legacy); n.IsEnabled() {
			t.Error("the mute of a stored legacy target was not saved")
		}
	})
}

// TestPutChecksTheMergedConfig: the page sends only the field it edited, so
// a rule pairing it with another is checked against that other's live
// value, and config.Validate runs on the merged config before anything is
// written. A raised warning threshold over the stored critical one, a hard
// sidecar limit under the stored soft one, an emptied output template — each
// used to pass the per-field checks and fail config.Save as a 500 naming no
// field. A browser_path sent alone is checked with the browser_type stored
// beside it.
//
// Mutants killed: mergedValidationView's disk or memory pair not filled
// (validateConfigUpdates passes the half alone — Validate still answers 400,
// but naming no field); its browser_type fill dropped (the path is refused
// as an unknown browser type); the config.Validate step dropped (the
// template case is a 500).
func TestPutChecksTheMergedConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		key  string
	}{
		{"warning over the stored critical", map[string]any{"disk": map[string]any{"disk_warn_percent": 99}}, "disk.disk_critical_percent"},
		{"hard limit under the stored soft one", map[string]any{"memory": map[string]any{"sidecar_hard_limit_mb": 1}}, "memory.sidecar_hard_limit_mb"},
		{"output template emptied", map[string]any{"downloader": map[string]any{"output_template": ""}}, "config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newConfigRoutesFixture(t)
			if err := f.store.Update(func(c *config.MoomboxConfig) {
				c.Memory.SidecarSoftLimitMB = 512
				c.Memory.SidecarHardLimitMB = 1024
			}); err != nil {
				t.Fatal(err)
			}
			before := f.store.Snapshot()
			rec := putConfigExpect(t, f, tc.body)
			var resp struct {
				Details map[string]string `json:"details"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if rec.Code != http.StatusBadRequest || resp.Details[tc.key] == "" {
				t.Fatalf("got %d %s, want a 400 naming %s", rec.Code, rec.Body.String(), tc.key)
			}
			after := f.store.Snapshot()
			if after.Disk != before.Disk || after.Memory != before.Memory || after.Downloader.OutputTemplate != before.Downloader.OutputTemplate {
				t.Error("a refused save changed the live config")
			}
		})
	}

	t.Run("browser_path alone, with the stored browser_type", func(t *testing.T) {
		f := newConfigRoutesFixture(t)
		name := "firefox"
		if runtime.GOOS == "windows" {
			name = "firefox.exe"
		}
		exe := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Update(func(c *config.MoomboxConfig) { c.Cookies.BrowserType = "firefox" }); err != nil {
			t.Fatal(err)
		}
		putConfig(t, f, map[string]any{"cookies": map[string]any{"browser_path": exe}})
		if got := f.store.Snapshot().Cookies.BrowserPath; got != exe {
			t.Errorf("browser_path = %q, want %q", got, exe)
		}
	})
}

// TestNoOpSaveWritesNothing: a save that leaves the live config as it is —
// a field typed to the value the TUI saved first — writes nothing: config.toml
// is not rewritten and nothing is hot-reloaded, the TUI save's rule.
//
// Mutant killed: the DeepEqual arm dropped (the file is rewritten and the
// log-level callback fires).
func TestNoOpSaveWritesNothing(t *testing.T) {
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"logs": map[string]any{"log_level": "DEBUG"}})
	f.logLevel.Store(nil)
	if err := os.WriteFile(f.store.SavePath(), []byte("# marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := putConfigExpect(t, f, map[string]any{"logs": map[string]any{"log_level": "DEBUG"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Fatalf("got %d %s, want 200 changed:false", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(f.store.SavePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "# marker\n" {
		t.Error("config.toml was rewritten by a save that changed nothing")
	}
	if f.logLevel.Load() != nil {
		t.Error("a save that changed nothing hot-reloaded the log level")
	}
}
