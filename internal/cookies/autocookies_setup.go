package cookies

// autocookies_setup.go — the interactive setup flow: launching a browser for
// the user to log in, extracting and saving what it produced, and cancelling
// or abandoning an attempt in flight.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// StartSetup launches a browser for the user to log in.
func (s *AutoCookieService) StartSetup(platform string) error {
	// Validated BEFORE the lock and before the claim: a wrong value must never
	// take the setup slot, and the two callers that are not the HTTP route —
	// the TUI's R L chord and the first-run wizard — reach this and nothing
	// else. The empty string is not an error; it is what a caller that omits
	// the field sends, and it has always meant YouTube.
	//
	// Ahead of the stopped and in-progress gates too, and deliberately: a value
	// this service could never act on is wrong whatever state the service is
	// in, and answering "try again shortly" to it would send the caller back
	// with the same unusable input.
	//
	// The error wraps the value. Safe: platform arrives from a JSON field the
	// operator controls, never from a credential, and both dashboards render
	// sentinel text verbatim.
	if platform == "" {
		platform = "youtube"
	}
	if platform != "youtube" && platform != "twitch" {
		return fmt.Errorf("%w (got %q)", ErrUnsupportedPlatform, platform)
	}

	s.mu.Lock()
	// Checked before the in-progress gate: a stopped service is not "busy",
	// and telling the caller to try again shortly would be wrong — Stop is
	// permanent for this service's lifetime.
	if s.stopped {
		s.mu.Unlock()
		return ErrServiceStopped
	}
	// Before the gate, not after: a setup the user walked away from must not be
	// the reason they cannot start a new one. This is the site the wedge was
	// reported at.
	s.reapAbandonedSetupLocked()
	if s.setupInProgressLocked() {
		s.mu.Unlock()
		return ErrSetupInProgress
	}
	if s.refreshCmd != nil {
		s.mu.Unlock()
		return fmt.Errorf("please try again shortly: %w", ErrRefreshInProgress)
	}
	// Claim the slot inside this critical section (mirrors RefreshCookies'
	// refreshCmd sentinel): browser detection, MkdirAll, and the icacls
	// shell-out below take tens of milliseconds, and a second StartSetup
	// passing the gate in that window would launch a second browser against
	// the same profile and leak the first Job Object. The claim drops when
	// this call returns — by then either setupProcess holds the real
	// process (success) or the attempt failed and the slot must free up.
	//
	// Claim time is the ONE place `cancelled` is cleared, and that is what
	// makes the check at the end of the preparation below mean anything. It
	// used to be cleared in cleanup() as well; cleanup() runs on every setup
	// exit path INCLUDING CancelSetup's own last act, so a complete cancel
	// erased its own flag microseconds after raising it and the check below
	// could only ever catch a cancel that landed in the sliver between
	// CancelSetup's flag write and its cleanup(). Clearing it here and nowhere
	// else means the flag survives until the setup it belongs to consumes it.
	s.setupClaimed = true
	s.cancelled = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.setupClaimed = false
		s.mu.Unlock()
	}()

	browser := s.resolvedBrowser()
	if browser == nil {
		return fmt.Errorf("supported browser (Firefox, Chrome, Brave, Edge, Opera, or Waterfox) required: %w", ErrNoBrowserFound)
	}

	if err := os.MkdirAll(s.profileDir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}
	// Tighten the dir's ACL so only the current user can read its
	// contents (cookies.sqlite, browsing history, login state). Non-
	// Windows hosts get a no-op; Windows shells out to icacls. A
	// failed tightening doesn't fail setup — log and continue.
	// Audit reports/cookies.md #25. Demoted to Debug (matches the config +
	// cookie dir sites): the common failure is ACCESS_DENIED on a dir
	// created under an elevated/admin context, benign on the single-user
	// host this targets — raise the log level to Debug to see the miss.
	if err := utils.ApplyUserOnlyDACL(s.profileDir); err != nil {
		s.logger.Debug("could not restrict profile dir to current user", "path", s.profileDir, "err", err)
	}

	s.mu.Lock()
	if s.cancelled {
		// CancelSetup landed while we were detecting browsers / preparing
		// the profile dir — the slot was already claimed, so the UI showed
		// setup-in-progress and offered Cancel. Honor it instead of
		// launching a browser the user just dismissed.
		s.mu.Unlock()
		return ErrSetupCancelled
	}
	s.setupBrowser = browser
	s.lastError = nil
	s.browserExited = false
	// Reset with browserExited, which it only has meaning alongside. The reap
	// or a cleanup has already zeroed it on every path that reaches here; the
	// pairing is so the two can never be read out of step.
	s.setupRetainedSince = time.Time{}
	// Already normalised and validated at the top of this function — one of
	// "youtube" or "twitch" by here, which is what the loginTarget choice
	// below and every finish branch compare against literally.
	s.targetPlatform = platform
	s.mu.Unlock()

	loginTarget := loginURL
	if platform == "twitch" {
		loginTarget = twitchLoginURL
	}

	if isFirefoxBased(browser.Type) {
		return s.startFirefoxSetup(browser, loginTarget)
	}
	return s.startChromiumSetup(browser, loginTarget)
}

// SetupResult reports what ONE interactive setup concluded, per platform.
//
// Two independent facts per platform, and the reason this type exists is that
// they can disagree:
//
//   - the verdict — what the auth check CONCLUDED, in the same three-way
//     vocabulary a refresh pass uses. RefreshUnknown is the zero value on
//     purpose, so any path that returns without checking cannot accidentally
//     assert health or failure.
//   - Accepted — what the CALLER is told, which is deliberately not the
//     verdict. A sign-in the user just completed is accepted when the site
//     could not answer; see the acceptance predicate in FinishSetupDetailed
//     for why, and why it is not extended to a check that never ran.
//
// Accepted with a verdict of RefreshUnknown is the state this type exists to
// carry: the cookies are saved and in use, and Moombox could not reach the
// site to confirm them. Collapsing that into either neighbour is what tells a
// user whose network blipped mid-check that their sign-in failed.
type SetupResult struct {
	YouTube RefreshVerdict
	Twitch  RefreshVerdict

	YouTubeAccepted bool
	TwitchAccepted  bool

	// Wrote reports that this finish REPLACED cookies.txt, and it is true on
	// one error path as well as on success.
	//
	// It is the setup path's counterpart to RefreshResult.Ran, and it exists
	// for the same caller: whoever has to decide whether the credential
	// fingerprint may have moved and an auth re-check is therefore owed
	// (Arc 10 R4/R5). Every other exit from FinishSetupDetailed leaves the file
	// exactly as it was — no setup in progress, a cancelled one, an unreadable
	// browser profile, the S9 abort that refuses to overwrite a cookies.txt it
	// could not read, a failed MkdirAll, a failed write — but the reload after
	// a SUCCESSFUL write can still fail, and that exit returns an error over a
	// file that has already been replaced. A caller that gates its re-check on
	// `err == nil` misses precisely that case, which is the one where the jar
	// in memory is stale and a re-check would repair it.
	//
	// Deliberately not on the wire: cookieSetupOutcome builds its payload key
	// by key, and this is an internal signal about the file, not a verdict
	// about a platform.
	Wrote bool
}

// FinishSetup extracts cookies from the running browser and saves them, and
// reports only whether each platform was ACCEPTED.
//
// A thin wrapper over FinishSetupDetailed, the same split RefreshCookies /
// RefreshCookiesDetailed already draws — with one honest difference: both of
// the callers that RENDER a finish (the HTTP route and the TUI wizard) had to
// move to the detailed form, so nothing but the tests calls this today. It is
// kept because the projection is the thing worth pinning: the acceptance
// answer must not drift as the verdicts gain consumers, and a caller whose
// question really is "did this platform end up usable" should not have to
// know about verdicts to ask it.
//
// A caller that renders the outcome must NOT use this: the bool pair cannot
// say "saved, but we could not check them", and a UI built on it has to guess.
func (s *AutoCookieService) FinishSetup(ctx context.Context) (ytAuth, twAuth bool, err error) {
	result, err := s.FinishSetupDetailed(ctx)
	return result.YouTubeAccepted, result.TwitchAccepted, err
}

// FinishSetupDetailed extracts cookies from the running browser, saves them,
// and reports both what it accepted and what it concluded.
func (s *AutoCookieService) FinishSetupDetailed(ctx context.Context) (SetupResult, error) {
	s.mu.Lock()
	if s.setupProcess == nil || s.setupBrowser == nil {
		s.mu.Unlock()
		return SetupResult{}, ErrNoSetupInProgress
	}
	if s.cancelled {
		s.mu.Unlock()
		return SetupResult{}, ErrSetupCancelled
	}
	// Restart the retention clock. The grace window exists FOR this call, and
	// measuring it from the browser's exit alone would let a finish that
	// started legitimately inside the window be reaped part-way through: the
	// Chromium path reads s.cdpPort, which cleanupLocked zeroes, so the reap
	// would turn a working extraction into "CDP port not available".
	//
	// Deliberately not a second flag. A "finishing" latch is one more piece of
	// lifecycle state that one path sets and another has to remember to clear —
	// this arc's recurring hazard — whereas moving a timestamp forward cannot
	// leave the slot stuck: the window still expires on its own, and every exit
	// path from here already calls cleanup().
	//
	// Note it stamps even when the browser is still running (browserExited
	// false), where it means nothing yet; the Firefox path is about to close
	// the browser itself, and the wait goroutine will re-stamp on the real exit
	// a moment later.
	s.setupRetainedSince = time.Now()
	browser := s.setupBrowser
	s.mu.Unlock()

	var netscapeCookies string
	var err error

	if isFirefoxBased(browser.Type) {
		s.closeFirefoxGracefully()
		var stats firefoxReadStats
		netscapeCookies, stats, err = readFirefoxCookies(s.profileDir)
		s.logFirefoxReadStats(stats)
	} else {
		netscapeCookies, err = s.extractChromiumCookies()
		s.killSetupProcess()
	}

	// Interactive setup has a legitimate empty state the refresh and
	// profile-import paths do not: the user opened the browser and closed it
	// without signing in. Both read paths report an empty profile as a hard
	// error (a silently empty jar is the bug those errors exist to catch), so
	// translate it back here — the setup dialog should say "no login detected",
	// not fail.
	//
	// OUTSIDE the if/else, not inside the Firefox arm where it used to live.
	// Chromium can produce the same sentinel now that cdpGetCookiesAsNetscape
	// distinguishes "the browser answered and holds nothing" from "the read
	// failed", and while it could not, a Chromium user who never signed in got
	// the route's default 500 "failed to finish setup" for a state that is not
	// a failure at all.
	if errors.Is(err, ErrNoCookiesInProfile) {
		// The error rides into the log line. On the Chromium path it can carry
		// a tier failure that was out-voted by another tier's empty answer, and
		// cdpGetCookiesAsNetscape has no logger of its own — so dropping it here
		// would leave the only evidence that this verdict might be wrong with
		// nowhere to go.
		s.logger.Info("cookie setup finished with an empty profile — no login detected", "detail", err)
		s.setError("no login detected — sign in before finishing setup")
		s.cleanup()
		// RefreshFailed, not RefreshUnknown, and the difference is what the
		// dialog says. This attempt produced no credential of any kind, which
		// is the same conclusion checkPlatformAuth reaches for a platform with
		// nothing on disk — "there is nothing to send, so no request can be
		// authenticated". Unknown would route the UI to its "we could not
		// check" copy, which is the one wrong thing to say about a browser
		// that plainly held no login.
		//
		// It is a statement about THIS setup, not about cookies.txt: this path
		// deliberately merges nothing and reloads nothing, so a working session
		// already on disk is untouched and unexamined. The UI branch it selects
		// says "no login detected" and makes no verification claim.
		return SetupResult{YouTube: RefreshFailed, Twitch: RefreshFailed}, nil
	}

	if err != nil {
		s.setError(err.Error())
		s.cleanup()
		return SetupResult{}, err
	}

	// Merge with existing cookies using temp file + rename for atomicity
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		// THE SET, which this exit and the two below it were missing. The
		// policy on the lastError field says every failure exit records what it
		// concluded, and these three returned an error to the caller while
		// leaving the field both dashboards render blank — so a setup that died
		// on a permission or mount problem showed one sentence in the dialog and
		// then, once the dialog closed, nothing anywhere. The dialog is modal and
		// transient; the status field is where an operator looks afterwards.
		//
		// Ordering is a convention rather than a requirement: cleanup() never
		// clears (see the field's policy and
		// TestCleanupAfterAFailedSetupKeepsLastError), so the set survives it
		// either way. Kept before cleanup to match every other exit here.
		s.setError("could not create the directory for cookies.txt: " + err.Error())
		s.cleanup()
		return SetupResult{}, err
	}

	existingData, readErr := readCookieFile(s.cookiePath)
	switch {
	case readErr == nil:
		if len(existingData) > 0 {
			netscapeCookies = mergeCookieFiles(string(existingData), netscapeCookies)
		}
	case errors.Is(readErr, fs.ErrNotExist):
		// No cookies.txt yet — the normal first-run case. Nothing to
		// merge; proceed with just the freshly extracted cookies exactly
		// as before.
	default:
		// A transient read failure (permission blip, locked file, I/O
		// error) is NOT the same as "no existing file" and must not be
		// treated as nothing to merge — that used to fall straight
		// through to the write below with ONLY the newly extracted
		// cookies, silently replacing a cookies.txt that may hold
		// working credentials for the other platform. Abort instead:
		// don't merge, don't write.
		//
		// Wraps ErrCookieFileUnreadable so callers can tell this apart from
		// every other setup failure — see the sentinel's doc comment for
		// why that distinction has to survive to the operator: the file was
		// deliberately left untouched, and must not be the thing they are
		// told to replace.
		mergeErr := fmt.Errorf("%w — refusing to merge or overwrite an existing cookies.txt that could not be read (%w)",
			ErrCookieFileUnreadable, readErr)
		s.logger.Error("cookie setup: aborting rather than overwrite cookies.txt after a read failure",
			"path", s.cookiePath, "err", readErr)
		s.setError(mergeErr.Error())
		s.cleanup()
		return SetupResult{}, mergeErr
	}

	// Write merged cookies via temp file + rename to prevent corruption on partial failure
	if err := writeFileAtomic(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		// Sets, for the reason spelled out at the MkdirAll exit above. The hint
		// names the one deployment mistake that actually produces this — the
		// write ends in a rename, and a rename cannot replace a single-file bind
		// mount — and is kept SHORT, unlike the paragraph refresh.go attaches to
		// its own failed write: that one goes to a log, this one goes to a status
		// line both dashboards render.
		s.setError("could not write cookies.txt: " + err.Error() +
			" — if this is Docker, mount the data directory rather than cookies.txt itself")
		s.cleanup()
		return SetupResult{}, err
	}

	// Reload jar and verify.
	//
	// Wrote is set from here down: writeFileAtomic above has replaced
	// cookies.txt, so every exit past this point — this error one included —
	// leaves a file whose credential pair may differ from the one the running
	// process last compared. See SetupResult.Wrote.
	if err := s.jar.Load(s.cookiePath); err != nil {
		// Sets, for the reason spelled out at the MkdirAll exit above. This one
		// is the worst of the three to leave silent: the cookies were extracted
		// AND written, so the file on disk is fine and nothing about the state
		// looks wrong — the setup simply reports nothing and the user has no
		// idea whether to run it again.
		s.setError("cookies.txt was written but could not be loaded: " + err.Error())
		s.cleanup()
		return SetupResult{Wrote: true}, err
	}

	// Presence + real API verification, through the same pairing the refresh
	// path uses. This was the last inline copy of it; the nil-callback contract
	// (presence is then the only signal, reported as success with a warning so
	// callers cannot quietly succeed on cookies that are present-but-invalid —
	// audit reports/cookies.md #21) lives in checkPlatformAuth now.
	ytCheck, twCheck := s.checkPlatformAuth(ctx)

	ytAuth := credentialAccepted(ytCheck)
	twAuth := credentialAccepted(twCheck)

	// What gets WRITTEN DOWN, which is a different claim and must be the
	// stricter one. PersistPlatforms unions into cfg.Cookies.Platforms, a set
	// that only ever grows and is never retracted, so accepting a login on an
	// inconclusive check and then recording it as verified turns one rate limit
	// during setup into a durable, permanent assertion that YouTube was
	// verified. Accepting is right; recording the acceptance as a verification
	// is not.
	ytVerified := ytCheck.ok()
	twVerified := twCheck.ok()

	// Inconclusive has to read as inconclusive. The nil-callback branch inside
	// checkPlatformAuth says so; the errored branch said nothing at all, so a
	// 429 during setup was indistinguishable from a clean pass. No cause is
	// named and no error is recorded: the check did not complete, which is not
	// a finding about the credentials, and s.lastError renders in Settings as
	// "your recordings will fail". The two halves get different wording because
	// they carry different advice — one is "try again", the other is "this
	// login is not usable as it stands".
	warnInconclusive := func(platform string, p platformAuth) {
		if s.logger == nil || !p.hasCookies || p.state != verifyUnknown {
			return
		}
		if p.attempted {
			s.logger.Warn(platform + " auth check did not complete during setup — accepting the sign-in without verifying it")
			return
		}
		s.logger.Warn(platform + " auth check was never attempted during setup — the extracted cookies cannot form an authenticated request")
	}
	warnInconclusive("YouTube", ytCheck)
	warnInconclusive("Twitch", twCheck)

	if !ytAuth && !twAuth && s.logger != nil {
		// Not "verification failed": a platform with no auth cookie at all was
		// never verified, and in a single-platform setup one of the two never
		// is.
		s.logger.Warn("cookies extracted, but neither platform is authenticated")
	}

	// Clear the re-login flag for every platform this setup ACCEPTED, not just
	// the ones it verified. The flag means "go and sign in again", the user
	// just did exactly that, and leaving it raised because the confirming
	// request hit a rate limit would nag them about work they have already
	// done. Unlike the persisted set below, this is process-local and the next
	// conclusive check re-raises it.
	s.mu.Lock()
	if ytAuth {
		s.needsRelogin["youtube"] = false
	}
	if twAuth {
		s.needsRelogin["twitch"] = false
	}
	s.mu.Unlock()

	// Persist verified platforms to config so we can detect auth loss after
	// restart (matches TS autoCookies.ts persistPlatforms). VERIFIED, not
	// accepted — see above. Withholding an unverified platform costs no
	// recovery: shouldFireRecovery's first-conclusive-check branch fires for a
	// platform absent from the persisted list, which is precisely the case
	// SetExpectedPlatforms's per-platform everConcluded flags exist to keep
	// working.
	if s.PersistPlatforms != nil {
		s.PersistPlatforms(ytVerified, twVerified)
	}

	now := time.Now()
	s.mu.Lock()
	s.lastRefresh = &now
	s.mu.Unlock()
	s.cleanup()

	// Persist LastRefresh to the sidecar so the next launch doesn't
	// re-run the refresh immediately. Audit reports/cookies.md #48.
	persistedPlatforms := []string{}
	if ytVerified {
		persistedPlatforms = append(persistedPlatforms, "youtube")
	}
	if twVerified {
		persistedPlatforms = append(persistedPlatforms, "twitch")
	}
	if metaErr := SaveMeta(s.cookiePath, CookieMeta{
		LastRefresh: now,
		Platforms:   persistedPlatforms,
	}); metaErr != nil && s.logger != nil {
		s.logger.Warn("could not persist cookies.meta.json", "err", metaErr)
	}

	// "verified" is the word this line uses, so only what verified goes in it.
	var verified []string
	if ytVerified {
		verified = append(verified, "YouTube")
	}
	if twVerified {
		verified = append(verified, "Twitch")
	}
	if len(verified) > 0 {
		s.logger.Info("[AutoCookies] Setup complete — verified: " + strings.Join(verified, " + "))
	}

	// The distinction the three log lines above draw used to end at the log.
	// verdictOf is the same projection the refresh path publishes, so the
	// dialog can render "saved, but we could not check them" in the wording
	// the manual-refresh surfaces already use.
	return SetupResult{
		YouTube:         verdictOf(ytCheck),
		Twitch:          verdictOf(twCheck),
		YouTubeAccepted: ytAuth,
		TwitchAccepted:  twAuth,
		Wrote:           true,
	}, nil
}

// CancelSetup aborts an in-flight setup: it raises the cancelled flag, kills
// the setup browser if one is running, and clears the per-setup state.
//
// "In flight" is `setupProcess != nil || setupClaimed`: there is something in
// the slot to tear down. The claim half is not a technicality — between
// StartSetup's gate and the browser launch there is no process yet, but there
// IS a setup to cancel, and StartSetup's mid-preparation check is what consumes
// the flag this call raises.
//
// THAT IS DELIBERATELY NOT setupInProgressLocked, AND IT IS DELIBERATELY WIDER.
// It used to be the identical expression, and this doc used to say so; the
// setup slot now expires, so the two have to diverge and the direction of the
// divergence is the whole point. Every disjunct of setupInProgressLocked
// implies `setupClaimed || setupProcess != nil`, so this gate is a strict
// SUPERSET: a cancel can never answer "nothing to cancel" while the UI is still
// showing the Cancel button that produced it. The converse — a slot that has
// expired but not yet been reaped reports SetupInProgress false while a cancel
// still succeeds — is the useful direction: that cancel is what tears the dead
// slot down, and answering 404 while leaving state behind would be worse.
//
// Narrowing this to setupInProgressLocked would therefore need a fourth reap
// site to stay coherent, and would trade a cancel that cleans up for one that
// declines. Don't.
//
// Returns ErrNoSetupInProgress when there was nothing to cancel — a second
// cancel, or a cancel with no setup ever started. This used to return nothing
// at all and the route answered `{"success": true}` unconditionally, so
// cancelling twice reported a cancel that never happened.
func (s *AutoCookieService) CancelSetup() error {
	s.mu.Lock()
	// Deliberately a superset of setupInProgressLocked — see
	// setupInProgressLocked in autocookies_setup_slot.go. Not a missed
	// migration.
	if s.setupProcess == nil && !s.setupClaimed {
		s.mu.Unlock()
		return ErrNoSetupInProgress
	}
	s.cancelled = true
	s.mu.Unlock()

	s.killSetupProcess()
	s.cleanup()
	s.logger.Info("auto-cookie setup cancelled")
	return nil
}

// AbandonSetup is what a CLIENT reports when the client itself went away — the
// dashboard tab unloaded. It is NOT CancelSetup, and the difference is the
// point: a deliberate click is consent to close the browser, a tab unload is
// not.
//
// The beacon behind this used to POST /cancel. That was harmless when it was
// written, because a Firefox setup had no Job Object and a cancel could not
// reach the browser. S5 gave it one, so every cancel now closes the window —
// and the setup flow's own instructions send the user AWAY from the dashboard
// tab to go and sign in. Closing the now-idle tab became a remote kill of the
// window they are typing their password into, on the default Windows path.
//
// So this releases the slot WITHOUT killing anything, and it releases only
// where releasing is not itself a kill. The split is not a new rule; it is
// setupBrowserGone's existing `known`, asked one more time:
//
//   - known — the Job Object can be interrogated, so the REAP owns this. It
//     will release the slot on its own correct predicate (the browser actually
//     being gone) and cannot fire while a login is in progress. Releasing here
//     would mean cleanupLocked, which closes that handle, which is
//     KILL_ON_JOB_CLOSE on a live browser. Do nothing.
//   - not known — no job (a failed assign on either platform), a Linux group
//     whose /proc cannot be read, or a platform with no primitive at all
//     (darwin and the fallback build). The reap can never fire there, so this
//     is the only thing that releases the slot; and with nothing able to reach
//     the browser, releasing kills nothing — on Linux the group kill inside
//     cleanupLocked refuses a group it cannot see. Release.
//
// Which is to say plainly: this call is redundant wherever a group or a Job
// Object was adopted — Windows, and Linux since the process-group reap — and
// load-bearing where nothing was: darwin, the fallback build, and any Linux
// launch whose group could not be adopted. The declining arm is not dead code
// on either platform — deleting the check would restore the kill on both.
//
// Two things it deliberately does NOT do. It does not raise `cancelled`,
// because it is not an abort: a StartSetup still preparing a launch is left to
// finish, and the slot it publishes is then governed by the normal rules. And
// it does not call killSetupProcess, which is the whole point.
//
// Returns ErrNoSetupInProgress when there was nothing to release, matching
// CancelSetup so the route can answer both the same way. `released` reports
// whether the slot was actually cleared; the beacon cannot read it, but the
// log line and the tests can.
func (s *AutoCookieService) AbandonSetup() (released bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.setupProcess == nil && !s.setupClaimed {
		return false, ErrNoSetupInProgress
	}
	if _, known := setupBrowserGone(s.setupJob); known {
		s.logger.Debug("client abandoned a cookie setup; leaving it to the reap",
			"platform", s.targetPlatform)
		return false, nil
	}
	if s.setupProcess == nil {
		// A claim in flight and no browser published yet. There is nothing to
		// release, and aborting the launch is not this call's business.
		s.logger.Debug("client abandoned a cookie setup mid-launch; nothing to release yet")
		return false, nil
	}
	s.logger.Info("client abandoned a cookie setup — releasing the slot, leaving the browser alone",
		"platform", s.targetPlatform)
	s.cleanupLocked()
	return true, nil
}
