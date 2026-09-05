package cookies

// autocookies_verdict.go — the refresh-outcome types and the verdict algebra
// over them: RefreshVerdict, RecheckReport, RefreshResult and the helpers that
// build or fold them.

import (
	"strings"
)

// RefreshVerdict is what a refresh pass concluded about ONE platform.
//
// The three states exist for the same reason verificationState's do: "we did
// not find out" is not "it is dead", and a notification built on the second
// when only the first is true tells an operator to re-export cookies that are
// perfectly fine.
type RefreshVerdict int

const (
	// RefreshUnknown means this pass learned nothing about the platform: the
	// refresh declined to run, aborted before it could verify, or the
	// verification itself could not reach the service. It is the ZERO VALUE
	// on purpose — a caller that forgets to populate a field must not
	// accidentally assert health or failure.
	RefreshUnknown RefreshVerdict = iota
	// RefreshFailed means the platform is conclusively not authenticated
	// after this pass. It covers "the credentials were rejected" and "there
	// are no credentials at all" alike: the question this answers is whether
	// authenticated requests will work, and in both cases they will not.
	RefreshFailed
	// RefreshOK means the platform is conclusively authenticated.
	RefreshOK
)

func (v RefreshVerdict) String() string {
	switch v {
	case RefreshFailed:
		return "failed"
	case RefreshOK:
		return "ok"
	default:
		return "unknown"
	}
}

// RecheckedPlatform pairs one platform's display label with what the manual
// recheck concluded about it.
type RecheckedPlatform struct {
	Label   string
	Verdict RefreshVerdict
}

// RecheckReport words the answer to a manual "recheck cookies".
//
// TWO SURFACES, ONE SENTENCE, exported for the same reason
// RefreshDeclinedCauses is: the TUI's R C chord and the Web dashboard's
// refresh button are THE SAME GESTURE, and they were answering it differently
// — the TUI with a three-way verdict, the Web with a two-arm
// "successful"/"completed" keyed on a success bool that is false for a check
// that never reached the site. The Web copy cannot import Go, so a test pins
// the rendered string against this function; sharing the sentence is what
// stops a fourth phrasing appearing the next time one side is edited.
//
// Only RefreshFailed says anything about the credentials. RefreshUnknown
// speaks about the CHECK — "could not establish", the arc's settled wording —
// because a check that could not reach the site has concluded nothing, and
// telling an operator their cookies failed is how they get sent off to
// re-export a session that is perfectly alive.
//
// Callers pass only the platforms they actually monitor; an empty list is a
// real state (nothing configured) and gets its own sentence rather than an
// empty one.
func RecheckReport(platforms ...RecheckedPlatform) string {
	if len(platforms) == 0 {
		return "Cookies: no platforms configured"
	}
	parts := make([]string, 0, len(platforms))
	for _, p := range platforms {
		switch p.Verdict {
		case RefreshOK:
			parts = append(parts, p.Label+" OK")
		case RefreshFailed:
			parts = append(parts, p.Label+" not authenticated")
		default:
			parts = append(parts, p.Label+" — could not establish")
		}
	}
	return "Cookies: " + strings.Join(parts, ", ")
}

// The two values of RefreshResult.Mechanism: which cookie source a refresh pass
// actually used. Exported because internal/web/routes puts them on the wire and
// internal/tui renders them, and because a literal repeated across three
// packages and one JavaScript file is how a vocabulary drifts.
//
// Deliberately NOT the cookies.acquisition values, and not spelled like them.
// Those name what the operator ASKED for; these name what happened, and the two
// differ on every host where no browser resolves — a container in "auto" mode
// imports, and did so years before the setting existed.
const (
	RefreshMechanismBrowser       = "browser"
	RefreshMechanismProfileImport = "profile-import"
)

// RefreshResult reports a refresh pass PER PLATFORM.
//
// The whole-service bool that RefreshCookies still returns cannot answer the
// question its callers actually ask. A recovery attempt is triggered FOR a
// platform, and a healthy sibling made a dead YouTube look recovered: the
// field log for 2026-08-20 03:40:01 reads "YouTube auth verification failed
// after refresh" and "auto-cookie recovery succeeded platform=youtube" three
// lines apart. Both platforms were already computed at that point; only the
// signature threw them away.
type RefreshResult struct {
	// Ran is false when the pass declined before doing any work at all —
	// setup in progress, a refresh already in flight, nothing to refresh.
	// Both verdicts are RefreshUnknown whenever this is false.
	Ran     bool
	YouTube RefreshVerdict
	Twitch  RefreshVerdict

	// Renewed reports whether THIS pass produced the credentials it verified,
	// as opposed to finding the previous ones still alive — the independent
	// 30-minute RefreshService keeps a working session alive whether or not
	// the browser refresh does anything at all.
	//
	// It is a fact about the MECHANISM, and the verdicts are facts about the
	// credentials; the two are independent and must stay that way. A pass can
	// verify both platforms while renewing nothing (a browser that never ran),
	// which is why RefreshCookies still returns true there: authenticated
	// requests will work. What must not happen is a UI reporting that as an
	// unqualified success — the operator pressed a button that runs the
	// browser refresh, and asking whether it worked is the whole reason they
	// pressed it.
	//
	// False on every declined and aborted pass, which renewed nothing by
	// definition. It is not a platform statement: the Firefox verdict is
	// browserLaunchActed's screenshot check everywhere, and the drain only adds
	// errBrowserDrainTimeout where a count exists (a Job Object on Windows, a
	// process group on Linux since the process-group arc; darwin and the
	// fallback build have none). It therefore means "not confirmed", never
	// "the browser failed" — see the wording note on browserLaunchActed.
	Renewed bool

	// YouTubeStored / TwitchStored report whether Moombox held ANY auth
	// cookies for that platform when the verdict was reached. This is
	// PRESENCE, not liveness, and it must never be used to decide whether
	// requests will work — that is what the verdict is for, and a stored
	// cookie that does not work is worth nothing.
	//
	// Its one legitimate use is choosing WORDING. RefreshFailed covers "the
	// credentials were rejected" and "there are no credentials at all"
	// alike, correctly, because neither will authenticate a request; but
	// telling an operator to replace dead cookies for a platform they never
	// configured names a cause that did not happen. The same AND-with-
	// presence guards three claims elsewhere in this file (needsRelogin, the
	// "manual re-login required" Warn, and the `failed` list, whose comment
	// reads "if a user only signed in to YouTube, they should not see
	// 'Twitch needs re-login'").
	//
	// False for both platforms on any pass that did not reach verification —
	// a declined or aborted pass never looked at the jar. That pairs with a
	// RefreshUnknown verdict, which asserts nothing either way.
	YouTubeStored bool
	TwitchStored  bool

	// Mechanism is which cookie source this pass actually used —
	// RefreshMechanismBrowser or RefreshMechanismProfileImport — or "" when it
	// stopped before choosing one. Every exit above the importedFromProfile
	// decision is "": stopped service, setup in flight, refresh in flight, and
	// the three no-source errors (no browser and no profile; launches disabled
	// and no profile; profile not found). The one decline BELOW it — the
	// browser branch's empty-jar gate — carries "browser": the path was chosen
	// before it declined.
	//
	// WORDING ONLY, exactly the rule YouTubeStored / TwitchStored carry above.
	// Nothing may branch a decision on it: whether the credentials work is what
	// the verdicts are for, and whether this pass produced them is Renewed.
	// What it answers is the question every post-flight sentence was getting
	// wrong — "Browser cookie refresh successful" after an import that launched
	// nothing (Arc 12c arc-close F2).
	//
	// "" is not a fourth state to render. Both surfaces fall back to
	// cookies.acquisition for the sentence's subject when it is empty, which is
	// also what an OLDER binary's payload degrades to, since it carries no
	// `mechanism` key at all — the same additive rule `ran` and `verdict` set.
	Mechanism string
}

// Verdict returns the verdict for a platform key ("youtube" / "twitch"),
// matched case-insensitively.
//
// An unrecognised key — including the empty string — returns RefreshUnknown,
// never RefreshFailed. Callers turn RefreshFailed into "your cookies are
// dead, recordings will fail"; firing that off a typo'd or absent platform
// string would be an unearned assertion sourced from a programming error.
func (r RefreshResult) Verdict(platform string) RefreshVerdict {
	switch strings.ToLower(platform) {
	case "youtube":
		return r.YouTube
	case "twitch":
		return r.Twitch
	default:
		return RefreshUnknown
	}
}

// HasCredentials reports whether Moombox held any auth cookies for the named
// platform when this pass reached its verdict. Unrecognised keys — including
// the empty string — report false.
//
// Presence, not liveness: see the field comment on RefreshResult. Use it only
// to choose how a Verdict is WORDED, never in place of one.
func (r RefreshResult) HasCredentials(platform string) bool {
	switch strings.ToLower(platform) {
	case "youtube":
		return r.YouTubeStored
	case "twitch":
		return r.TwitchStored
	default:
		return false
	}
}

// Overall folds the per-platform verdicts into the single verdict a
// whole-service caller can render. It is the one place that fold happens.
//
//   - RefreshOK: at least one platform is conclusively authenticated, so
//     authenticated work is possible.
//   - RefreshFailed: no platform verified AND at least one was conclusively
//     found unauthenticated. Conclusive, and the only branch entitled to
//     report a verification failure.
//   - RefreshUnknown: nothing conclusive either way. Says nothing about the
//     credentials — most of the ways to get here leave a perfectly healthy
//     session.
//
// Unknown covers two very different events and callers MUST NOT word them the
// same: a pass that declined before doing any work, and a pass that ran and
// could not find out. Ran draws exactly that line — see its field comment, and
// cookieRefreshReportFor in cmd/moombox/services.go for the vocabulary.
func (r RefreshResult) Overall() RefreshVerdict {
	switch {
	case r.YouTube == RefreshOK || r.Twitch == RefreshOK:
		return RefreshOK
	case r.YouTube == RefreshFailed || r.Twitch == RefreshFailed:
		return RefreshFailed
	default:
		return RefreshUnknown
	}
}

// AnyVerified is the whole-service bool RefreshCookies has always returned:
// at least one platform is conclusively authenticated.
//
// Exported because the Web and TUI wirings each hand-rolled this same OR over
// the two verdict fields, which is two copies of a rule that has to agree.
// Derived from Overall so there is a single fold rather than a second one that
// can drift from it.
func (r RefreshResult) AnyVerified() bool {
	return r.Overall() == RefreshOK
}

// RefreshDeclinedCauses names every way a refresh pass can decline with a NIL
// error IN FRONT OF A READER, for UI copy that has to explain a decline
// without naming a cause it cannot know.
//
// Three named, four in the code, and the gap is deliberate — read this before
// adding a fifth. The named three are the two slot conflicts at the top of
// RefreshCookiesDetailed (setup in progress, refresh already in flight) and the
// empty refreshPlatforms() gate. The unnamed fourth is the `stopped` latch,
// which sits above all of them and declines once Stop() has been called.
//
// The latch is left out because this constant is operator-facing copy, rendered
// by a worker log line, a TUI toast and a Web toast, and a decline caused by
// Stop() has no reader: the only way to reach it is a "Refresh now" that raced
// process shutdown, and by the time the toast would render the service it
// describes is gone. Naming it would put a cause nobody can act on in front of
// every operator who ever hits one of the three real ones.
//
// So the invariant is exhaustiveness over the declines a RUNNING service can
// produce, not over every refreshDeclined() return. A new decline reachable
// before Stop() must be added here; one reachable only after it must not.
//
// The other two refreshDeclined() returns — no browser and no profile, profile
// not found — both carry an error, so every caller has already branched away
// before it reaches this text.
//
// Exported because three surfaces render it (the worker's log via
// cookieRefreshReportFor, the TUI's R F feedback, and the Web toast) and they
// had already begun to drift — "cookies to refresh" against "cookies worth
// refreshing" — with nothing to catch it. The Go callers share this constant;
// the Web copy is pinned against it by a test, since app.js cannot import it.
const RefreshDeclinedCauses = "a setup or another refresh is already in flight, " +
	"or no platform has cookies worth refreshing"

// refreshDeclined is the result of a pass that did no work: it says nothing
// about either platform, because it never looked. See RefreshDeclinedCauses for
// the ways to get here with a nil error.
func refreshDeclined() RefreshResult { return RefreshResult{} }

// refreshAborted is the result of a pass that started work and stopped before
// verification. It ran, but it still learned nothing about either platform —
// an extraction or write failure says nothing about whether the credentials
// on disk are alive.
func refreshAborted() RefreshResult { return RefreshResult{Ran: true} }

// verdictOf projects one platform's internal verification state onto the
// exported enum. The mapping is one-to-one by construction; there is no state
// that "sort of" verified.
func verdictOf(p platformAuth) RefreshVerdict {
	switch p.state {
	case verifyOK:
		return RefreshOK
	case verifyFailed:
		return RefreshFailed
	default:
		return RefreshUnknown
	}
}
