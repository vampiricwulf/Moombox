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

	// ErrNoSetupInProgress is returned by FinishSetup or CancelSetup when
	// no StartSetup call is active. HTTP consumers can map to 404.
	ErrNoSetupInProgress = errors.New("no cookie auto-setup in progress")

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
)
