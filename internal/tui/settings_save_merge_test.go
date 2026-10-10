package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The webhook targets the tests below configure. Real-shaped Discord URLs,
// because the editor's Enter validates the URL it saves.
const (
	hookA  = "https://discord.com/api/webhooks/1/aaa"
	hookA2 = "https://discord.com/api/webhooks/1/aaa-rotated"
	hookB  = "https://discord.com/api/webhooks/2/bbb"
	hookC  = "https://discord.com/api/webhooks/3/ccc"
	hookD  = "https://discord.com/api/webhooks/4/ddd"
	hookE  = "https://discord.com/api/webhooks/5/eee"
)

// overlayOverStore opens the Settings overlay over a store seeded by seed,
// wired as runTUI wires it: the overlay holds the store's own config, and
// OnSave is OnSaveConfig's config.Save under the store lock. saves counts
// the OnSave calls.
func overlayOverStore(t *testing.T, seed func(*config.MoomboxConfig)) (m *SettingsModel, store *config.Store, saves *int) {
	t.Helper()
	store = config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	if err := store.Update(seed); err != nil {
		t.Fatal(err)
	}
	m = NewSettingsModel()
	m.configStore = store
	saves = new(int)
	m.OnSave = func(c *config.MoomboxConfig) error {
		*saves++
		mu := store.RWMutex()
		mu.Lock()
		defer mu.Unlock()
		return config.Save(c, store.SavePath())
	}
	m.Open(store.Config())
	return m, store, saves
}

// onDashboard changes the config as PUT /api/config does while the overlay
// is open: under the store lock, replacing the slices it changes whole.
func onDashboard(t *testing.T, store *config.Store, fn func(*config.MoomboxConfig)) {
	t.Helper()
	if err := store.Update(fn); err != nil {
		t.Fatal(err)
	}
}

// liveAndDisk returns the store's config and the one config.toml holds.
func liveAndDisk(t *testing.T, store *config.Store) (live, disk *config.MoomboxConfig) {
	t.Helper()
	disk, err := config.Load(store.SavePath())
	if err != nil {
		t.Fatal(err)
	}
	return store.Snapshot(), disk
}

// threeTargets seeds the targets A, B and C.
func threeTargets(c *config.MoomboxConfig) {
	c.Notifications = []config.NotificationConfig{{URL: hookA}, {URL: hookB}, {URL: hookC}}
}

// editTarget opens the notification editor on the target at idx, lets edit
// change the form, and presses Enter — the operator's own keys, so the
// editor's bookkeeping of which target an entry came from is what is tested.
func editTarget(t *testing.T, m *SettingsModel, idx int, edit func()) {
	t.Helper()
	m.switchSection(sectionIndexByName(t, "Integrations"))
	m.notifIndex = idx
	m.handleNotifKey(keyEnter)
	if m.notifMode != "edit" {
		t.Fatalf("Enter on target %d did not open the editor", idx)
	}
	edit()
	m.handleNotifEditKey(keyEnter)
	if m.status == saveError {
		t.Fatalf("the editor refused the target: %s", m.errorMsg)
	}
}

// toggleEvent flips the editor's checkbox for event.
func toggleEvent(m *SettingsModel, event string) {
	m.notifEditFocus = notifEditEventBase + slices.Index(allNotifEvents, event)
	m.handleNotifEditKey(keySpace)
}

// saveOverlay presses Save and requires it to succeed.
func saveOverlay(t *testing.T, m *SettingsModel) {
	t.Helper()
	m.recheckDirty()
	m.saveAndClose()
	if m.status != saveSaved {
		t.Fatalf("save: status %v, error %q", m.status, m.errorMsg)
	}
}

func targetURLs(ns []config.NotificationConfig) []string {
	var urls []string
	for _, n := range ns {
		urls = append(urls, n.URL)
	}
	return urls
}

func findTarget(t *testing.T, ns []config.NotificationConfig, url string) config.NotificationConfig {
	t.Helper()
	for _, n := range ns {
		if n.URL == url {
			return n
		}
	}
	t.Fatalf("no target %s in %q", url, targetURLs(ns))
	return config.NotificationConfig{}
}

// TestTUISaveKeepsDashboardScalarChanges is the owner decision's first
// half: the overlay copied every setting at Open and a save wrote them all
// back, so a setting the dashboard changed while it was open — here the
// parallel-download count and the feed interval — was reverted by a TUI save
// of the log level. Only the field the overlay edited is written now.
//
// Mutants killed: applyValues writing every field (edited replaced by every
// key of m.values) — both dashboard values revert; editedKeys comparing the
// typed values with the live config instead of with Open's copy — the
// second save's field, typed away and back to its Open value, differs from
// the dashboard's newer value and is written over it.
func TestTUISaveKeepsDashboardScalarChanges(t *testing.T) {
	m, store, _ := overlayOverStore(t, func(*config.MoomboxConfig) {})
	onDashboard(t, store, func(c *config.MoomboxConfig) {
		c.Downloader.NumParallelDownloads = 7
		c.Monitors.FeedCheckInterval = config.FlexDuration{Value: 2.5}
	})
	m.values["log_level"] = "DEBUG"
	saveOverlay(t, m)

	live, disk := liveAndDisk(t, store)
	for name, c := range map[string]*config.MoomboxConfig{"live": live, "config.toml": disk} {
		if c.Downloader.NumParallelDownloads != 7 {
			t.Errorf("%s: num_parallel_downloads = %d after a TUI save of the log level, want the dashboard's 7",
				name, c.Downloader.NumParallelDownloads)
		}
		if c.Monitors.FeedCheckInterval.Value != 2.5 {
			t.Errorf("%s: feed_check_interval = %v, want the dashboard's 2.5", name, c.Monitors.FeedCheckInterval.Value)
		}
		if c.Logs.LogLevel != "DEBUG" {
			t.Errorf("%s: log_level = %q, want the TUI's DEBUG", name, c.Logs.LogLevel)
		}
	}

	// A field typed away and back to its Open value is not an edit either.
	m.Open(store.Config())
	onDashboard(t, store, func(c *config.MoomboxConfig) { c.Downloader.NumParallelDownloads = 3 })
	m.values["num_parallel_downloads"] = "99"
	m.values["num_parallel_downloads"] = m.originalValues["num_parallel_downloads"]
	m.values["log_level"] = "WARN"
	saveOverlay(t, m)
	if live, _ := liveAndDisk(t, store); live.Downloader.NumParallelDownloads != 3 {
		t.Errorf("num_parallel_downloads = %d, want the dashboard's 3: a field typed back to its Open value "+
			"was written over it", live.Downloader.NumParallelDownloads)
	}
}

// TestTUISaveLastWriterWinsOnTheSameField: the merge only spares what the
// overlay did not touch. A field both sides changed — a scalar setting, a
// target's mention — takes the TUI's value, the TUI's save being the later
// write.
//
// Mutants killed: writing an edited field only while live still holds its
// Open value (first writer wins) — the scalar keeps 7; mergeNotifFields
// keeping live's value for a field both changed — the mention stays @here.
func TestTUISaveLastWriterWinsOnTheSameField(t *testing.T) {
	m, store, _ := overlayOverStore(t, threeTargets)
	onDashboard(t, store, func(c *config.MoomboxConfig) {
		c.Downloader.NumParallelDownloads = 7
		ns := slices.Clone(c.Notifications)
		ns[1].Mention = "@here"
		c.Notifications = ns
	})
	m.values["num_parallel_downloads"] = "5"
	editTarget(t, m, 1, func() { m.notifEditMention = "@everyone" })
	saveOverlay(t, m)

	live, disk := liveAndDisk(t, store)
	for name, c := range map[string]*config.MoomboxConfig{"live": live, "config.toml": disk} {
		if c.Downloader.NumParallelDownloads != 5 {
			t.Errorf("%s: num_parallel_downloads = %d, want the TUI's 5", name, c.Downloader.NumParallelDownloads)
		}
		if got := findTarget(t, c.Notifications, hookB).Mention; got != "@everyone" {
			t.Errorf("%s: target B's mention = %q, want the TUI's @everyone", name, got)
		}
	}
}

// TestTUISaveKeepsDashboardNotificationChanges is the owner decision's
// second half: Open copied the target list and a save wrote it back whole,
// so a save of anything else dropped a target the dashboard added, undid
// its edits and restored one it removed. The overlay's own changes are
// applied per target now, and per field within one: here the TUI narrows
// A's event filter while the dashboard mutes A, edits B, removes C and adds
// D.
//
// Mutants killed: `m.cfg.Notifications = m.notifications` (D dropped, C
// back, B's and A's dashboard edits undone); mergeNotifFields writing the
// editor's target whole (A un-muted); mergeNotificationEdits treating every
// target it kept as edited (C, which the dashboard removed and the TUI did
// not touch, is added back as an edit of a removed target).
func TestTUISaveKeepsDashboardNotificationChanges(t *testing.T) {
	m, store, _ := overlayOverStore(t, threeTargets)
	off := false
	onDashboard(t, store, func(c *config.MoomboxConfig) {
		ns := slices.Clone(c.Notifications)
		ns[0].Enabled = &off
		ns[1].Enabled = &off
		ns[1].Mention = "@here"
		ns = slices.Delete(ns, 2, 3)
		c.Notifications = append(ns, config.NotificationConfig{URL: hookD})
	})
	editTarget(t, m, 0, func() { toggleEvent(m, allNotifEvents[0]) })
	m.values["log_level"] = "DEBUG"
	saveOverlay(t, m)

	live, disk := liveAndDisk(t, store)
	for name, c := range map[string]*config.MoomboxConfig{"live": live, "config.toml": disk} {
		if got := targetURLs(c.Notifications); !slices.Equal(got, []string{hookA, hookB, hookD}) {
			t.Fatalf("%s: targets %q, want A, B and the dashboard's D (C removed there)", name, got)
		}
		a := findTarget(t, c.Notifications, hookA)
		if a.IsEnabled() {
			t.Errorf("%s: target A is enabled again — the TUI's edit of its events wrote its Open-time mute back", name)
		}
		if len(a.Events) != len(allNotifEvents)-1 || slices.Contains(a.Events, allNotifEvents[0]) {
			t.Errorf("%s: target A's events = %v, want every event but the TUI's unticked %s", name, a.Events, allNotifEvents[0])
		}
		b := findTarget(t, c.Notifications, hookB)
		if b.IsEnabled() || b.Mention != "@here" {
			t.Errorf("%s: target B = enabled %v, mention %q; want the dashboard's mute and @here", name, b.IsEnabled(), b.Mention)
		}
	}
}

// TestTUISaveAppliesItsOwnTargetChangesByIdentity: what the TUI did to the
// targets is applied to the CURRENT live list by each target's identity —
// the URL it had at Open, so a target whose URL the TUI rotated is still
// the target the dashboard muted meanwhile. A target the TUI removed goes
// even though the dashboard edited it, one it added is appended, and the
// dashboard's own addition stays.
//
// Mutants killed: ignoring notifFrom (every entry read as added: the
// targets land out of order, the rotated A un-muted); the delete arm leaving
// notifFrom unshifted (C, after the deleted B, is matched to B's Open copy:
// B's live entry is rewritten into a C carrying B's dashboard mention, and
// the real C is removed).
func TestTUISaveAppliesItsOwnTargetChangesByIdentity(t *testing.T) {
	m, store, _ := overlayOverStore(t, threeTargets)
	off := false
	onDashboard(t, store, func(c *config.MoomboxConfig) {
		ns := slices.Clone(c.Notifications)
		ns[0].Enabled = &off
		ns[1].Mention = "@here"
		c.Notifications = append(ns, config.NotificationConfig{URL: hookD})
	})
	editTarget(t, m, 0, func() { m.notifEditURL = hookA2 })
	// Delete B: D on the list, then D again to confirm.
	m.notifIndex = 1
	m.handleNotifKey("d")
	m.handleNotifKey("d")
	// C is now at index 1; rename nothing, but mute it in the TUI.
	editTarget(t, m, 1, func() {
		m.notifEditFocus = notifEditEnabledRow
		m.handleNotifEditKey(keySpace)
	})
	m.handleNotifKey("a")
	m.notifEditURL = hookE
	m.handleNotifEditKey(keyEnter)
	saveOverlay(t, m)

	live, disk := liveAndDisk(t, store)
	for name, c := range map[string]*config.MoomboxConfig{"live": live, "config.toml": disk} {
		if got := targetURLs(c.Notifications); !slices.Equal(got, []string{hookA2, hookC, hookD, hookE}) {
			t.Fatalf("%s: targets %q, want A (rotated), C, the dashboard's D and the TUI's E", name, got)
		}
		if a := findTarget(t, c.Notifications, hookA2); a.IsEnabled() {
			t.Errorf("%s: the rotated A is enabled — the dashboard's mute of the same target was lost", name)
		}
		if cc := findTarget(t, c.Notifications, hookC); cc.IsEnabled() || cc.Mention != "" {
			t.Errorf("%s: C = enabled %v, mention %q; want the TUI's mute and no mention", name, cc.IsEnabled(), cc.Mention)
		}
	}
}

// TestMergeNotificationEditsIdentityEdges pins the corners of a target's
// identity the overlay can reach.
//
// Mutants killed: an identity without its occurrence count (deleting one of
// two entries with one URL removes both); an added target always appended
// (a webhook the TUI and the dashboard both added is listed twice); an edit
// of a target the dashboard removed dropped instead of added back.
func TestMergeNotificationEditsIdentityEdges(t *testing.T) {
	x := config.NotificationConfig{URL: hookA}
	t.Run("one of two entries with one URL deleted", func(t *testing.T) {
		got, changed := mergeNotificationEdits(
			[]config.NotificationConfig{x, x}, []config.NotificationConfig{x}, []int{0},
			[]config.NotificationConfig{x, x})
		if !changed || len(got) != 1 {
			t.Errorf("merged %q (changed %v), want one entry left", targetURLs(got), changed)
		}
	})
	t.Run("added on both sides", func(t *testing.T) {
		mine := config.NotificationConfig{URL: hookB, Mention: "@here"}
		got, _ := mergeNotificationEdits(
			[]config.NotificationConfig{x}, []config.NotificationConfig{x, mine}, []int{0, -1},
			[]config.NotificationConfig{x, {URL: hookB}})
		if !slices.Equal(targetURLs(got), []string{hookA, hookB}) || got[1].Mention != "@here" {
			t.Errorf("merged %q, want B once, as the TUI saved it", targetURLs(got))
		}
	})
	t.Run("edited here, removed there", func(t *testing.T) {
		muted := false
		edited := config.NotificationConfig{URL: hookA, Enabled: &muted}
		got, _ := mergeNotificationEdits(
			[]config.NotificationConfig{x}, []config.NotificationConfig{edited}, []int{0}, nil)
		if len(got) != 1 || got[0].IsEnabled() {
			t.Errorf("merged %q, want the TUI's muted A added back", targetURLs(got))
		}
	})
	t.Run("nothing changed", func(t *testing.T) {
		live := []config.NotificationConfig{{URL: hookD}}
		got, changed := mergeNotificationEdits(
			[]config.NotificationConfig{x}, []config.NotificationConfig{x}, []int{0}, live)
		if changed || &got[0] != &live[0] {
			t.Errorf("an untouched list was rewritten (changed %v)", changed)
		}
	})
}

// TestTUINoOpSaveWritesNothing: a save whose changes leave the live config
// as it is writes nothing at all — no OnSave, so config.toml keeps exactly
// what the dashboard wrote, and the dashboard's changes stand. Opening a
// target and pressing Enter changes nothing even though the editor spells
// an absent enabled and mode out, and so does a field typed away and back;
// in particular it is not an edit of a target the dashboard removed, which
// would add that target back.
//
// Mutants killed: saveAndClose calling OnSave whatever the write changed
// (the file is rewritten, saves = 1); mergeNotificationEdits comparing the
// stored structs instead of notifFormValues (the Enter on C reads as an
// edit of a removed target, and C is added back).
func TestTUINoOpSaveWritesNothing(t *testing.T) {
	m, store, saves := overlayOverStore(t, threeTargets)
	onDashboard(t, store, func(c *config.MoomboxConfig) {
		c.Downloader.NumParallelDownloads = 7
		c.Notifications = append(slices.Delete(slices.Clone(c.Notifications), 2, 3),
			config.NotificationConfig{URL: hookD})
	})
	before, err := os.ReadFile(store.SavePath())
	if err != nil {
		t.Fatal(err)
	}
	editTarget(t, m, 0, func() {})
	editTarget(t, m, 2, func() {})
	m.values["log_level"] = "DEBUG"
	m.recheckDirty()
	m.values["log_level"] = m.originalValues["log_level"]
	if !m.structDirty {
		t.Fatal("the no-change Enter did not mark the form dirty — the save below would not run")
	}
	saveOverlay(t, m)

	if *saves != 0 {
		t.Errorf("OnSave called %d times for a save that changed nothing", *saves)
	}
	after, err := os.ReadFile(store.SavePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("config.toml was rewritten by a save that changed nothing:\n%s", after)
	}
	live, _ := liveAndDisk(t, store)
	if got := targetURLs(live.Notifications); live.Downloader.NumParallelDownloads != 7 ||
		!slices.Equal(got, []string{hookA, hookB, hookD}) {
		t.Errorf("live: num_parallel_downloads %d, targets %q; want the dashboard's 7 and A, B, D",
			live.Downloader.NumParallelDownloads, got)
	}
	if findTarget(t, live.Notifications, hookA).Enabled != nil {
		t.Error("target A's enabled was spelled out by an Enter that changed nothing")
	}
	if m.IsVisible() {
		t.Error("the overlay stayed open after a save with nothing to write")
	}
}

// TestTUISaveWritesTargetEditsTheFormUsedToHide: whether a target was
// edited is decided by notifFormValues, which hid two differences the
// manager acts on. A filter naming only events outside the vocabulary — a
// retired connectivity_pause, still delivered through its alias, or
// connectivity_lost, which matches nothing — read as "*", every event, so
// ticking every row to make the target receive every event was no edit: the
// overlay said Saved, OnSave never ran, and the live config and config.toml
// both kept the old filter. And the URL was compared trimmed, so deleting
// the stray space the manager refuses a padded URL for was no edit either.
//
// Mutants killed: notifFormEvents reading a filter with no vocabulary event
// as "*" (the old known-events-only form) — the retired filters stay;
// "url" compared trimmed — the padded URL stays; Space on an event row not
// marking the rows touched — Enter keeps the stored filter.
func TestTUISaveWritesTargetEditsTheFormUsedToHide(t *testing.T) {
	for _, retired := range []string{"connectivity_pause", "connectivity_lost"} {
		t.Run(retired, func(t *testing.T) {
			m, store, saves := overlayOverStore(t, func(c *config.MoomboxConfig) {
				c.Notifications = []config.NotificationConfig{{URL: hookA, Events: []string{retired}}}
			})
			editTarget(t, m, 0, func() {
				for _, e := range allNotifEvents {
					if !m.notifEditEvents[e] {
						toggleEvent(m, e)
					}
				}
			})
			saveOverlay(t, m)
			if *saves != 1 {
				t.Errorf("OnSave called %d times, want the edit written once", *saves)
			}
			live, disk := liveAndDisk(t, store)
			for name, c := range map[string]*config.MoomboxConfig{"live": live, "config.toml": disk} {
				if got := c.Notifications[0].Events; len(got) != 0 {
					t.Errorf("%s: events = %v after the TUI saved every event, want none (every event)", name, got)
				}
			}
		})
	}
	t.Run("padded URL", func(t *testing.T) {
		m, store, saves := overlayOverStore(t, func(c *config.MoomboxConfig) {
			c.Notifications = []config.NotificationConfig{{URL: " " + hookA}}
		})
		editTarget(t, m, 0, func() { m.notifEditURL = hookA })
		saveOverlay(t, m)
		if *saves != 1 {
			t.Errorf("OnSave called %d times, want the edit written once", *saves)
		}
		live, disk := liveAndDisk(t, store)
		for name, c := range map[string]*config.MoomboxConfig{"live": live, "config.toml": disk} {
			if got := c.Notifications[0].URL; got != hookA {
				t.Errorf("%s: URL = %q after the TUI deleted its stray space, want %q", name, got, hookA)
			}
		}
	})
}

// TestNotifEditorKeepsAFilterItsRowsCannotShow: the editor's rows show only
// the vocabulary, so a filter naming a retired key opens with no row
// ticked. Now that the save tells such a filter from "every event", an
// Enter that rebuilt the filter from those rows would turn a look at the
// target into an edit to every event. The filter is rebuilt only once an
// event row was toggled in that editing session — by Space or by a click —
// and a toggle in an earlier session left with Esc does not count.
//
// Mutants killed: Enter rebuilding the filter whether or not a row was
// toggled; opening a target without clearing the earlier session's toggle;
// a click on an event row not marking the rows touched. Clearing it when
// "a" opens an added target is not pinned: "a" ticks every row, and the
// rebuilt filter is then every event, the same as an untouched one.
func TestNotifEditorKeepsAFilterItsRowsCannotShow(t *testing.T) {
	retired := []string{"connectivity_pause"}
	m, store, saves := overlayOverStore(t, func(c *config.MoomboxConfig) {
		c.Notifications = []config.NotificationConfig{{URL: hookA, Events: retired}}
	})
	// An abandoned session: a row toggled, then Esc.
	m.switchSection(sectionIndexByName(t, "Integrations"))
	m.notifIndex = 0
	m.handleNotifKey(keyEnter)
	toggleEvent(m, allNotifEvents[0])
	m.handleNotifEditKey(keyEsc)

	editTarget(t, m, 0, func() {})
	if got := m.notifications[0].Events; !slices.Equal(got, retired) {
		t.Fatalf("an Enter that toggled no row left the entry's events at %v, want the stored %v", got, retired)
	}
	saveOverlay(t, m)
	if *saves != 0 {
		t.Errorf("OnSave called %d times for a save that changed nothing", *saves)
	}
	if got := store.Snapshot().Notifications[0].Events; !slices.Equal(got, retired) {
		t.Errorf("live events = %v, want the stored %v", got, retired)
	}

	// A click on the first event row is a toggle as Space is: line 0 of the
	// event area is the group's blank line, 1 its header, 2 its first event.
	m.Open(store.Config())
	editTarget(t, m, 0, func() { m.clickNotifEvent(2) })
	if got, want := m.notifications[0].Events, []string{notifEventGroups[0].events[0]}; !slices.Equal(got, want) {
		t.Errorf("a click that ticked one row left the entry's events at %v, want %v", got, want)
	}
}

// TestTUISaveValidatesTheMergedResult: the checks run on what will be
// live after the save — the TUI's edits over the live config as it stands
// — not on the overlay's Open-time copy. A warning threshold the TUI raised
// to 92 is fine against the critical 95 it opened with, but the dashboard
// lowered critical to 91 meanwhile, and 92 over 91 is the pair config.Save
// refuses.
//
// Mutant killed: validateSettingsValues run over m.values (the typed edits
// over the Open values) — the check passes, the write lands and config.Save
// refuses it as a raw "Save failed" after OnSave has run.
func TestTUISaveValidatesTheMergedResult(t *testing.T) {
	m, store, saves := overlayOverStore(t, func(*config.MoomboxConfig) {})
	onDashboard(t, store, func(c *config.MoomboxConfig) { c.Disk.CriticalPercent = 91 })
	m.values["disk_warn_percent"] = "92"
	m.recheckDirty()
	m.saveAndClose()

	if m.status != saveError || !strings.Contains(m.errorMsg, "Disk critical threshold must exceed warning threshold") {
		t.Fatalf("status %v, error %q; want the disk pair refused on the merged values", m.status, m.errorMsg)
	}
	if *saves != 0 {
		t.Errorf("OnSave called %d times for a refused save", *saves)
	}
	if live, _ := liveAndDisk(t, store); live.Disk.WarnPercent != 90 || live.Disk.CriticalPercent != 91 {
		t.Errorf("live disk thresholds %d/%d after a refused save, want 90/91", live.Disk.WarnPercent, live.Disk.CriticalPercent)
	}
}

// TestTUISaveRestartReflectsWhatChanged: the restart prompt follows the
// restart-required fields the operator edited, each compared — as the live
// config holds it after the write — with the value Open showed. The process
// cannot have restarted since Open (a restart closes the overlay), so a
// typed port the dashboard had already saved still leaves config.toml on a
// port the listener is not on: the write changes nothing (and writes
// nothing), yet it prompts. A save of the log level while the dashboard
// changed the port prompts nothing — that restart is the dashboard's to ask
// for — and neither does a field typed away and back. A blank probe_targets
// keeps the stored list, so it prompts only when that list is not the one
// Open showed.
//
// Mutants killed: the restart decided from the live config before and after
// the write (the first case is silent); every restart-required key compared
// with its Open value, edited or not (the dashboard's port prompts the log
// level save); the typed value compared with the Open one, the old
// hasRestartChanges (the blank probe_targets that kept Open's list prompts).
func TestTUISaveRestartReflectsWhatChanged(t *testing.T) {
	// save opens the overlay, lets dashboard change the live config, types
	// typed over the overlay's values, saves, and reports whether a restart
	// was asked for — the question and the banner must agree — and how many
	// times OnSave ran.
	save := func(t *testing.T, dashboard func(*config.MoomboxConfig), typed map[string]string) (restart bool, saves int, store *config.Store) {
		t.Helper()
		m, store, n := overlayOverStore(t, func(*config.MoomboxConfig) {})
		if dashboard != nil {
			onDashboard(t, store, dashboard)
		}
		prompted := 0
		m.OnRestartRequired = func() { prompted++ }
		for k, v := range typed {
			m.values[k] = v
		}
		saveOverlay(t, m)
		if m.showRestartOverlay != (prompted == 1) {
			t.Fatalf("restart question shown %v, OnRestartRequired called %d times", m.showRestartOverlay, prompted)
		}
		return m.showRestartOverlay, *n, store
	}
	t.Run("typed to the dashboard's value", func(t *testing.T) {
		restart, saves, store := save(t, func(c *config.MoomboxConfig) { c.Network.Port = 9090 },
			map[string]string{"port": "9090"})
		if !restart {
			t.Error("a port typed away from the one Open showed prompted no restart: the listener is still on the boot port")
		}
		if saves != 0 {
			t.Errorf("OnSave called %d times for a save that changed nothing in the live config", saves)
		}
		if live, _ := liveAndDisk(t, store); live.Network.Port != 9090 {
			t.Errorf("port = %d, want 9090", live.Network.Port)
		}
	})
	t.Run("another field saved", func(t *testing.T) {
		restart, _, store := save(t, func(c *config.MoomboxConfig) { c.Network.Port = 9090 },
			map[string]string{"log_level": "DEBUG"})
		if restart {
			t.Error("a save of the log level prompted a restart for the dashboard's port")
		}
		if live, _ := liveAndDisk(t, store); live.Network.Port != 9090 {
			t.Errorf("port = %d, want the dashboard's 9090", live.Network.Port)
		}
	})
	t.Run("port moved", func(t *testing.T) {
		restart, _, store := save(t, nil, map[string]string{"port": "9091"})
		if !restart {
			t.Error("a port change did not prompt a restart")
		}
		if live, _ := liveAndDisk(t, store); live.Network.Port != 9091 {
			t.Errorf("port = %d, want 9091", live.Network.Port)
		}
	})
	t.Run("probe targets blanked", func(t *testing.T) {
		if restart, _, _ := save(t, nil, map[string]string{"probe_targets": "", "log_level": "DEBUG"}); restart {
			t.Error("a blank probe_targets, which keeps the list Open showed, prompted a restart")
		}
	})
	t.Run("probe targets blanked over the dashboard's list", func(t *testing.T) {
		restart, _, store := save(t,
			func(c *config.MoomboxConfig) { c.Connectivity.ProbeTargets = []string{"1.0.0.1:443"} },
			map[string]string{"probe_targets": ""})
		if !restart {
			t.Error("a blank probe_targets kept a list other than the one Open showed and prompted nothing")
		}
		live, disk := liveAndDisk(t, store)
		for name, c := range map[string]*config.MoomboxConfig{"live": live, "config.toml": disk} {
			if !slices.Equal(c.Connectivity.ProbeTargets, []string{"1.0.0.1:443"}) {
				t.Errorf("%s: probe_targets = %v, want the stored list kept", name, c.Connectivity.ProbeTargets)
			}
		}
	})
}

// TestEverySettingsFieldIsWritable pins writeSettingsField to the fields the
// overlay shows: a field loadSettingsValues loads but the write does not
// know would be edited, saved as "Saved", and silently never written.
//
// Mutant killed: deleting any one case from writeSettingsField (or from
// writeNotifField, for the editor's target fields).
func TestEverySettingsFieldIsWritable(t *testing.T) {
	v := map[string]string{}
	loadSettingsValues(v, config.Defaults())
	for key := range v {
		c := config.Defaults()
		if !writeSettingsField(c, key, v, settingsParsed{flex: map[string]float64{}}) {
			t.Errorf("the overlay shows %q but a save cannot write it", key)
		}
	}
	if writeSettingsField(config.Defaults(), "no_such_field", v, settingsParsed{}) {
		t.Error("writeSettingsField accepted a field it does not know")
	}
	for key := range notifFormValues(config.NotificationConfig{}) {
		if !writeNotifField(&config.NotificationConfig{}, config.NotificationConfig{}, key) {
			t.Errorf("the notification editor shows %q but a save cannot write it", key)
		}
	}
}
