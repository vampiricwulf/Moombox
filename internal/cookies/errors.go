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
)
