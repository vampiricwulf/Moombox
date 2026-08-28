package cookies

import "errors"

// Exported sentinels for cookies-package failures that consumers may want
// to discriminate. Producer sites wrap these with `fmt.Errorf("...: %w",
// Err)` so consumers can match via `errors.Is(err, ErrXxx)` while still
// surfacing the contextual prefix in logs / user messages.
//
// Audit cross-cutting C3 follow-on (sentinel migration).
var (
	// ErrNoBrowserFound is returned by StartSetup and RefreshCookies when
	// no supported browser (Firefox, Chrome, Brave, Edge, Opera, Waterfox,
	// LibreWolf, Zen, Vivaldi, Thorium) was detected. Distinguishes a
	// configuration / install issue from a transient failure.
	ErrNoBrowserFound = errors.New("no supported browser found")

	// ErrSetupInProgress is returned by StartSetup when a previous
	// StartSetup call is still active (browser process running, awaiting
	// FinishSetup or CancelSetup). HTTP consumers can map to 409 Conflict.
	ErrSetupInProgress = errors.New("cookie auto-setup already in progress")

	// ErrNoSetupInProgress is returned by FinishSetup or CancelSetup when no
	// StartSetup call is active. HTTP consumers can map to 404.
	//
	// The two producers do NOT share a predicate, and the difference is
	// load-bearing rather than an oversight:
	//
	//   - CancelSetup treats a setup as active while `setupProcess != nil ||
	//     setupClaimed` — anything in the slot to tear down. The claim half
	//     counts because a cancel arriving during StartSetup's preparation has a
	//     real setup to abort even though no process exists yet — StartSetup's
	//     mid-preparation check is what consumes it.
	//   - FinishSetup requires `setupProcess != nil && setupBrowser != nil`. It
	//     has to actually read cookies out of a specific browser, so a claim
	//     with nothing behind it yet gives it nothing to finish.
	//
	// So a claim in flight is a state CancelSetup will act on and FinishSetup
	// reports this sentinel for. That asymmetry is correct — there is something
	// to abort and nothing to harvest — but any caller reasoning about "is a
	// setup in progress" must pick the producer it means rather than assume one
	// answer covers both.
	//
	// A THIRD predicate now exists and neither producer uses it. Since the setup
	// slot acquired a lifetime, `setupInProgressLocked()` — what GetStatus
	// publishes as SetupInProgress, and what StartSetup and RefreshCookies gate
	// on — excludes a slot whose browser is gone and whose grace has expired.
	// CancelSetup's gate is a strict superset of it, so a cancel still succeeds
	// whenever the UI is offering one; it just also succeeds on an expired slot
	// nothing has reaped yet, which is the cancel that cleans it up. This
	// paragraph exists because the sentence it replaced ("the same expression
	// GetStatus publishes as SetupInProgress") went stale the moment that
	// lifetime landed.
	ErrNoSetupInProgress = errors.New("no cookie auto-setup in progress")

	// ErrServiceStopped is returned by StartSetup once Stop has been called.
	// Distinct from ErrSetupInProgress and ErrRefreshInProgress because those
	// two clear on their own and this one never does: the service is finished
	// for the remaining lifetime of the process, so "try again shortly" is the
	// wrong advice. HTTP consumers can map to 503.
	ErrServiceStopped = errors.New("cookie service stopped")

	// ErrSetupCancelled is returned by FinishSetup after CancelSetup has
	// flipped the cancelled flag — the setup state is still partially
	// populated but the user explicitly aborted.
	ErrSetupCancelled = errors.New("cookie auto-setup was cancelled")

	// ErrRefreshInProgress is returned by StartSetup when a periodic
	// (or on-demand) RefreshCookies call holds the refresh slot. HTTP
	// consumers can map to 409 Conflict with "try again shortly" copy.
	ErrRefreshInProgress = errors.New("cookie refresh in progress")

	// ErrProfileNotFound is returned by RefreshCookies when the configured
	// profile directory does not exist (typically: refresh attempted before
	// the user ever ran setup). HTTP consumers can map to 404 + "run setup
	// first" guidance.
	ErrProfileNotFound = errors.New("browser profile not found")

	// --- browser-free profile import (Docker / headless hosts) ---
	//
	// These four describe the realistic ways reading a MOUNTED browser
	// profile fails. They are deliberately distinct: the operator action
	// differs for each (fix the mount, point at the right dir, stop
	// Firefox, re-export the profile), so collapsing them into one
	// "profile import failed" would strip the only useful part of the
	// message.

	// ErrProfileNotADirectory is returned when the configured profile path
	// exists but is a regular file — the classic "bind-mounted cookies.txt
	// onto browser_profile_dir" mistake.
	ErrProfileNotADirectory = errors.New("browser profile path is not a directory")

	// ErrCookieDBNotFound is returned when the profile directory exists but
	// holds no cookies.sqlite — usually the user mounted the Firefox
	// *installation* root or the profiles-parent dir instead of the single
	// profile directory (the one containing prefs.js).
	ErrCookieDBNotFound = errors.New("cookies.sqlite not found in browser profile")

	// ErrCookieDBLocked is returned when cookies.sqlite could not be read
	// because another process holds it. Recent Firefox builds default
	// storage.sqlite.exclusiveLock.enabled to true, which locks out external
	// readers entirely while the browser runs.
	ErrCookieDBLocked = errors.New("cookies.sqlite is locked by another process")

	// ErrCookieDBUnreadable is returned when cookies.sqlite exists and is
	// not locked but cannot be queried — truncated copy, wrong file, or a
	// corrupt database.
	ErrCookieDBUnreadable = errors.New("cookies.sqlite could not be read")

	// ErrNoCookiesInProfile is returned when the cookie database was read
	// successfully but yielded ZERO YouTube/Google/Twitch cookies. This is
	// never treated as a success: it is the exact signature of a profile
	// copied without its -wal sidecar (the copy opens fine and returns
	// nothing) and of a profile snapshotted out from under a live Firefox.
	ErrNoCookiesInProfile = errors.New("no YouTube/Twitch cookies found in browser profile")

	// ErrCookieFileUnreadable is returned by FinishSetup and
	// RefreshCookiesDetailed when the EXISTING cookies.txt could not be read
	// for a reason other than "the file does not exist" — a permission blip,
	// a locked file, an I/O error, a bind-mount hiccup in Docker. Both
	// producers abort on this error BEFORE merging or writing anything: the
	// unreadable file may hold working credentials for a platform this pass
	// never touched, and proceeding as if it were simply absent would
	// silently replace it with only whatever this pass just acquired.
	//
	// Consumers MUST discriminate this from every other refresh/setup
	// failure. The correct instruction is "fix the permission or mount
	// problem and it will retry" — never "replace cookies.txt". Moombox
	// deliberately left the file untouched, and telling the operator to
	// overwrite the one file it just went out of its way not to destroy
	// defeats the entire point of the abort.
	ErrCookieFileUnreadable = errors.New("existing cookies.txt could not be read")

	// ErrAuthCheckNotAttempted marks an auth check that failed BEFORE any
	// request left the process — the jar holds something, but not enough to
	// build a request out of (no cookie header, no SAPISIDHASH). It is still
	// an error rather than (false, nil), for the reason spelled out above
	// checkYouTubeAuth: a structural failure is not a verdict on the
	// credentials, and shouldFireRecovery must not read it as one.
	//
	// The sentinel exists because "we could not reach the site" and "we could
	// not form the question" are both inconclusive and are NOT
	// interchangeable. A caller may reasonably accept a sign-in over the
	// first — a rate limit says nothing about a login the user completed
	// thirty seconds ago — while the second means nothing was ever asked, so
	// there is no answer to accept.
	ErrAuthCheckNotAttempted = errors.New("auth check could not be attempted")
)
