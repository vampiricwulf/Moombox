package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Status-bar density tiers, richest to poorest. Both halves of the bar
// carry a tier ladder and View picks the richest pair that actually fits
// the measured width (see fitTiers), so the bar adapts to real content —
// a long backfill channel name, "12 selected", OFFLINE — instead of to a
// guessed column count. The previous rule was a cliff: one width
// threshold flipped both halves between two renderings, and any remaining
// overflow dropped the ENTIRE chord-hint half, so a narrow window showed
// no keybinds at all.
type barTier int

const (
	tierFull      barTier = iota // full labels: "Tab Focus", "Disk 45% (120G free)"
	tierCompact                  // shortened labels: "Tab", "D:45% 120G"
	tierKeys                     // chords lose their names; right keeps alerts + counts
	tierTight                    // chords space-separated; right keeps alerts only
	tierEssential                // only the chords that lead everywhere else; right only if warning
	tierNone                     // half omitted entirely — last resort
)

// Package-level styles for status bar rendering (avoid alloc per render).
var (
	statusBarBgStyle  = lipgloss.NewStyle().Background(lipgloss.Color("#1a1a2e")).Foreground(ColorWhite)
	statusBarKeyStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	statusBarRedStyle = lipgloss.NewStyle().Foreground(ColorRed)
	statusBarGrnStyle = lipgloss.NewStyle().Foreground(ColorGreen)
	statusBarWrnStyle = lipgloss.NewStyle().Foreground(ColorWarning)
	statusBarYelStyle = lipgloss.NewStyle().Foreground(ColorYellow)
	// statusBarCookieStyle is the unreadable-cookie-file alert, and the only
	// alert on this bar that is not red. It has to be distinguishable from the
	// other two at the tight tiers, where every label collapses to a bare "YT"
	// and colour is the ONLY thing left — a red "YT" already means "the
	// credentials were rejected, or a job parked", whose remedy is a
	// re-export, and "YT!" already means "sign in again". A third remedy needs
	// a third rendering, and a longer label is exactly what these tiers do not
	// have room for. ColorCookies rather than a new value: it is already this
	// program's "a cookie file is the problem" colour (the COOKIES? job
	// status), so the bar borrows a meaning the operator has already seen.
	statusBarCookieStyle = lipgloss.NewStyle().Foreground(ColorCookies)
)

// CookieStatus represents the authentication state for a platform.
type CookieStatus int

const (
	CookieStatusNone CookieStatus = iota
	CookieStatusOK
	CookieStatusCookiesOnly
	CookieStatusRelogin
	// CookieStatusUnknown: cookies ARE configured and the last check could
	// not conclude anything about them — a network fault, a non-200, a
	// redirect, or a body with no login marker we recognise.
	//
	// APPENDED, not inserted: CookieStatusNone is the zero value and both the
	// TUI wiring and every test rely on an unset status meaning "no cookies".
	//
	// Distinct from CookieStatusCookiesOnly, which this state used to be
	// rendered as, and that conflation is the bug: CookiesOnly is a
	// CONCLUSION (the credentials were rejected, or there are none) and earns
	// the red alert that survives every tier, while this one is an absence of
	// information the operator cannot act on.
	CookieStatusUnknown
	// CookieStatusFileUnreadable: cookies.txt is PRESENT and could not be read
	// (permission, a wrong mount). Appended for the same reason
	// CookieStatusUnknown was, and distinct from CookieStatusNone for the
	// reason this state exists at all: a file that cannot be read used to
	// render as "never configured", which sends the operator to set cookies up
	// again instead of to the permission that is actually broken.
	CookieStatusFileUnreadable
)

// StatusBarModel renders the bottom status bar.
type StatusBarModel struct {
	width    int
	ytCookie CookieStatus
	twCookie CookieStatus
	ytActive bool
	twActive bool
	// Jobs list for detecting COOKIES? status (B1)
	jobs []*database.Job
	// Disk status
	diskFree    uint64
	diskUsedPct float64
	diskWarn    string // "ok", "warn", "critical"
	// In-flight backfill scan (spec §11 progress surfacing). Scans are
	// strictly serial, so one slot IS the whole "scanning" state; empty
	// backfillChannel hides the indicator. Terminal states clear the slot —
	// done/idle are quiet by design and error is already loud in the log
	// panel (the R B overlay owns detailed per-channel state).
	backfillChannel string // channel ID of the in-flight scan ("" = none)
	backfillName    string // display name resolved by the App handler
	backfillTab     string
	backfillPages   int
	// ShowChordHint shows a newcomer hint instead of the full chord reference.
	ShowChordHint bool
	// SelectedCount tracks batch-selected jobs for status bar display.
	SelectedCount int
	// offline indicates that internet connectivity is unavailable.
	offline bool
	// sidecarDown indicates that the BotGuard sidecar is not running. An
	// alert, not a status: while it is down, signature-ciphered formats
	// cannot be resolved at all (sig has no goja fallback) and PO tokens
	// fall to a path that errors.
	sidecarDown bool
}

// NewStatusBarModel creates a new status bar model.
func NewStatusBarModel() *StatusBarModel {
	return &StatusBarModel{diskWarn: "ok"}
}

// SetWidth updates the bar width.
func (m *StatusBarModel) SetWidth(w int) {
	m.width = w
}

// SetCookieStatus sets the cookie status for a platform.
func (m *StatusBarModel) SetCookieStatus(yt, tw CookieStatus) {
	m.ytCookie = yt
	m.twCookie = tw
}

// SetActivePlatforms sets which platform indicators are visible.
func (m *StatusBarModel) SetActivePlatforms(yt, tw bool) {
	m.ytActive = yt
	m.twActive = tw
}

// ReloginPlatform names the platform this bar is currently asking the operator
// to sign back in to, or "" when neither is.
//
// Gated on the ACTIVE flags, so it answers about the platforms the bar actually
// renders: a Relogin verdict for a platform with no configured monitors is not
// being shown to anyone, and preselecting it would answer an alarm never raised.
// YouTube wins when both are flagged — the overlay signs in to one platform at
// a time, the operator can pick the other row, and more of the pipeline depends
// on YouTube's credentials.
//
// Two readers, deliberately ONE predicate: the R L chord preselects this
// platform, and renderCookieStatus decides on it whether to name the chord at
// all. A second copy would let the badge advertise a remedy the chord then
// opens elsewhere.
func (m *StatusBarModel) ReloginPlatform() string {
	if m.ytActive && m.ytCookie == CookieStatusRelogin {
		return "youtube"
	}
	if m.twActive && m.twCookie == CookieStatusRelogin {
		return "twitch"
	}
	return ""
}

// SetJobs updates the jobs reference for COOKIES? detection (B1).
func (m *StatusBarModel) SetJobs(jobs []*database.Job) {
	m.jobs = jobs
}

// SetDiskStatus updates the disk space display.
func (m *StatusBarModel) SetDiskStatus(free uint64, usedPct float64, warn string) {
	m.diskFree = free
	m.diskUsedPct = usedPct
	m.diskWarn = warn
}

// SetBackfillStatus updates the backfill scan indicator from a
// BackfillStatusMsg. "scanning" occupies the single slot; any other state
// clears it — but only for the channel that owns it, so a seeded terminal
// state for another channel can't blank an in-flight scan's display.
func (m *StatusBarModel) SetBackfillStatus(chID, name, tab string, pages int, state string) {
	if state == "scanning" {
		m.backfillChannel = chID
		m.backfillName = name
		m.backfillTab = tab
		m.backfillPages = pages
		return
	}
	if m.backfillChannel == chID {
		m.backfillChannel = ""
		m.backfillName = ""
		m.backfillTab = ""
		m.backfillPages = 0
	}
}

// View renders the status bar.
func (m *StatusBarModel) View() string {
	if m.width <= 0 {
		return ""
	}

	left, right := m.fitTiers()

	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)

	// At least one column between the halves so they never abut; fitTiers
	// budgeted for it. max() also keeps Repeat's count non-negative in the
	// pathological case where even tierNone/tierNone overflows (a width of
	// 1-2 columns), which the MaxWidth clamp below then trims.
	padding := max(m.width-leftW-rightW, 1)

	bar := left + strings.Repeat(" ", padding) + right

	if barW := lipgloss.Width(bar); barW < m.width {
		bar += strings.Repeat(" ", m.width-barW)
	}

	// MaxWidth is the hard guard: an over-wide bar used to WRAP, pushing a
	// second line into the terminal and corrupting the frame below it.
	// Clamping (ANSI-aware, so styled runs are cut without severing escape
	// sequences) degrades to a clipped bar instead of a broken layout.
	return statusBarBgStyle.MaxWidth(m.width).Render(bar)
}

// statusBarDescent is the fixed order in which the two halves give up
// density, richest pair first. Three phases:
//
//  1. The RIGHT steps down alone to tierKeys. Status verbosity is the
//     cheapest thing to lose — the backfill line and the long disk label
//     are the widest, least urgent content on the bar — so the chord hints
//     keep their full labels through the first two steps.
//  2. The LEFT follows down to tierKeys, shedding its labels.
//  3. From there the two ALTERNATE, right first, down to tierNone.
//
// Because every entry lowers exactly one side by exactly one rung and both
// ladders narrow monotonically (pinned by TestStatusBarTiersNarrowMonotonically),
// widths along this path are non-increasing — so the FIRST entry that fits
// is also the richest one that fits. That is what lets fitTiers be a plain
// scan: an earlier greedy version had to climb back out of overshoot, where
// stepping one side down hid content the other side's step alone would have
// freed room for.
var statusBarDescent = [...]struct{ left, right barTier }{
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

// fitTiers picks the richest (left, right) tier pair whose combined width
// fits m.width, including the one-column gap between them, by walking
// statusBarDescent. Falls through to the fully-empty pair when even that
// overflows (a 1-2 column window); View's MaxWidth clamp is the backstop.
func (m *StatusBarModel) fitTiers() (string, string) {
	lefts := m.controlTiers()
	rights := m.metricTiers()

	for _, step := range statusBarDescent {
		if lipgloss.Width(lefts[step.left])+1+lipgloss.Width(rights[step.right]) <= m.width {
			return lefts[step.left], rights[step.right]
		}
	}
	return lefts[tierNone], rights[tierNone]
}

// controlTiers renders the chord hints at every density, richest first,
// indexed by barTier. Every tier keeps the leading space that separates the
// hints from the terminal edge.
//
// The ladder drops, in order: the long labels on the single-glyph chords
// ("Tab Focus" → "Tab"), then the remaining chord names ("A Action" → "A"),
// then the pipe separators, then all but the two chords that reach every
// other feature — M opens the menu and ? opens help, so those two survive
// longest. Only tierNone hides the keybinds outright, and fitTiers reaches
// it only when the window cannot seat even a single glyph plus the gap.
func (m *StatusBarModel) controlTiers() []string {
	if m.ShowChordHint {
		// Newcomer hint: same ladder shape, prose instead of chords.
		return []string{
			" " + DimStyle.Render("Press ? for help · M for menu · A for actions"),
			" " + DimStyle.Render("? help · M menu · A actions"),
			" " + DimStyle.Render("? help · M menu"),
			" " + DimStyle.Render("? for help"),
			" " + DimStyle.Render("?"),
			"",
		}
	}

	key := statusBarKeyStyle
	named := []string{
		key.Render("A") + " Action",
		key.Render("R") + " Request",
		key.Render("O") + " Open",
		key.Render("F") + " Filter",
		key.Render("M") + " Menu",
	}
	glyphs := []string{key.Render("Tab"), key.Render("`"), key.Render("?")}
	bare := []string{
		key.Render("A"), key.Render("R"), key.Render("O"),
		key.Render("F"), key.Render("M"),
	}

	full := append(append([]string{}, named...),
		key.Render("Tab")+" Focus",
		key.Render("`")+" Settings",
		key.Render("?")+" Help",
	)
	compact := append(append([]string{}, named...), glyphs...)
	keysOnly := append(append([]string{}, bare...), glyphs...)

	return []string{
		" " + strings.Join(full, " | "),
		" " + strings.Join(compact, " | "),
		" " + strings.Join(keysOnly, " | "),
		" " + strings.Join(keysOnly, " "),
		" " + key.Render("M") + " " + key.Render("?"),
		"",
	}
}

// barJobCounts is everything the bar derives from the job list. Both halves
// of it used to be recomputed inside the per-tier renderers, and metricTiers
// renders all six tiers before fitTiers picks one — about eight walks of the
// whole job list per frame, at up to 60 frames a second.
type barJobCounts struct {
	active   int  // Downloading + Live + Muxing (Queued waits for a slot)
	ytParked bool // a job parked in COOKIES? on YouTube (an empty Platform counts as YouTube)
	twParked bool // a job parked in COOKIES? on Twitch
}

// tallyJobs walks the job list once per frame. Two passes rather than one:
// parkedCookieJobs stays the named home of the attribution rule (the spec doc
// cites it by symbol and declaring file), so this calls it instead of
// inlining it.
func (m *StatusBarModel) tallyJobs() barJobCounts {
	c := barJobCounts{}
	c.ytParked, c.twParked = m.parkedCookieJobs()
	for _, j := range m.jobs {
		switch j.Status {
		case database.StatusDownloading, database.StatusLive, database.StatusMuxing:
			c.active++
		}
	}
	return c
}

// metricTiers renders the right half (metrics + auth indicators) at every
// density, richest first, indexed by barTier.
//
// What survives longest is what the operator cannot afford to miss:
// OFFLINE, a disk warning, and a re-login prompt are alerts, while the
// backfill scan, the selection count, and the active-download tally are
// informational. So the informational items drop first, then the routine
// (non-warning) disk and cookie readouts, leaving a bar that is silent
// when everything is healthy and still shouts when it isn't.
func (m *StatusBarModel) metricTiers() []string {
	counts := m.tallyJobs()
	tiers := make([]string, tierNone+1)
	for t := tierFull; t <= tierNone; t++ {
		tiers[t] = m.renderMetrics(t, counts) + m.renderCookieStatus(t, counts)
	}
	return tiers
}

// renderMetrics renders disk usage and activity indicators at tier t.
func (m *StatusBarModel) renderMetrics(t barTier, counts barJobCounts) string {
	if t >= tierNone {
		return ""
	}
	var parts []string

	// Connectivity indicator — an alert, so it outlives every counter and
	// only abbreviates.
	if m.offline {
		if t >= tierTight {
			parts = append(parts, statusBarRedStyle.Render("OFF"))
		} else {
			parts = append(parts, statusBarRedStyle.Render("OFFLINE"))
		}
	}

	// BotGuard sidecar — an alert for the same reason OFFLINE is one: it names
	// a capability that is GONE, not a number. So it abbreviates instead of
	// disappearing.
	if m.sidecarDown {
		if t >= tierTight {
			parts = append(parts, statusBarRedStyle.Render("POT"))
		} else {
			parts = append(parts, statusBarRedStyle.Render("SIDECAR DOWN"))
		}
	}

	// Batch selection count — informational; the count itself is the point,
	// so it abbreviates to a bare number before disappearing.
	if m.SelectedCount > 0 && t <= tierKeys {
		if t == tierKeys {
			parts = append(parts, statusBarYelStyle.Render(fmt.Sprintf("%d sel", m.SelectedCount)))
		} else {
			parts = append(parts, statusBarYelStyle.Render(fmt.Sprintf("%d selected", m.SelectedCount)))
		}
	}

	// Backfill scan in flight (green, like the Active indicator — a routine
	// background activity, not a warning), so it is the first thing dropped.
	if m.backfillChannel != "" && t <= tierCompact {
		if t == tierCompact {
			parts = append(parts, statusBarGrnStyle.Render(
				fmt.Sprintf("BF:%s p%d", m.backfillTab, m.backfillPages)))
		} else {
			name := m.backfillName
			if r := []rune(name); len(r) > 16 {
				name = string(r[:15]) + "…"
			}
			parts = append(parts, statusBarGrnStyle.Render(
				fmt.Sprintf("Backfill %s: %s p%d", name, m.backfillTab, m.backfillPages)))
		}
	}

	// Disk indicator (only shown once we have data). At tierEssential only a
	// warning/critical reading is worth the columns — a healthy disk says
	// nothing there.
	warning := m.diskWarn == "warn" || m.diskWarn == "critical"
	if (m.diskFree > 0 || m.diskUsedPct > 0) && (t < tierEssential || warning) {
		var style lipgloss.Style
		switch m.diskWarn {
		case "critical":
			style = statusBarRedStyle
		case "warn":
			style = statusBarWrnStyle
		default:
			style = statusBarGrnStyle
		}

		freeGB := float64(m.diskFree) / (1024 * 1024 * 1024)
		pct := int(m.diskUsedPct)
		switch {
		case t >= tierTight:
			parts = append(parts, style.Render(fmt.Sprintf("D:%d%%", pct)))
		case t >= tierCompact:
			parts = append(parts, style.Render(fmt.Sprintf("D:%d%% %.0fG", pct, freeGB)))
		default:
			parts = append(parts, style.Render(fmt.Sprintf("Disk %d%% (%.0fG free)", pct, freeGB)))
		}
	}

	// Active download count (StatusQueued is deliberately absent — a queued
	// job is waiting for an archive slot, not an active download).
	if t <= tierKeys && counts.active > 0 {
		if t >= tierCompact {
			parts = append(parts, statusBarGrnStyle.Render(fmt.Sprintf("▶%d", counts.active)))
		} else {
			parts = append(parts, statusBarGrnStyle.Render(fmt.Sprintf("Active: %d", counts.active)))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// cookieUnknownLabel words the could-not-check badge for one platform.
//
// "Unknown" rather than a fourth invention: it is what RefreshVerdict.String()
// returns for this state, and the same three-way vocabulary already carries
// the setup toasts and the R F feedback. A badge cannot hold the full sentence
// ("could not establish whether they work"), so it borrows the enum's own word
// instead of coining a synonym that would drift.
//
// Abbreviates at tierTight like the re-login prompt does. The bare code is
// still colour-distinct from the green OK and the yellow no-cookies, and the
// state is dropped one tier later anyway.
func cookieUnknownLabel(code string, t barTier) string {
	if t >= tierTight {
		return code
	}
	return code + ": Unknown"
}

// cookieFileErrorLabel is cookieUnknownLabel's sibling for the unreadable-file
// state. Abbreviates at tierTight like the others, but to the BARE code rather
// than to "YT!" — that spelling belongs to the re-login prompt, whose remedy is
// a browser login and not a permission fix, and two alerts that render
// identically are one alert.
//
// The bare code collides with the CookiesOnly arm, which renders one too; what
// separates them at those tiers is the COLOUR (statusBarCookieStyle, not red),
// because the label is the scarce resource there and a third glyph would have
// to be learnt. Pinned across all four tiers by
// TestUnreadableBadgeIsDistinctFromRejectedCredentials.
func cookieFileErrorLabel(code string, t barTier) string {
	if t >= tierTight {
		return code
	}
	return code + ": cookies.txt unreadable"
}

// parkedCookieJobs reports whether any job is parked in COOKIES? for YouTube
// and for Twitch, separately.
//
// SEPARATELY is the whole point. This used to be one unfiltered bool set by
// any parked job whatever, and it was consumed only inside the YouTube
// branch — so a parked TWITCH job reddened the YOUTUBE indicator and left the
// Twitch one untouched. The badge pointed at the wrong platform and stayed
// silent about the right one, which is worse than not escalating at all: it
// sends the operator to re-export credentials that were never the problem.
//
// An EMPTY Platform counts as YouTube, deliberately, for three reasons:
//
//   - it is what the rest of the TUI already means by the absence of
//     "twitch" (task_list.go:579, job_details.go:249, app_actions.go:387 all
//     test for "twitch" and treat everything else as YouTube), and a status
//     bar that partitioned platforms differently from the rows above it would
//     be its own defect;
//   - the rows that actually carry an empty Platform are pre-Twitch ones, and
//     ImportFromJSON (database_jobs.go:905) already backfills exactly this
//     value when it meets them;
//   - of the three candidate rules it is the only one that neither loses the
//     alert (reddening neither) nor asserts a Twitch failure on no evidence
//     (reddening both). Reddening "whichever platform is configured" was the
//     fourth option and it decides nothing when both are.
//
// The consequence to be aware of: a job on some future third platform would
// also land on YouTube here. That is not an oversight this function can fix
// alone — every platform test named above would have to move together — so it
// stays binary, matching them, rather than inventing a third answer here.
//
// One site deliberately decides the empty case the OTHER way and is not a
// counter-example to the above: worker.go:1072 is choosing which platform to
// drive a refresh AGAINST, where guessing wrong does work on the wrong
// credentials. Attributing a badge and dispatching a refresh carry different
// costs for the same unknown, so they are allowed to differ — but a survey
// that listed only the agreeing sites would have read as more settled than it
// is.
//
// This deliberately does NOT filter on ParkReason, and the distinction is
// worth stating because the badge is coarser than the status it reads. Coarser
// in two ways, not one — an earlier draft of this comment said "two reasons,
// dead credentials or membership", and that is a tidier split than the code:
//
//   - ParkReasonMembership: the request WAS signed in and YouTube refused
//     anyway. worker.go:1058 logs "the credentials are alive and the account
//     simply lacks access". The remedy is cookies from a DIFFERENT account.
//   - ParkReasonAuth: usually dead credentials — but not only. worker.go:884-890
//     files twitch.ErrSubscriberOnly here ON PURPOSE, because Usher's 403 cannot
//     tell an anonymous session from an un-entitled one. So an Auth park can sit
//     on a perfectly healthy, signed-in Twitch session.
//   - ParkReasonNone: the zero value every pre-v18 COOKIES? row still carries,
//     because migrateV18 deliberately backfills nothing. Sweeps treat it as
//     Auth; so does this, by not looking.
//
// All three escalate, because in all three the remedy is credentials of some
// kind, and filtering any of them out would lose a real alarm. What the red
// badge means is therefore "a download stopped for want of usable credentials",
// NOT "your cookies expired" — job_details.go:548-554 is where the operator
// gets the difference.
func (m *StatusBarModel) parkedCookieJobs() (yt, tw bool) {
	for _, j := range m.jobs {
		if j.Status != database.StatusCookies {
			continue
		}
		if j.Platform == "twitch" {
			tw = true
		} else {
			yt = true
		}
		if yt && tw {
			break
		}
	}
	return yt, tw
}

// renderCookieStatus renders auth indicators and warnings (B2) at tier t.
//
// A platform whose auth is HEALTHY is dropped at tierEssential — the green
// "YT" is reassurance, not information — while a re-login prompt or a
// rejected-cookie indicator survives to the last tier, abbreviated. That
// keeps the narrowest useful bar showing only what needs acting on.
//
// A job parked in COOKIES? escalates ITS OWN platform's indicator to that
// surviving red, and only its own — see parkedCookieJobs. Both branches carry
// the escalation now; the Twitch one never had it.
//
// CookieStatusUnknown joins the dropped group rather than the surviving one,
// and that placement IS the fix. "The last check could not reach YouTube" is
// not something the operator can act on — it is the state that used to render
// as the red always-visible CookiesOnly alert, so a DNS blip shouted at the
// same volume as a dead session for as long as it lasted. The tier rule
// already says what to do with un-actionable information; this state simply
// belongs on that side of it.
//
// CookieStatusFileUnreadable goes on the OTHER side, immediately under the
// re-login prompt: cookies.txt is present and could not be read, which is
// conclusive, is the operator's to fix, and explains every symptom the arms
// below it would otherwise report. It used to render as CookieStatusNone —
// the yellow never-configured badge — which sends them back through a cookie
// setup they have already done.
//
// THE REASON IS DELIBERATELY ABSENT HERE, and it is the one place in the tree
// where that is a decision rather than an omission. AuthStatus carries
// YouTubeError / TwitchError — WHY a check landed where it did, which since
// Arc 10 covers a conclusive REFUSAL as well as an inconclusive check (the
// unsignable-jar sentinel, and the Twitch chat-downgrade mark) — and Arc 8
// Task 12a gave them readers on the two PER-REQUEST paths: the REST payload
// (CookieStatusPayload) and the R C result line. This panel is not one of
// those. It is fed by pushes from RefreshService.OnAuthChange, and
// authStatusChanged (internal/cookies/refresh.go, and it is the function name
// that is the reference here — the line has moved twice) deliberately
// excludes the two reason strings from its change-detection gate — so a
// reason-only change produces no push, and a reason rendered here would sit
// unchanged beside a verdict that is still correct. Widening that gate is the
// precondition for putting the reason on this line; refresh.go is not this
// task's to change. Until then the operator gets the reason on the next R C,
// which is a recheck away, rather than live.
func (m *StatusBarModel) renderCookieStatus(t barTier, counts barJobCounts) string {
	if t >= tierNone || (!m.ytActive && !m.twActive) {
		return ""
	}

	var parts []string

	// Jobs parked in COOKIES?, attributed to the platform they belong to (B1).
	// Tallied once per frame by tallyJobs; see parkedCookieJobs for the rule.
	ytRejected, twRejected := counts.ytParked, counts.twParked

	// healthy reports whether a platform's indicator is pure reassurance —
	// dropped once space is scarce (tierEssential).
	healthy := t < tierEssential

	// YouTube status (only if active)
	if m.ytActive {
		switch {
		case m.ytCookie == CookieStatusRelogin:
			if t >= tierTight {
				parts = append(parts, statusBarRedStyle.Render("YT!"))
			} else {
				parts = append(parts, statusBarRedStyle.Render("YT: Re-login"))
			}
		case m.ytCookie == CookieStatusFileUnreadable:
			// Above the parked/CookiesOnly arm: a cookies.txt that cannot be
			// read explains every symptom below it, and it is the one remedy
			// the other arms would send the operator away from. Not gated on
			// `healthy` — conclusive and actionable, like Re-login. Rendered in
			// statusBarCookieStyle, not red: at tierTight and below the label
			// is a bare "YT" and colour is all that separates this from the
			// rejected-credentials badge beneath it.
			parts = append(parts, statusBarCookieStyle.Render(cookieFileErrorLabel("YT", t)))
		case ytRejected || m.ytCookie == CookieStatusCookiesOnly:
			// ytRejected stays ahead of the unknown arm on purpose: a job
			// parked in COOKIES? is evidence from a real download attempt, and
			// it outranks a check that could not reach the site.
			parts = append(parts, statusBarRedStyle.Render("YT"))
		case m.ytCookie == CookieStatusUnknown:
			if healthy {
				parts = append(parts, statusBarWrnStyle.Render(cookieUnknownLabel("YT", t)))
			}
		case m.ytCookie == CookieStatusNone:
			if healthy {
				parts = append(parts, statusBarYelStyle.Render("YT"))
			}
		case m.ytCookie == CookieStatusOK:
			if healthy {
				parts = append(parts, statusBarGrnStyle.Render("YT"))
			}
		default:
			if healthy {
				parts = append(parts, DimStyle.Render("YT"))
			}
		}
	}

	// Twitch status (only if active).
	//
	// Switched on no value, like the YouTube ladder above it, so twRejected can
	// join its arm. The two blocks have drifted apart repeatedly — a state
	// reachable on one and dead on the other, an escalation present on one and
	// missing here — and keeping them the same SHAPE is what makes the next
	// divergence visible. CookieStatusNone is still handled by default and not
	// by an arm of its own: Twitch without cookies is ordinary anonymous mode
	// and gets the neutral dim indicator, unlike YouTube's yellow warning.
	if m.twActive {
		switch {
		case m.twCookie == CookieStatusRelogin:
			if t >= tierTight {
				parts = append(parts, statusBarRedStyle.Render("TW!"))
			} else {
				parts = append(parts, statusBarRedStyle.Render("TW: Re-login"))
			}
		case m.twCookie == CookieStatusFileUnreadable:
			// Same rank and the same reason as the YouTube arm above; the two
			// ladders stay the same SHAPE so the next divergence is visible.
			// One cookies.txt holds both platforms, so both badges can carry it.
			parts = append(parts, statusBarCookieStyle.Render(cookieFileErrorLabel("TW", t)))
		case twRejected || m.twCookie == CookieStatusCookiesOnly:
			// Same precedence as YouTube's, and for the same reason: a parked
			// job is evidence from a real download attempt and outranks a check
			// that could not reach the site.
			//
			// CookiesOnly here is reachable since AuthStatus gained
			// HasTwitchCookies. It was dead code for as long as the TUI could
			// only assign CookieStatusOK for Twitch, which meant a Twitch
			// session whose auth-token had been pruned on expiry looked exactly
			// like one that was never set up.
			parts = append(parts, statusBarRedStyle.Render("TW"))
		case m.twCookie == CookieStatusUnknown:
			if healthy {
				parts = append(parts, statusBarWrnStyle.Render(cookieUnknownLabel("TW", t)))
			}
		case m.twCookie == CookieStatusOK:
			if healthy {
				parts = append(parts, statusBarGrnStyle.Render("TW"))
			}
		default:
			if healthy {
				parts = append(parts, DimStyle.Render("TW"))
			}
		}
	}

	// The chord that ANSWERS the alert, named once for the bar and only where
	// there is room. A badge that says "Re-login" and stops has named a problem
	// and no remedy; the dashboard's warning is clickable, and R L is this
	// surface's click.
	//
	// tierFull only: it is the widest thing this function can add and the least
	// urgent — the alert is the information, and the remedy is also in the menu
	// and in help — so it is given up first, which is what keeps metricTiers
	// narrowing monotonically for fitTiers' scan. ReloginPlatform is the same
	// predicate R L preselects on, so the badge cannot advertise a remedy that
	// then opens on the other platform.
	if t == tierFull && m.ReloginPlatform() != "" {
		parts = append(parts, DimStyle.Render("(")+statusBarKeyStyle.Render("R L")+DimStyle.Render(")"))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}
