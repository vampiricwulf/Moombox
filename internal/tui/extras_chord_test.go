package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// extrasWiredApp binds the four callbacks the Extras chords are gated on: each
// of E Y, E L, E I and E T is registered only when its own callback is set
// (buildMenuItems), so a fixture that wired less would silently shorten the
// category and every order assertion below with it.
//
// OnForceCheck rides along for one reason: every Request chord is conditional
// too, and a Request category with no items is dropped from the help overlay
// entirely — the section-order check would then pass without ever seeing R.
func extrasWiredApp() *App {
	app := NewApp()
	app.SetSetupCallbacks(
		func(*config.MoomboxConfig) error { return nil },
		func(int, bool) {},
		func(string) error { return nil },
		func() (cookies.SetupResult, error) { return cookies.SetupResult{}, nil },
		func() {},
		func() {},
	)
	app.OnImportCookieFile = func(string) (cookies.ImportResult, error) { return cookies.ImportResult{}, nil }
	app.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) { return ytdlpplugin.Info{}, nil }
	app.OnGetStats = func() (stats.Snapshot, error) { return stats.Snapshot{}, nil }
	app.OnForceCheck = func() {}
	return app
}

// TestExtrasCategoryHoldsExactlyTheFourChords pins the re-homing itself: the
// four features that used to sit under Request are the whole of the Extras
// category, in the agreed order (yt-dlp, Login, Import, Stats), and no alias
// was left behind under R.
//
// The emission POINT matters as much as the membership: the action menu groups
// by CONSECUTIVE Category (action_menu.go), so an Extras item appended before
// the Open block would split Open into two headed groups. Hence the "after
// every Open item" assertion rather than a bare set comparison.
//
// MUTANTS: leave one item under Request (the Request scan fails); reorder the
// four (the exact-slice compare fails); append them before the Open block (the
// index assertion fails).
func TestExtrasCategoryHoldsExactlyTheFourChords(t *testing.T) {
	items := extrasWiredApp().buildMenuItems()
	if len(items) == 0 {
		t.Fatal("buildMenuItems returned nothing — nothing below can be concluded")
	}

	type entry struct{ chord, label string }
	want := []entry{
		{"E Y", "yt-dlp Plugin"},
		{"E L", "Cookie Login"},
		{"E I", "Import Cookie File"},
		{"E T", "Statistics"},
	}

	var got []entry
	firstExtras, lastOpen := -1, -1
	for i, it := range items {
		switch it.Category {
		case "Extras":
			if firstExtras < 0 {
				firstExtras = i
			}
			got = append(got, entry{it.Chord, it.Label})
		case "Open":
			lastOpen = i
		}
	}

	if len(got) != len(want) {
		t.Fatalf("Extras holds %d chords (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Extras item %d = %v, want %v", i, got[i], want[i])
		}
	}

	if lastOpen < 0 {
		t.Fatal("premise lost: no Open items to order Extras against")
	}
	if firstExtras < lastOpen {
		t.Errorf("Extras starts at index %d, before the last Open item at %d — "+
			"the action menu groups by consecutive Category and would split Open in two",
			firstExtras, lastOpen)
	}

	// No alias stays behind: neither the old chords nor the old category.
	orphaned := map[string]bool{"R Y": true, "R L": true, "R I": true, "R T": true}
	movedLabel := map[string]bool{}
	for _, w := range want {
		movedLabel[w.label] = true
	}
	for _, it := range items {
		if orphaned[strings.TrimSpace(it.Chord)] {
			t.Errorf("chord %q still registered (%q) — the four moved to E", it.Chord, it.Label)
		}
		if it.Category == "Request" && movedLabel[it.Label] {
			t.Errorf("%q (%s) is still filed under Request", it.Label, it.Chord)
		}
	}
}

// TestExtrasPrefixIsRecognisedAndLabelled pins the half of the chord system
// that buildMenuItems does NOT own: the prefix SET is hard-coded in
// handleChord, and the feedback line's category label in chordFeedback. A
// category with menu items and no prefix key is unreachable from the keyboard;
// a prefix with no label falls through to the bare letter.
//
// MUTANT: omit "e" from handleChord's prefix switch → the press reports
// "Invalid Chord: E" instead of arming. Omit the chordFeedback case → the line
// reads "E: ..." instead of "Extras: ...".
func TestExtrasPrefixIsRecognisedAndLabelled(t *testing.T) {
	app := extrasWiredApp()

	app.handleKey(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if app.chord.prefix != "e" {
		t.Fatalf("pressing E armed prefix %q, want %q (feedback: %q)", app.chord.prefix, "e", app.feedback.msg)
	}

	// The feedback derives its options from buildMenuItems by prefix, so the
	// exact line pins the label, the order and the hint words in one read.
	const want = "Extras: Y yt-dlp | L Login | I Import Cookies | T Stats (3s)"
	if app.feedback.msg != want {
		t.Errorf("E feedback = %q, want %q", app.feedback.msg, want)
	}
	if got := app.chordFeedback("e"); got != want {
		t.Errorf("chordFeedback(\"e\") = %q, want %q", got, want)
	}
}

// TestRequestNoLongerAnswersTheExtrasKeys is the negative half of the move: R
// keeps its own chords, and the four second keys that left are now ordinary
// invalid chords rather than silent no-ops or surviving aliases.
func TestRequestNoLongerAnswersTheExtrasKeys(t *testing.T) {
	for _, tc := range []struct {
		key  rune
		want string
	}{
		{'y', "Invalid Chord: R Y"},
		{'l', "Invalid Chord: R L"},
		{'i', "Invalid Chord: R I"},
		{'t', "Invalid Chord: R T"},
	} {
		app := extrasWiredApp()
		app.handleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
		if app.chord.prefix != "r" {
			t.Fatalf("premise lost: R did not arm (%q)", app.feedback.msg)
		}
		app.handleKey(tea.KeyPressMsg{Code: tc.key, Text: string(tc.key)})
		if app.feedback.msg != tc.want {
			t.Errorf("R %c = %q, want %q", tc.key, app.feedback.msg, tc.want)
		}
	}
}

// TestHelpSectionsRunActionRequestOpenExtras pins the derived help order
// against categoryOrder + categoryHelpTitles. A category missing from either
// map is dropped from the overlay entirely (sectionsFromMenu skips it), which
// TestHelpCoversEveryChord would catch — but not the ORDER, which is the part
// the operator reads as the bar's own left-to-right sequence.
func TestHelpSectionsRunActionRequestOpenExtras(t *testing.T) {
	h := NewHelpModel()
	h.SetMenuItems(extrasWiredApp().buildMenuItems())

	var derived []string
	for _, sec := range h.orderedSections() {
		if sec.title == quickKeys.title {
			break // the static sections start here
		}
		derived = append(derived, sec.title)
	}

	want := []string{"Action (A)", "Request (R)", "Open (O)", "Extras (E)"}
	if strings.Join(derived, " / ") != strings.Join(want, " / ") {
		t.Errorf("help sections = %v, want %v", derived, want)
	}
}

// TestChordHintBarNamesExtrasAfterOpen pins the bar's own hard-coded list —
// the fourth copy of the prefix set, and the one the operator sees without
// opening anything. Extras sits where the help and the menu put it: after
// Open, before the single keys. It is checked on both rungs the ladder builds
// from a list (the named labels and the bare glyphs), because those are two
// separate literals in controlTiers.
func TestChordHintBarNamesExtrasAfterOpen(t *testing.T) {
	m := busyStatusBar()
	m.SetWidth(200)
	tiers := m.controlTiers()

	named := stripANSI(tiers[tierFull])
	open, extras, filter := strings.Index(named, "O Open"), strings.Index(named, "E Extras"), strings.Index(named, "F Filter")
	if open < 0 || extras < 0 || filter < 0 {
		t.Fatalf("the wide chord hints are missing a label: %q", named)
	}
	if !(open < extras && extras < filter) {
		t.Errorf("wide hints order %q — want E Extras between O Open and F Filter", named)
	}

	if bare := stripANSI(tiers[tierKeys]); !strings.Contains(bare, "O | E | F") {
		t.Errorf("the bare chord hints do not read \"O | E | F\": %q", bare)
	}
}
