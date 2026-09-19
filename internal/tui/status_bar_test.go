package tui

import (
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// sgrPattern matches the SGR escape sequences lipgloss emits. Assertions
// below run on stripped text: a styled chord renders as
// "\x1b[..mA\x1b[m Action", so "A Action" is not a literal substring of the
// raw output even though that is exactly what the terminal shows.
var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return sgrPattern.ReplaceAllString(s, "") }

// busyStatusBar is a worst-case bar: every optional indicator populated,
// including a long backfill channel name. Narrow-width behavior is only
// interesting when there is more content than room.
func busyStatusBar() *StatusBarModel {
	m := NewStatusBarModel()
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	m.SetActivePlatforms(true, true)
	m.SetDiskStatus(120*1024*1024*1024, 45, "ok")
	m.SetBackfillStatus("UC123", "A Very Long Channel Name", "videos", 3, "scanning")
	m.SelectedCount = 12
	m.SetJobs([]*database.Job{
		{Status: database.StatusDownloading},
		{Status: database.StatusLive},
	})
	return m
}

// TestStatusBarNeverExceedsWidth pins the overflow guard. The pre-tier bar
// dropped the whole left half on overflow but left the right half
// unbounded, so a busy bar in a narrow window rendered WIDER than the
// terminal — which wraps, pushing a second line into the frame and
// corrupting every row below it. Clipping is the acceptable failure;
// wrapping is not.
func TestStatusBarNeverExceedsWidth(t *testing.T) {
	m := busyStatusBar()
	for w := 1; w <= 200; w++ {
		m.SetWidth(w)
		out := m.View()
		if got := lipgloss.Width(out); got > w {
			t.Fatalf("width %d: rendered %d columns (would wrap): %q", w, got, out)
		}
		if strings.Contains(out, "\n") {
			t.Fatalf("width %d: status bar must be a single line, got %q", w, out)
		}
	}
}

// TestStatusBarKeepsKeybindsWhenNarrow is the regression test for the
// reported bug: the old rule blanked the entire chord-hint half the moment
// both sides didn't fit, so ordinary narrow terminals showed no keybinds at
// all. Some chord must survive at every width that can seat one.
func TestStatusBarKeepsKeybindsWhenNarrow(t *testing.T) {
	m := busyStatusBar()
	for w := 12; w <= 200; w++ {
		m.SetWidth(w)
		out := m.View()
		if !strings.Contains(stripANSI(out), "?") {
			t.Errorf("width %d: no chord hint survived: %q", w, out)
		}
	}
}

// TestStatusBarShowsFullLabelsWhenWide: a wide terminal must still get the
// verbose rendering — the ladder is for scarcity, not a blanket downgrade.
func TestStatusBarShowsFullLabelsWhenWide(t *testing.T) {
	m := busyStatusBar()
	m.SetWidth(200)
	out := m.View()
	for _, want := range []string{"A Action", "Tab Focus", "` Settings", "? Help", "Disk 45%", "Active: 2"} {
		if !strings.Contains(stripANSI(out), want) {
			t.Errorf("wide bar missing %q: %q", want, out)
		}
	}
}

// TestStatusBarTiersNarrowMonotonically: each rung of both ladders must be
// no wider than the rung above it, or fitTiers' descent could step onto a
// WIDER rendering and loop or overflow.
func TestStatusBarTiersNarrowMonotonically(t *testing.T) {
	m := busyStatusBar()
	m.SetWidth(200)

	for _, tc := range []struct {
		name  string
		tiers []string
	}{
		{"controls", m.controlTiers()},
		{"metrics", m.metricTiers()},
	} {
		if len(tc.tiers) != int(tierNone)+1 {
			t.Fatalf("%s: %d tiers, want %d (one per barTier)", tc.name, len(tc.tiers), tierNone+1)
		}
		for i := 1; i < len(tc.tiers); i++ {
			prev, cur := lipgloss.Width(tc.tiers[i-1]), lipgloss.Width(tc.tiers[i])
			if cur > prev {
				t.Errorf("%s tier %d (%d cols) is wider than tier %d (%d cols)", tc.name, i, cur, i-1, prev)
			}
		}
		if got := lipgloss.Width(tc.tiers[tierNone]); got != 0 {
			t.Errorf("%s: tierNone must be empty, got %d cols", tc.name, got)
		}
	}
}

// TestStatusBarDescentSchedule pins the agreed degradation order, which is
// a deliberate product decision rather than an emergent property of the
// fitting loop: the right half steps down ALONE to tierKeys, the left then
// follows to tierKeys, and from there the two alternate — right first —
// down to tierNone. Every step must lower exactly one side by exactly one
// rung, or the "first fit is the richest fit" property that lets fitTiers
// be a plain scan no longer holds.
func TestStatusBarDescentSchedule(t *testing.T) {
	want := []struct{ left, right barTier }{
		{tierFull, tierFull},
		{tierFull, tierCompact},
		{tierFull, tierKeys},
		{tierCompact, tierKeys},
		{tierKeys, tierKeys},
		{tierKeys, tierTight},
		{tierTight, tierTight},
		{tierTight, tierEssential},
		{tierEssential, tierEssential},
		{tierEssential, tierNone},
		{tierNone, tierNone},
	}
	if len(statusBarDescent) != len(want) {
		t.Fatalf("descent has %d steps, want %d", len(statusBarDescent), len(want))
	}
	for i, w := range want {
		if statusBarDescent[i] != w {
			t.Errorf("step %d = (%v,%v), want (%v,%v)",
				i, statusBarDescent[i].left, statusBarDescent[i].right, w.left, w.right)
		}
	}

	// Exactly one side moves down exactly one rung per step, never up.
	for i := 1; i < len(statusBarDescent); i++ {
		prev, cur := statusBarDescent[i-1], statusBarDescent[i]
		dl, dr := cur.left-prev.left, cur.right-prev.right
		if dl < 0 || dr < 0 {
			t.Errorf("step %d moves a side UP: (%v,%v) -> (%v,%v)", i, prev.left, prev.right, cur.left, cur.right)
		}
		if dl+dr != 1 {
			t.Errorf("step %d changes %d rungs, want exactly 1", i, dl+dr)
		}
	}

	// The right must reach tierKeys before the left leaves tierFull — the
	// "status verbosity is the cheapest thing to lose" rule.
	for _, step := range statusBarDescent {
		if step.left > tierFull && step.right < tierKeys {
			t.Errorf("left degraded to %v while right was still at %v (richer than tierKeys)", step.left, step.right)
		}
	}
}

// TestStatusBarAlertsOutliveCounters pins the priority rule: when space is
// scarce the informational indicators (backfill scan, selection count,
// active tally) go first and the alerts (OFFLINE, re-login) stay. An
// operator squeezed for columns needs to know something is WRONG far more
// than they need a healthy tally.
func TestStatusBarAlertsOutliveCounters(t *testing.T) {
	m := busyStatusBar()
	m.offline = true
	m.SetCookieStatus(CookieStatusRelogin, CookieStatusOK)

	// A width that cannot seat the full bar but is far from degenerate.
	m.SetWidth(46)
	out := m.View()

	if !strings.Contains(stripANSI(out), "OFF") {
		t.Errorf("offline alert dropped at width 46: %q", out)
	}
	if !strings.Contains(stripANSI(out), "YT") {
		t.Errorf("re-login alert dropped at width 46: %q", out)
	}
	if sa := stripANSI(out); strings.Contains(sa, "Backfill") || strings.Contains(sa, "BF:") {
		t.Errorf("informational backfill survived past the alerts at width 46: %q", out)
	}
}

// TestStatusBarHealthyAuthYieldsToAlerts: a green "YT"/"TW" is reassurance,
// so it is dropped at tierEssential — but a re-login prompt at the same tier
// is not.
func TestStatusBarHealthyAuthYieldsToAlerts(t *testing.T) {
	healthy := NewStatusBarModel()
	healthy.SetActivePlatforms(true, true)
	healthy.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	if got := healthy.renderCookieStatus(tierEssential, healthy.tallyJobs()); got != "" {
		t.Errorf("healthy auth at tierEssential = %q, want dropped", got)
	}

	alerting := NewStatusBarModel()
	alerting.SetActivePlatforms(true, true)
	alerting.SetCookieStatus(CookieStatusRelogin, CookieStatusOK)
	if got := alerting.renderCookieStatus(tierEssential, alerting.tallyJobs()); !strings.Contains(stripANSI(got), "YT") {
		t.Errorf("re-login at tierEssential = %q, want it to survive", got)
	}
}

// TestStatusBarChordHintDegrades: the newcomer hint takes the same ladder
// rather than vanishing at the first squeeze.
func TestStatusBarChordHintDegrades(t *testing.T) {
	m := busyStatusBar()
	m.ShowChordHint = true
	for w := 12; w <= 120; w++ {
		m.SetWidth(w)
		if out := m.View(); !strings.Contains(stripANSI(out), "?") {
			t.Errorf("width %d: chord hint fully vanished: %q", w, out)
		}
	}
}

// TestStatusBarChordHintNeverOverflows pins the newcomer-hint half to the
// bar's own bound: controlTiers' ShowChordHint branch is already narrower at
// every rung than the named-chord branch it replaces (a fixed-string ladder,
// not a width computation), and View's trailing MaxWidth clamp truncates the
// single-line bar rather than letting it wrap — so this combination (busy
// metrics, hint shown, a width the pre-tier bar used to overflow at) can
// never render wider than the terminal or spill onto a second line. Pinned
// directly rather than only through the general sweep in
// TestStatusBarNeverExceedsWidth, whose fixture never sets ShowChordHint.
func TestStatusBarChordHintNeverOverflows(t *testing.T) {
	m := busyStatusBar()
	m.ShowChordHint = true
	m.SetWidth(60)
	out := m.View()
	if got := lipgloss.Width(out); got > 60 {
		t.Errorf("width 60: rendered %d columns (would wrap): %q", got, out)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("width 60: chord hint bar must stay a single line, got %q", out)
	}
}

// TestStatusBarZeroWidth: an unsized bar renders nothing rather than
// panicking on a negative pad.
func TestStatusBarZeroWidth(t *testing.T) {
	m := busyStatusBar()
	for _, w := range []int{0, -1, -80} {
		m.SetWidth(w)
		if out := m.View(); out != "" {
			t.Errorf("width %d: expected empty bar, got %q", w, out)
		}
	}
}

// TestInactiveYouTubeReloginNamesNoChord pins ReloginPlatform's ytActive gate
// (status_bar.go:131) directly, beside the badge where the difference is
// visible: TestCookieLoginChordPreselectsThePlatformTheBadgeIsAlarmingAbout
// cannot see this mutant because an inactive-but-flagged YouTube yields
// cookieFocus 0 whether ReloginPlatform says "" or "youtube" — but a bar that
// shows no YouTube alert must not carry (R L) regardless. The mirror row does
// the same check for an inactive-but-flagged Twitch.
//
// MUTANT: drop `m.ytActive &&` from ReloginPlatform (status_bar.go:131). Both
// assertions in the YouTube row fail.
func TestInactiveYouTubeReloginNamesNoChord(t *testing.T) {
	m := NewStatusBarModel()
	m.SetActivePlatforms(false, true)
	m.SetCookieStatus(CookieStatusRelogin, CookieStatusOK)
	if got := m.ReloginPlatform(); got != "" {
		t.Errorf("ReloginPlatform() = %q for an inactive platform, want \"\"", got)
	}
	if full := stripANSI(m.renderCookieStatus(tierFull, m.tallyJobs())); strings.Contains(full, "R L") {
		t.Errorf("the bar names a remedy for an alarm it does not show: %q", full)
	}

	// The mirror row: an inactive-but-flagged Twitch.
	mtw := NewStatusBarModel()
	mtw.SetActivePlatforms(true, false)
	mtw.SetCookieStatus(CookieStatusOK, CookieStatusRelogin)
	if got := mtw.ReloginPlatform(); got != "" {
		t.Errorf("ReloginPlatform() = %q for an inactive platform, want \"\"", got)
	}
	if full := stripANSI(mtw.renderCookieStatus(tierFull, mtw.tallyJobs())); strings.Contains(full, "R L") {
		t.Errorf("the bar names a remedy for an alarm it does not show: %q", full)
	}
}

// TestReloginBadgeNamesTheChordThatAnswersIt pins R2: a bar that says
// "YT: Re-login" and stops has named a problem and no remedy. The dashboard's
// equivalent warning is clickable; R L is the TUI's click, and the badge is
// where an operator is looking when they need it.
//
// MUTANTS, one per assertion:
//   - drop the append → the widest bar names no remedy at all;
//   - append per platform instead of once → the both-flagged row reads
//     "YT: Re-login (R L) TW: Re-login (R L)", spending scarce width twice;
//   - append at tierCompact or below → the compact assertion fails (the hint
//     outlived the first squeeze); append at a LOWER tier but not tierFull →
//     that rung is wider than the one above it, which the ladder sweep at the
//     end catches. TestStatusBarTiersNarrowMonotonically cannot: its
//     busyStatusBar fixture is OK/OK and never renders the hint;
//   - gate on something other than ReloginPlatform (a bare `!= CookieStatusOK`,
//     say) → the healthy row advertises a login nobody needs.
func TestReloginBadgeNamesTheChordThatAnswersIt(t *testing.T) {
	flagged := func(yt, tw CookieStatus) *StatusBarModel {
		m := NewStatusBarModel()
		m.SetActivePlatforms(true, true)
		m.SetCookieStatus(yt, tw)
		return m
	}

	m1 := flagged(CookieStatusRelogin, CookieStatusOK)
	full := stripANSI(m1.renderCookieStatus(tierFull, m1.tallyJobs()))
	if !strings.Contains(full, "R L") {
		t.Errorf("the re-login badge names no chord at tierFull: %q", full)
	}

	m2 := flagged(CookieStatusRelogin, CookieStatusRelogin)
	both := stripANSI(m2.renderCookieStatus(tierFull, m2.tallyJobs()))
	if got := strings.Count(both, "R L"); got != 1 {
		t.Errorf("the chord is named %d times with both platforms flagged, want 1: %q", got, both)
	}

	m3 := flagged(CookieStatusRelogin, CookieStatusOK)
	compact := stripANSI(m3.renderCookieStatus(tierCompact, m3.tallyJobs()))
	if strings.Contains(compact, "R L") {
		t.Errorf("the hint survived past tierFull, which breaks the monotonic ladder: %q", compact)
	}

	m4 := flagged(CookieStatusOK, CookieStatusOK)
	healthy := stripANSI(m4.renderCookieStatus(tierFull, m4.tallyJobs()))
	if strings.Contains(healthy, "R L") {
		t.Errorf("a healthy bar advertises a cookie login: %q", healthy)
	}

	// The alert itself must still outlive the hint — the hint is the part that
	// is allowed to go, not the badge.
	m5 := flagged(CookieStatusRelogin, CookieStatusOK)
	tight := stripANSI(m5.renderCookieStatus(tierTight, m5.tallyJobs()))
	if !strings.Contains(tight, "YT") {
		t.Errorf("the re-login alert was dropped along with its hint: %q", tight)
	}

	// The ladder, swept on a FLAGGED bar. TestStatusBarTiersNarrowMonotonically
	// runs on busyStatusBar(), which is OK/OK, so this is the only place the
	// hint is inside the monotonicity check that fitTiers' scan relies on.
	ladder := flagged(CookieStatusRelogin, CookieStatusRelogin)
	ladder.SetWidth(200)
	tiers := ladder.metricTiers()
	for i := 1; i < len(tiers); i++ {
		if prev, cur := lipgloss.Width(tiers[i-1]), lipgloss.Width(tiers[i]); cur > prev {
			t.Errorf("metrics tier %d (%d cols) is wider than tier %d (%d cols) with the re-login "+
				"hint in play — the hint has landed on a rung below the one it is dropped from", i, cur, i-1, prev)
		}
	}
}

// unreadableBar is a bar whose cookies.txt could not be read, for one platform.
func unreadableBar(yt, tw CookieStatus) *StatusBarModel {
	m := NewStatusBarModel()
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(yt, tw)
	return m
}

// TestStatusBarNamesAnUnreadableCookieFile is COOKIES-6's TUI half. The bar had
// no state for it, so an unreadable cookies.txt rendered as CookieStatusNone —
// the yellow "never configured" badge — for a file sitting on the volume.
//
// Red and NOT gated on `healthy`, the same shape CookieStatusRelogin already
// has: it is a conclusive, operator-actionable failure, so it must survive
// every tier rather than dropping out at tierEssential like Unknown does.
//
// Mutants:
//   - render it through the `healthy` gate -> the tierEssential row loses the
//     badge exactly when the bar is narrowest and the operator most confused.
//   - reuse "YT!" (the Relogin abbreviation) -> the tierTight row becomes
//     indistinguishable from a re-login prompt, which has a different remedy.
//   - add the arm to the YouTube ladder only -> the Twitch subtest fails.
//   - abbreviate to the BARE code at the tight tiers (what this did until the
//     Arc M close review) -> the stripped tight and essential renders are
//     byte-identical to a CookiesOnly bar's, and the only thing telling the two
//     apart is the colour — invisible to a colour-blind operator, to a NO_COLOR
//     terminal and to every log or screenshot the bytes end up in.
//   - use "!" as the glyph -> it collides with Relogin's "YT!", and the
//     re-login comparison below fails.
func TestStatusBarNamesAnUnreadableCookieFile(t *testing.T) {
	for _, tc := range []struct {
		code    string
		unread  *StatusBarModel
		relogin *StatusBarModel
	}{
		{"YT", unreadableBar(CookieStatusFileUnreadable, CookieStatusOK), unreadableBar(CookieStatusRelogin, CookieStatusOK)},
		{"TW", unreadableBar(CookieStatusOK, CookieStatusFileUnreadable), unreadableBar(CookieStatusOK, CookieStatusRelogin)},
	} {
		t.Run(tc.code, func(t *testing.T) {
			m, r := tc.unread, tc.relogin

			full := stripANSI(m.renderCookieStatus(tierFull, m.tallyJobs()))
			if want := tc.code + ": cookies.txt unreadable"; !strings.Contains(full, want) {
				t.Errorf("tierFull = %q, want it to contain %q — the operator is sent to the "+
					"permission, not back through a cookie setup they already did", full, want)
			}

			// The tight tiers carry the GLYPH, not the bare code: the colour is
			// the only other thing separating this badge from CookiesOnly's, and
			// colour alone is not a distinction a colour-blind operator, a
			// NO_COLOR terminal or a pasted screenshot can carry.
			glyph := tc.code + "?"
			rejected := unreadableBar(CookieStatusCookiesOnly, CookieStatusOK)
			if tc.code == "TW" {
				rejected = unreadableBar(CookieStatusOK, CookieStatusCookiesOnly)
			}
			for _, tier := range []struct {
				name string
				t    barTier
			}{{"tierTight", tierTight}, {"tierEssential", tierEssential}} {
				got := stripANSI(m.renderCookieStatus(tier.t, m.tallyJobs()))
				if !strings.Contains(got, glyph) {
					t.Errorf("%s = %q, want it to carry %q — an unreadable cookies.txt is conclusive "+
						"and actionable, and at this width the label is all the operator has",
						tier.name, got, glyph)
				}
				if reject := stripANSI(rejected.renderCookieStatus(tier.t, rejected.tallyJobs())); got == reject {
					t.Errorf("%s renders %q for both an unreadable file and rejected credentials once "+
						"the SGR bytes are stripped — the remedies are a permission fix and a "+
						"re-export, and colour cannot be the only thing telling them apart", tier.name, got)
				}
				if relogin := stripANSI(r.renderCookieStatus(tier.t, r.tallyJobs())); got == relogin {
					t.Errorf("%s renders %q for both an unreadable file and a re-login prompt — "+
						"two alerts with different remedies are one alert", tier.name, got)
				}
			}
		})
	}
}

// TestUnreadableBadgeIsDistinctFromRejectedCredentials is fix round 1's
// Minor 4. cookieFileErrorLabel abbreviates to the bare code at tierTight so
// it cannot be confused with the re-login prompt's "YT!" — and that left it
// byte-identical to the CookiesOnly arm, which renders the bare code too. Two
// alerts that render identically are one alert, which is the helper's own
// argument used against it by a different neighbour.
//
// Fixed by COLOUR rather than by a longer label: the label is the scarce thing
// at these tiers, and ColorCookies is already this program's "a cookie file is
// the problem" colour (the COOKIES? job status). Asserted on the RAW render,
// not the stripped one — the escape sequence is what the operator's terminal
// actually paints, and stripANSI is a test convenience, not the surface.
//
// ONE PLATFORM ACTIVE PER ROW, so the whole render is the badge under test.
// With both active a mutant that recolours only ONE ladder's CookiesOnly arm
// leaves the other platform's badge still differing, and the comparison passes
// over a collision it was written to catch.
//
// Mutants:
//   - render the unreadable arms in statusBarRedStyle -> every tier collapses
//     onto the CookiesOnly rendering and the two states are one badge again.
//   - give either ladder's CookiesOnly arm the cookie colour -> the same
//     collapse from the other side, on that platform's row.
func TestUnreadableBadgeIsDistinctFromRejectedCredentials(t *testing.T) {
	// One platform active at a time; the other renders nothing at all.
	bar := func(ytActive bool, state CookieStatus) *StatusBarModel {
		m := NewStatusBarModel()
		m.SetActivePlatforms(ytActive, !ytActive)
		if ytActive {
			m.SetCookieStatus(state, CookieStatusOK)
		} else {
			m.SetCookieStatus(CookieStatusOK, state)
		}
		return m
	}

	for _, platform := range []struct {
		code   string
		ytSide bool
	}{{"YT", true}, {"TW", false}} {
		for _, tier := range []barTier{tierFull, tierKeys, tierTight, tierEssential} {
			unread := bar(platform.ytSide, CookieStatusFileUnreadable)
			got := unread.renderCookieStatus(tier, unread.tallyJobs())
			if stripANSI(got) == "" {
				t.Errorf("%s tier %d: the unreadable badge rendered nothing", platform.code, tier)
			}
			for _, other := range []struct {
				name  string
				state CookieStatus
			}{
				{"rejected credentials", CookieStatusCookiesOnly},
				{"a re-login prompt", CookieStatusRelogin},
			} {
				o := bar(platform.ytSide, other.state)
				if want := o.renderCookieStatus(tier, o.tallyJobs()); got == want {
					t.Errorf("%s tier %d: an unreadable cookies.txt renders exactly like %s (%q) — "+
						"the remedies are a permission fix, a re-export and a browser login, "+
						"and one badge cannot mean all three", platform.code, tier, other.name, stripANSI(got))
				}
			}
		}
	}
}
