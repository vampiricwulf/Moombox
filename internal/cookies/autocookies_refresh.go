package cookies

// autocookies_refresh.go — the headless-browser refresh pass: RefreshCookies /
// RefreshCookiesDetailed and their unexported bodies.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies/dpapi"
)

// RefreshCookies performs a headless browser visit to refresh cookies and
// reports whether ANY platform ended up authenticated.
//
// Kept as a thin wrapper over RefreshCookiesDetailed for callers whose question
// really is whole-service ("can we do authenticated work at all?"). It once had
// four: the startup seed, the periodic tick, the Settings "refresh now" button
// and the TUI's equivalent. All four have since moved to the detailed form, for
// two different reasons — the manual pair need to tell "this pass renewed the
// credentials" from "the old ones still work", and the two AUTOMATIC ones need
// Ran, which this projection discards, to decide whether anything was written
// and an auth re-check is owed (see OnPassCompleted). Nothing in production
// calls this today; the projection is kept because it is the honest answer to
// the whole-service question and the tests that ask it are the ones pinning
// that it does not drift.
//
// Callers acting ON BEHALF of one platform must use RefreshCookiesDetailed
// instead — see RefreshResult.
func (s *AutoCookieService) RefreshCookies(ctx context.Context) (bool, error) {
	return s.refreshCookies(ctx, gateApplies)
}

func (s *AutoCookieService) refreshCookies(ctx context.Context, policy browserGatePolicy) (bool, error) {
	result, err := s.refreshCookiesDetailed(ctx, policy)
	return result.AnyVerified(), err
}

// RefreshCookiesDetailed performs a headless browser visit to refresh cookies
// and reports the outcome per platform.
//
// The exported form always honours cookies.auto_enabled. The periodic timer is
// the one exception and calls refreshCookiesDetailed directly — see
// browserGatePolicy.
//
// FOUR callers outside this package, and the count matters because getting it
// wrong hid a real question once already (a review wrote "three" and the missing
// one was the only automatic caller):
//
//	internal/web/routes/cookies.go   the dashboard's shift+click, and the
//	                                 Settings page's profile-import button
//	cmd/moombox/tui_wiring.go        the TUI's R F chord
//	cmd/moombox/services.go          the download worker's auth-failure retry
//	cmd/moombox/monitor_callbacks.go the monitor's recovery attempt — PASSED AS
//	                                 A METHOD VALUE (s.autoCookieSvc.Refresh-
//	                                 CookiesDetailed, handed to
//	                                 handleRecoveryNeeded), which is why it is
//	                                 easy to miss: it is not a call expression,
//	                                 so a structural search for call sites walks
//	                                 straight past it. TestRefreshCookiesDetailed-
//	                                 CallersAreEnumerated matches references
//	                                 rather than calls for exactly that reason.
//
// The last two are AUTOMATIC and deliberately do NOT consult
// automaticImportGuard, so on a browserless host they import over an existing
// cookies.txt. That is correct and is not an oversight — see the guard's doc
// for why, and the comment at the monitor_callbacks.go call site.
func (s *AutoCookieService) RefreshCookiesDetailed(ctx context.Context) (RefreshResult, error) {
	return s.refreshCookiesDetailed(ctx, gateApplies)
}

func (s *AutoCookieService) refreshCookiesDetailed(ctx context.Context, policy browserGatePolicy) (out RefreshResult, retErr error) {
	// ONE stamp for eighteen returns.
	//
	// Mechanism has to be true of every exit — eight aborts, seven declines
	// and three verdicts — and threading it through each return literal is
	// exactly how the nineteenth one gets added without it. The named result
	// plus this defer make the stamp structural instead: a return site added
	// later carries it whether or not its author knew the field existed.
	//
	// It starts empty and is set only where the path is actually chosen, at the
	// importedFromProfile decision below, so a pass that declined above that
	// point reports "" — the honest answer, and the one both surfaces know how
	// to fall back from. The one decline below that point — the browser
	// branch's empty-jar gate — carries "browser", because the branch WAS
	// chosen.
	//
	// NO LOCK: the closure touches the named result and nothing else, and it is
	// registered before the first s.mu.Lock() so it runs LAST, after every path
	// has already released the mutex.
	mechanism := ""
	defer func() { out.Mechanism = mechanism }()

	s.mu.Lock()
	// A stopped service must not launch a browser. Declined rather than
	// errored, matching the two gates below it: nothing was examined, so the
	// pass has no verdict to report and no failure to blame on the
	// credentials. Stop() latches, so unlike those two this never clears.
	if s.stopped {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — service stopped")
		return refreshDeclined(), nil
	}
	s.reapAbandonedSetupLocked()
	// GRACE-GATED, NOT LIVE-GATED, and the difference is a data-loss bug.
	// setupInProgressLocked stays true for a setup whose browser has exited but
	// whose FinishSetup may still be running, so a headless refresh cannot
	// launch a second browser at the same profile directory while that finish
	// is reading it and merging into cookies.txt. Two writers into one cookie
	// store is the class of bug the previous arc was entirely about. Weakening
	// this to setupBrowserLiveLocked() would buy at most 60 seconds of
	// refresh availability and re-open it.
	if s.setupInProgressLocked() {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — setup in progress")
		return refreshDeclined(), nil
	}
	if s.refreshCmd != nil {
		s.mu.Unlock()
		s.logger.Debug("skipping cookie refresh — already refreshing")
		return refreshDeclined(), nil
	}
	s.refreshCmd = &exec.Cmd{} // sentinel to claim slot
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.refreshCmd = nil
		s.mu.Unlock()
	}()

	// refreshBrowser, not resolvedBrowser: this is the one site where
	// cookies.auto_enabled reaches a refresh, and it reaches it by answering
	// nil rather than by refusing the pass. See BrowserLaunchAllowed.
	browser := s.refreshBrowser(policy)

	if _, err := statProfileDir(s.profileDir); os.IsNotExist(err) {
		// Neither a browser to drive nor a profile to import from: the
		// install genuinely has no cookie source, so keep the historical
		// answer and the "install a supported browser" UI copy that hangs
		// off it.
		if browser == nil {
			// Two ways to arrive with no browser, and they do not share a
			// remedy. The gate above can drop a browser that is installed and
			// working, and "no browser found" would send that operator to
			// install a second copy of one they already have — the unearned
			// cause this arc exists to stop. Same sentinel either way, because
			// every consumer's branch is genuinely the same (there is no
			// browser to use); different sentence, because the reader's next
			// action is not.
			if s.browserLaunchBlocked(policy) {
				disabled := fmt.Errorf(
					"cookies.auto_enabled is false so no headless browser was launched, and there is no "+
						"browser profile at %s to import from (%w for this pass)",
					s.profileDir, ErrNoBrowserFound)
				s.setError(disabled.Error())
				return refreshDeclined(), disabled
			}
			// Wrapped rather than bare, and symmetrically with the arm above,
			// because the Web route renders this message verbatim now — the
			// static sentence it used to substitute ("no supported browser
			// installed") is a claim only ONE of these two arms can support.
			// Both now say which of the two cookie sources was missing and why.
			missing := fmt.Errorf("%w, and there is no browser profile at %s to import from",
				ErrNoBrowserFound, s.profileDir)
			s.setError("no browser found for refresh, and no browser profile to import from")
			return refreshDeclined(), missing
		}
		s.setError("browser profile not found — run setup first")
		return refreshDeclined(), fmt.Errorf("run setup first: %w", ErrProfileNotFound)
	}

	// importedFromProfile selects the browser-free path: no browser is
	// installed (a container, or a headless host) or none may be launched
	// (cookies.auto_enabled is off), but the configured profile directory is
	// present and may hold a readable cookies.sqlite.
	// Before this branch existed, RefreshCookies bailed on `browser == nil`
	// BEFORE it ever looked at the profile — so a perfectly readable
	// mounted profile was refused on a technicality.
	//
	// The disabled case is the same shape and lands here for the same reason:
	// an operator who hand-updates their browser profile presses R F to have it
	// read, and launching nothing is precisely what they want.
	//
	// THE THIRD WAY IN is cookies.acquisition = "profile", and it is the only
	// one that does not depend on the host. A Windows desktop with Firefox
	// installed resolves a browser on every pass, so before this the read-only
	// import was unreachable there — the operator could not ask to have their
	// REAL signed-in profile read instead of a managed one driven headlessly.
	// "auto" leaves the rule exactly as it was.
	importedFromProfile := browser == nil
	if s.resolvedAcquisition() == AcquisitionProfile {
		importedFromProfile = true
	}
	// The one place the mechanism is known. Everything above this line declined
	// without choosing; everything below it ran the branch named here, and the
	// defer at the top carries the answer out of whichever exit is taken.
	if importedFromProfile {
		mechanism = RefreshMechanismProfileImport
	} else {
		mechanism = RefreshMechanismBrowser
	}

	var netscapeCookies string
	var err error
	// browserActed answers the question this function used to get wrong: did
	// the browser we launched actually DO anything?
	//
	// Success here is measured against cookies.txt, which the independent
	// 30-minute RefreshService keeps alive regardless — so a refresh whose
	// browser was killed mid-load, or never started, still verified and still
	// logged "cookie refresh succeeded".
	//
	// It starts TRUE and only the browser branch below can clear it. That is
	// the scoping, not an oversight: the import path and the empty-profile
	// fallback never launch a browser, so there is no browser whose inaction
	// could be detected, and gating them on this would make every
	// containerised profile import report a refresh that never renews —
	// permanently, on every restart.
	browserActed := true
	// emptyBrowserProfile records that a browser refresh read a profile with
	// no relevant cookies in it. Not fatal on its own (see below), but it is
	// the explanation the operator needs if auth then fails to verify.
	emptyBrowserProfile := false

	if importedFromProfile {
		// No refreshPlatforms() gate here. That gate asks "does the jar
		// already hold cookies worth re-fetching?", which is the right
		// question for a browser refresh and the wrong one for an import:
		// seeding a container that has no cookies.txt yet is the primary
		// use case.
		netscapeCookies, err = s.importProfileCookies()
		if err != nil {
			s.setError(err.Error())
			s.logger.Warn("browser-profile cookie import failed", "profile_dir", s.profileDir, "err", err)
			return refreshAborted(), err
		}
	} else {
		if len(s.refreshPlatforms()) == 0 {
			s.logger.Debug("skipping cookie refresh — no platforms have cookies")
			return refreshDeclined(), nil
		}

		s.logger.Info("refreshing cookies via " + browser.Type)

		if isFirefoxBased(browser.Type) {
			netscapeCookies, browserActed, err = s.refreshFirefox(ctx, browser)
		} else {
			// Chromium needs no screenshot: the navigations are driven over
			// CDP, so each one reports its own outcome and refreshChromium
			// ANDs them, exactly as refreshFirefox ANDs its per-launch
			// verdicts. It is a weaker signal than the Firefox screenshot —
			// "no navigation reported a transport failure", see
			// refreshChromium — but it is the one this path can produce.
			//
			// The READ error alone is not it, which is what this used to take
			// it for. That error comes from cdpGetCookiesAsNetscape, and the
			// read is satisfied by a profile the previous session already
			// populated — so every navigation could have failed and the pass
			// would still have claimed it renewed the credentials. Both halves
			// are required.
			var navigated bool
			netscapeCookies, navigated, err = refreshChromiumCookies(s, ctx, browser)
			browserActed = err == nil && navigated
		}
	}

	// DPAPI fallback: if the CDP path failed and the user has opted
	// in, try reading cookies directly from their real Chromium-family
	// profile via CryptUnprotectData. Skipped for Firefox-based
	// browsers — Firefox uses cookies.sqlite (no DPAPI involved) and
	// already has its own SQLite-direct path. DECISIONS #6.
	if err != nil && browser != nil && s.DpapiFallback && !isFirefoxBased(browser.Type) {
		s.logger.Warn("CDP refresh failed; attempting DPAPI fallback", "cdp_err", err)
		// H7: the configured browser OVERRIDE, not the resolved/auto-detected
		// `browser` above — an operator who explicitly named a browser in
		// settings gets DPAPI restricted to it; auto-detect leaves every
		// Chromium-family profile as a candidate for dpapiExtractAsNetscape's
		// own selection (it picks exactly one, it never merges). Gated by
		// browserOverrideConfigured — the SAME predicate resolvedBrowser
		// uses — so a browser_type set with no path (Finding 3, Arc 8 fix
		// round 1: reachable from the TUI's free-text field) is "no
		// override" here too, not a hard filter on a half-configured value.
		var cfgBrowserType string
		if s.ConfiguredBrowserOverride != nil {
			if path, btype := s.ConfiguredBrowserOverride(); browserOverrideConfigured(path, btype) {
				cfgBrowserType = btype
			}
		}
		fallbackCookies, fallbackErr := dpapiExtractAsNetscape(s.logger, cfgBrowserType)
		if fallbackErr != nil {
			// ErrNotSupported is not a failure: the fallback does not exist on
			// this platform and has already said so once, at Debug. Everything
			// else is a real attempt that did not work.
			if !errors.Is(fallbackErr, dpapi.ErrNotSupported) {
				s.logger.Warn("DPAPI fallback also failed; surfacing original CDP error",
					"dpapi_err", fallbackErr)
			}
			// fall through with the original CDP err
		} else {
			s.logger.Info("DPAPI fallback succeeded; using user's signed-in browser cookies")
			netscapeCookies = fallbackCookies
			err = nil
			// The headless launch failed, but this pass did bring fresh
			// credential material in — read out of the user's real,
			// signed-in browser profile rather than recycled from
			// cookies.txt. That is what browserActed asks about, so the
			// failed launch does not veto it.
			browserActed = true
		}
	}

	// An empty profile is a hard error for the IMPORT path, where it means the
	// read is broken. That path has already returned above on any error, so the
	// !importedFromProfile guard is belt-and-braces — it also keeps browser
	// non-nil for the log line.
	//
	// On the BROWSER path it has a mundane explanation — a browser set to clear
	// cookies on close leaves the profile empty every time — and before this
	// package that produced a no-op merge and a refresh that succeeded off the
	// still-good cookies.txt. Failing here instead would fire "Cookie
	// Auto-Refresh Failed — recordings will fail" at a user whose recordings are
	// fine.
	//
	// So: contribute nothing, let verification below decide, and remember the
	// fact so it is named if the existing cookies turn out to be dead too. That
	// keeps the desktop behaviour while refusing to let a browser that silently
	// stopped saving cookies masquerade as an ordinary expiry.
	//
	// BOTH FAMILIES. This used to sit inside the Firefox arm of the if/else
	// above, so the identical Chromium state — cdpGetCookiesAsNetscape can now
	// tell an empty profile from a failed read and says ErrNoCookiesInProfile
	// for it — aborted the refresh instead.
	//
	// Placed AFTER the DPAPI fallback rather than immediately after the if/else,
	// and that ordering is load-bearing on Windows: an empty headless profile is
	// exactly when reading the user's real signed-in profile is worth trying, so
	// the downgrade must not consume the error before the fallback sees it.
	// Firefox is unaffected either way — the fallback skips that family.
	//
	// browserActed is deliberately left as the branches set it. This pass
	// contributed no credentials, so whatever verifies below was already on
	// disk, and Renewed must not claim otherwise.
	if !importedFromProfile && errors.Is(err, ErrNoCookiesInProfile) {
		s.logger.Warn("browser refresh produced no cookies — falling back to the existing cookies.txt",
			"browser", browser.Type, "profile_dir", s.profileDir, "err", err)
		emptyBrowserProfile = true
		netscapeCookies, err = "", nil
	}

	if err != nil {
		s.setError(err.Error())
		return refreshAborted(), err
	}

	// Merge with existing cookies using temp file + rename for atomicity.
	// previousCookies is kept verbatim so an import that turns out to have
	// damaged a platform can hand that platform's rows back untouched.
	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		// Sets, exactly as FinishSetup's twin does (autocookies_setup.go): the
		// policy on lastError says every exit that returns an error from a
		// cookie pass records what it concluded, and these three returned while
		// leaving the field both dashboards render blank — so a refresh failing
		// every 30 minutes looked like a healthy install with stale cookies.
		s.setError("could not create the directory for cookies.txt: " + err.Error())
		return refreshAborted(), err
	}
	var previousCookies string
	existingData, readErr := readCookieFile(s.cookiePath)
	switch {
	case readErr == nil:
		if len(existingData) > 0 {
			previousCookies = string(existingData)
		}
	case errors.Is(readErr, fs.ErrNotExist):
		// No cookies.txt yet — nothing to merge or protect via rollback.
	default:
		// This has to abort BEFORE previousCookies is used for anything:
		// it gates both the merge below and, on BOTH paths, whether the
		// pre-write verification that makes rollback possible even runs
		// at all (`previousCookies != ""` further down). Silently
		// treating a transient read failure as "no existing file" would
		// leave previousCookies empty, which both disables that rollback
		// AND lets the write below replace cookies.txt with only the
		// newly-fetched cookies — losing whatever the other platform
		// had. Abort instead: don't merge, don't write, don't touch the
		// rollback gate.
		//
		// Wraps ErrCookieFileUnreadable so callers can tell this apart from
		// every other refresh failure — see the sentinel's doc comment for
		// why that distinction has to survive to the operator: the file was
		// deliberately left untouched, and must not be the thing they are
		// told to replace.
		mergeErr := fmt.Errorf("%w — refusing to merge or overwrite an existing cookies.txt that could not be read (%w)",
			ErrCookieFileUnreadable, readErr)
		s.setError(mergeErr.Error())
		s.logger.Error("cookie refresh: aborting rather than overwrite cookies.txt after a read failure",
			"path", s.cookiePath, "err", readErr)
		return refreshAborted(), mergeErr
	}

	// Verify BEFORE overwriting, on BOTH paths. Rolling back a regression is
	// impossible without knowing what worked beforehand, and "the file had
	// cookies" is not the same as "those cookies worked".
	//
	// The browser path was excluded until A2's narrowed form (ruling Q13).
	// What the two paths DO with this snapshot still differs — see the policy
	// selection below — but the snapshot itself is the same question.
	//
	// Skipped when there is nothing to protect, so the common container case
	// (no cookies.txt yet) costs no extra round trips. On the browser path the
	// cost is two verification round trips per pass on an install that already
	// has credentials: the price of not silently destroying them.
	//
	// snapshotPlatformAuth is shared with ImportCookies, which is the same
	// question about the same file. Its `protected` half is DISCARDED here on
	// purpose: this path has always gone on to use a snapshot taken over a jar
	// that could not be reloaded, and the extraction changes nothing about
	// that. The import path, which has no such history, gates on it.
	pre := map[string]platformAuth{}
	if previousCookies != "" {
		pre, _ = s.snapshotPlatformAuth(ctx, "refresh")
	}

	// What we believed we held going in, and what this pass actually brought
	// back. Both are read BEFORE the write, because afterwards the only thing
	// left to look at is the merged result — and the whole point below is to
	// tell "we lost what we had" apart from "there was never anything here".
	//
	// The jar is the right source for the first pair: it is what
	// refreshPlatforms() gated on, and the disagreement between the jar
	// (which ignores expiry) and mergeCookieFiles (which prunes on it) is
	// precisely how a refresh can end up writing an empty file.
	//
	// Same predicate as refreshPlatforms() and checkPlatformAuth, so `lost`
	// below compares like with like. It also makes the sentence
	// cookiesLostMessage prints true: "nothing is left to authenticate with"
	// describes a platform that went from some credential to none, which is
	// what these now measure. Under the complete-set predicate a full set
	// degrading to a partial one was reported as a total loss (false), and a
	// partial set vanishing entirely was not reported at all (silent).
	hadYTAuth := s.jar.HasAnyYouTubeAuthCookie()
	hadTWAuth := s.jar.HasAnyTwitchAuthCookie()
	fetchedRows := countNetscapeCookieRows(netscapeCookies)
	// fetchedNoCredential is the state the outcome switch at the bottom of this
	// function could not name: rows came back, and NOT ONE of them is a session
	// credential. A browser profile that is signed out, or one set to clear
	// cookies on exit and re-seeded with YSC/VISITOR_INFO1_LIVE by the
	// navigation this pass just made, lands here every time.
	//
	// Read as either of the two nearest existing cases it was wrong: "the
	// browser profile contained no cookies" is FALSE (rows came back — that arm
	// is emptyBrowserProfile, which is only set when the read produced nothing
	// at all), and "auth verification failed — manual re-login required" is true
	// but says nothing an operator can act on, because the thing to fix is that
	// the browser is not signed in rather than that Moombox's check failed.
	//
	// A NEW FLAG, never a redefinition of fetchedRows. Overloading `fetchedRows
	// == 0` would put this state inside the counter, whose deliberate
	// over-counting is load-bearing for the import guard and is mutation-pinned;
	// and reusing emptyBrowserProfile would make "the profile was empty" mean
	// two different things one line apart.
	//
	// Measured on what THIS PASS FETCHED, before the merge below folds the
	// previous cookies.txt in. After the merge the answer would be about the
	// file rather than about the browser, and the file's credentials are exactly
	// the ones this state is not a statement about.
	fetchedNoCredential := fetchedRows > 0 && !netscapeCookiesHoldACredential(netscapeCookies)

	if previousCookies != "" {
		fetchedCookies := netscapeCookies
		netscapeCookies = mergeCookieFiles(previousCookies, netscapeCookies)
		// ONE line, for the one prune outcome that leaves a credential pair
		// half alive with nothing else in the process able to see it. See
		// twitchLoginPrunedFromMerge; it names no value and no account.
		if twitchLoginPrunedFromMerge(previousCookies, fetchedCookies, netscapeCookies) {
			s.logger.Warn("the Twitch login row expired and was pruned while the auth-token survived — " +
				"chat will capture anonymously (no subscriber-only messages, no badges) until a new login row arrives")
		}
	}
	if err := writeCookieFile(s.cookiePath, []byte(netscapeCookies), 0o600); err != nil {
		// Same wording and the same short Docker hint as FinishSetup's write
		// exit: the write ends in a rename, and a rename cannot replace a
		// single-file bind mount. This goes to a status line both dashboards
		// render, so it stays one sentence.
		s.setError("could not write cookies.txt: " + err.Error() +
			" — if this is Docker, mount the data directory rather than cookies.txt itself")
		return refreshAborted(), err
	}

	// Reload jar
	if err := s.jar.Load(s.cookiePath); err != nil {
		// The worst of the three to leave silent: the cookies were fetched AND
		// written, so the file on disk is fine and nothing about the state looks
		// wrong — the pass simply reported nothing.
		s.setError("cookies.txt was written but could not be loaded: " + err.Error())
		return refreshAborted(), err
	}

	// Verify auth via API callbacks (matches TypeScript refreshCookies behavior)
	postYT, postTW := s.checkPlatformAuth(ctx)

	// Roll back, per platform, a write that made that platform worse.
	//
	// A mounted profile can be STALE, and mergeCookieFiles lets the newly
	// written value win by name+domain+path — so a dead Twitch token in the
	// profile overwrites a working one on disk. Judging the write as a WHOLE
	// hides exactly that: a healthy YouTube result masks the Twitch loss, the
	// refresh reports success, and the working credential is gone. The
	// startup one-shot would then repeat it on every restart.
	//
	// BOTH paths do this now, under DIFFERENT policies (ruling Q13, A2's
	// narrowed form). The import path takes both arms — a regression and an
	// inconclusive check on a platform that had credentials. The browser path
	// takes the regression arm ONLY: it has just re-fetched from the live
	// site, so a check that could not reach the network afterwards is evidence
	// about the network, and restoring on it would discard a fresher set on
	// every blip. See platformsToRestoreOnRegression for the full
	// argument.
	//
	// This comment used to say the browser path needed no rollback at all,
	// because its cookies "cannot be staler than what was on disk". True of
	// their AGE; silent on whether they authenticate — a profile the browser
	// signed out of hands back rows that win the merge and do not work.
	//
	// importCheck is kept under its own name because the rollback branch below
	// REPLACES postYT/postTW with a re-verification of what was restored. The
	// question "why did we reject the new cookies" can only be answered by the
	// check that rejected them. See rollbackWasInconclusive.
	importCheck := map[string]platformAuth{"youtube": postYT, "twitch": postTW}
	restorePolicy := platformsToRestoreOnRegression
	if importedFromProfile {
		restorePolicy = platformsToRestore
	}
	var restoredPlatforms []string
	if restore := restorePolicy(pre, importCheck); len(restore) > 0 {
		for _, platform := range []string{"youtube", "twitch"} {
			if restore[platform] {
				restoredPlatforms = append(restoredPlatforms, platform)
			}
		}
		source := "the browser refresh"
		if importedFromProfile {
			source = "the imported profile cookies"
		}
		s.logger.Warn("the newly written cookies did not hold up — restoring the previous credentials for those platforms",
			"source", source, "platforms", strings.Join(restoredPlatforms, ","), "profile_dir", s.profileDir)

		restored := restorePlatformRows(netscapeCookies, previousCookies, restore)

		// A rollback that does not land must not be reported as one. Both
		// failures below leave the process describing a jar that is not what
		// is on disk, and the status built at the bottom of this function
		// would go on to say "kept the previous cookies for X" — while the
		// rejected import is the file the next download actually uses. Worse,
		// a sibling platform that verified would carry the whole call to
		// "refresh succeeded".
		//
		// So they end the refresh instead, with a message describing the
		// state that really exists. Failing the call matches how every other
		// write failure in this function is handled.
		//
		// The write, the reload and the two recordings are
		// restorePreviousCookies, shared with the paste import; this path keeps
		// its own wording — it names the browser profile, and it carries no
		// import sentinel — and its own short returned errors.
		if fail := s.restorePreviousCookies(restored, restoredPlatforms, rollbackMessages{
			writeHead: "the browser profile did not verify for " + strings.Join(restoredPlatforms, " + ") +
				", and Moombox could not restore the previous cookies",
			// The FILE is correct here; the running process is not. Saying
			// "kept the previous cookies" would be true of the disk and false
			// of everything using the jar until the next successful load.
			reloadHead: "restored the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
				" after the browser profile did not verify, but reloading them failed",
			writeLog: "could not restore the previous cookies.txt",
		}); fail != nil {
			// Explicit on BOTH stages rather than "write, else reload": the
			// two short wrappers are what this path's callers match on, and a
			// third stage added to rollbackStage must not silently inherit
			// the reload wording.
			switch fail.stage {
			case rollbackWriteFailed:
				return refreshAborted(), fmt.Errorf("restore previous cookies: %w", fail.cause)
			case rollbackReloadFailed:
				return refreshAborted(), fmt.Errorf("reload cookie jar after restore: %w", fail.cause)
			default:
				return refreshAborted(), fmt.Errorf("restore previous cookies (unknown rollback stage %d): %w", fail.stage, fail.cause)
			}
		}

		// Re-verify the file we actually kept. Without this, the status
		// below would describe the DISCARDED merged jar and flag a
		// re-login for credentials that were restored and never
		// re-checked — an instruction a container operator cannot even
		// act on.
		postYT, postTW = s.checkPlatformAuth(ctx)
	}

	ytAuth := postYT.ok()
	twAuth := postTW.ok()

	// The per-platform answer, fixed here because postYT/postTW are final from
	// this point on (the rollback branch above is the last thing that can
	// re-verify). Every remaining exit reports THIS — the three of them differ
	// in what they log and record, not in what they concluded.
	//
	// hasCookies travels WITH the verdict rather than being re-read from the
	// jar, so the two can never describe different moments: a rollback
	// re-verifies, and a presence bit sampled before it would belong to the
	// discarded import. renewed rides along for the same reason — the gates
	// further down consume it, and a UI caller that has to re-derive it would
	// be re-deriving it from information it does not have.
	//
	// renewed says whether this pass actually produced the credentials it is
	// about to be judged on, as opposed to finding the previous ones still
	// alive. The import and empty-profile paths always did (they read a
	// profile); the browser path only did if the browser ran.
	//
	// Written as an explicit `importedFromProfile ||` rather than leaning on
	// browserActed's initialiser so the scoping is visible at the point of
	// use: an earlier draft of this change gated success on the profile
	// database's mtime and would have made every containerised import report
	// failure forever.
	renewed := importedFromProfile || browserActed
	result := RefreshResult{
		Ran:           true,
		Renewed:       renewed,
		YouTube:       verdictOf(postYT),
		YouTubeStored: postYT.hasCookies,
		Twitch:        verdictOf(postTW),
		TwitchStored:  postTW.hasCookies,
	}

	// Update re-login flags based on verification results. Only a CONCLUSIVE
	// failure flags a re-login: an unreachable network told us nothing about
	// the credentials, and sending the user to sign in again over a blip is
	// both wrong and, in a container, impossible to act on.
	//
	// Taken from the verdicts rather than re-read from the jar, as the comment
	// above result says: the presence bit has to describe the same moment and
	// the same question as the state it is paired with. Re-reading it strictly
	// here silently dropped the half-cleared platform out of both `failed` and
	// the re-login flag, so a session YouTube had conclusively rejected
	// produced no prompt and no targeted message.
	ytHasCookies := postYT.hasCookies
	twHasCookies := postTW.hasCookies

	// Platforms that HAD auth cookies going into this refresh and do not
	// have them coming out. This is per platform on purpose: the jar ignores
	// expiry while mergeCookieFiles prunes on it, so one platform's rows can
	// vanish while the other's survive — and a sibling that verifies would
	// otherwise carry the whole call to "refresh succeeded" over a
	// credential that just disappeared. A platform the import legitimately
	// REPLACED still has auth in the reloaded jar, and the rollback above
	// puts previous rows back before this point, so neither of those reads
	// as a loss.
	var lost []string
	if hadYTAuth && !ytHasCookies {
		lost = append(lost, "YouTube")
	}
	if hadTWAuth && !twHasCookies {
		lost = append(lost, "Twitch")
	}

	s.mu.Lock()
	if postYT.state == verifyFailed && ytHasCookies {
		s.needsRelogin["youtube"] = true
	}
	if postTW.state == verifyFailed && twHasCookies {
		s.needsRelogin["twitch"] = true
	}
	if ytAuth {
		s.needsRelogin["youtube"] = false
	}
	if twAuth {
		s.needsRelogin["twitch"] = false
	}
	s.mu.Unlock()

	if postYT.state == verifyFailed && ytHasCookies {
		s.logger.Warn("YouTube auth verification failed after refresh — manual re-login required")
	}
	if postTW.state == verifyFailed && twHasCookies {
		s.logger.Warn("Twitch auth verification failed after refresh — manual re-login required")
	}

	// Consider refresh successful if any platform verified
	if ytAuth || twAuth {
		now := time.Now()
		// One platform verifying does not license clearing the status over
		// another platform's credentials disappearing. Success here is
		// partial, and the part that was lost is the part nobody would
		// otherwise find out about.
		lostMsg := ""
		if len(lost) > 0 {
			lostMsg = cookiesLostMessage(lost)
		}
		s.mu.Lock()
		if renewed {
			// Withheld when the browser did nothing. lastRefresh is what
			// shouldSkipPeriodicRefresh consults and what the settings page
			// prints as "Last refresh"; stamping it for a pass that renewed
			// nothing would both suppress the NEXT attempt (interval/2) and
			// tell the user their credentials are fresher than they are.
			s.lastRefresh = &now
		}
		switch {
		case lostMsg != "":
			// A loss is something THIS pass observed, so it is recorded
			// whether or not the pass renewed anything.
			s.lastError = &lostMsg
		case renewed:
			s.lastError = nil
		default:
			// Withheld for the same reason lastRefresh is. Clearing lastError
			// is an assertion — "whatever was wrong is not wrong any more" —
			// and this pass has no basis for it. What it established is that
			// the credentials ON DISK verify; what it could not establish is
			// that the refresh mechanism works, which is exactly what a
			// previously recorded error may have been about ("the browser
			// profile contained no cookies to refresh from — check whether the
			// browser is clearing cookies on exit"). Retracting that report
			// off a pass whose browser did nothing is how a twice-broken
			// refresh presents a clean bill of health.
			//
			// Nothing is set here either: the credentials verify, so the
			// Settings error field — which reads as "your recordings will
			// fail" — would be alarming a user whose recordings are fine.
			// The honest signals for this case are a lastRefresh that stays
			// stale and the Warn logged below.
		}
		s.mu.Unlock()

		// Persist LastRefresh to the sidecar (audit cookies.md #48).
		persistedPlatforms := []string{}
		if ytAuth {
			persistedPlatforms = append(persistedPlatforms, "youtube")
		}
		if twAuth {
			persistedPlatforms = append(persistedPlatforms, "twitch")
		}
		// Same reason as lastRefresh above: the sidecar is the copy that
		// survives a restart, so writing a timestamp for a refresh that never
		// ran makes the lie durable.
		if renewed {
			if metaErr := SaveMeta(s.cookiePath, CookieMeta{
				LastRefresh: now,
				Platforms:   persistedPlatforms,
			}); metaErr != nil && s.logger != nil {
				s.logger.Warn("could not persist cookies.meta.json", "err", metaErr)
			}
		}

		var verified []string
		if ytAuth {
			verified = append(verified, "YouTube")
		}
		if twAuth {
			verified = append(verified, "Twitch")
		}
		switch {
		case lostMsg != "":
			s.logger.Warn("cookie refresh verified one platform and lost another",
				"verified", strings.Join(verified, " + "), "lost", strings.Join(lost, ","), "detail", lostMsg)
		case !renewed:
			// The credentials on disk verify, but nothing here established that
			// THIS pass produced them: they may be the same ones that were
			// already there, kept alive by the independent 30-minute
			// RefreshService. Calling that "cookie refresh succeeded" is the
			// claim this branch exists to stop making — it is how a
			// Firefox-family refresh that did nothing at all reported success
			// on every run for the life of the feature.
			//
			// The wording stops at "could not confirm" on purpose. Naming a
			// mechanism ("the browser never completed a page load") would be
			// wrong in the partial case — with two platforms, one browser can
			// genuinely have rendered while the other did not, and the verdict
			// is an AND — and unprovable wherever there is no Job Object to
			// drain. Replacing one unearned claim with its mirror image is not
			// the fix.
			//
			// Still `return true`: the caller asked whether authenticated
			// requests will work, and they will. What changes is that nothing
			// here credits this pass for it.
			s.logger.Warn("cookies still verify, but this pass could not confirm the browser refreshed the profile",
				"verified", strings.Join(verified, " + "))
		default:
			// The horizons ride THIS line and not the per-launch
			// "<browser> <platform> refresh completed" lines: this SUCCESS arm
			// is the completion point that carries them, so a horizon logged
			// inside refreshFirefox would describe the credentials the pass
			// started with — which the startup line already reported. The
			// other two arms above are downstream of the same writeCookieFile
			// and s.jar.Load but carry no horizon at all: "cookie refresh
			// verified one platform and lost another" and "cookies still
			// verify, but this pass could not confirm the browser refreshed
			// the profile" — a refresh landing on either logs none. One site
			// covers both browser families (refreshChromium has no Info
			// completion line of its own) and the import path. Read against
			// the boot line's identical three fields, this is the settling
			// observation for whether the periodic twitch.tv navigation
			// renews auth-token. Timestamps only; see HorizonLogFields.
			refreshFields := append([]any{"verified", strings.Join(verified, " + ")}, s.jar.HorizonLogFields()...)
			s.logger.Info("cookie refresh succeeded", refreshFields...)
		}
		return result, nil
	}

	// Neither platform verified. Build a targeted message naming only the
	// platforms that actually had cookies worth verifying — if a user only
	// signed in to YouTube, they should not see "Twitch needs re-login".
	var failed []string
	if ytHasCookies {
		failed = append(failed, "YouTube")
	}
	if twHasCookies {
		failed = append(failed, "Twitch")
	}
	if len(failed) == 0 {
		// Execution is PAST writeFileAtomic, so whatever sits in cookies.txt
		// now is what this pass produced — and it authenticates neither
		// platform. That is three different situations wearing one face, and
		// clearing lastError for all of them (the old behaviour) is only
		// right for the third.
		switch {
		case len(lost) > 0:
			// We HAD credentials and now the file has none. The usual cause
			// is the disagreement noted above: the jar ignores expiry, the
			// merge prunes on it, so every stored row can be dropped by a
			// refresh that thought it had something to refresh. Whatever the
			// cause, an empty credential file must never be reported as a
			// clean no-op.
			errMsg := cookiesLostMessage(lost)
			s.setError(errMsg)
			s.logger.Warn("cookie refresh left no auth cookies on disk",
				"platforms", strings.Join(lost, ","), "detail", errMsg)
		case fetchedRows > 0:
			// Nothing was lost — there was nothing to lose — but this pass
			// did write cookies, and none of them authenticate anything. The
			// container case: a mounted profile that is not signed in, or
			// one whose saved logins have since lapsed. Saying nothing here
			// makes a useless mount look like a working one.
			errMsg := "cookies were read but none of them authenticate YouTube or Twitch — " +
				"the browser profile is not signed in to either platform, or its saved logins have expired"
			s.setError(errMsg)
			s.logger.Warn("cookie refresh produced no auth cookies", "rows", fetchedRows, "detail", errMsg)
		default:
			// Genuinely nothing to do and nothing lost (e.g. first run before
			// setup). The refresh completed cleanly; there was just nothing
			// to refresh yet.
			//
			// NO ROUTE TO HERE HAS BEEN FOUND, as of Arc 8 Task 12a. Reaching it
			// needs `failed` and `lost` both empty with fetchedRows == 0, i.e.
			// a pass that fetched nothing while neither platform had a
			// credential going in. The only path that fetches nothing is the
			// ErrNoCookiesInProfile downgrade above, which lives on the browser
			// branch — and that branch is gated on refreshPlatforms() being
			// non-empty, which is the same pair of loose predicates hadYTAuth /
			// hadTWAuth are read from. The import branch has no such gate but
			// cannot fetch nothing: importProfileCookies raises
			// ErrNoCookiesInProfile rather than returning an empty blob.
			//
			// Kept, with the derivation written down, rather than deleted:
			// "I could not find a route" is not "there is none", and the arm is
			// the right behaviour for the state it describes. If a future change
			// does open a route, note that this is a CLEAR — see the write
			// policy on the lastError field for why clears are the dangerous
			// half — and it may only stay correct while the state really is
			// "nothing happened and nothing was lost".
			s.logger.Debug("cookie refresh completed with no cookies to verify")
			s.mu.Lock()
			s.lastError = nil
			s.mu.Unlock()
		}
		return result, nil
	}
	// Say what actually happened. "Manual re-login required" is the right
	// advice only when the credentials were conclusively rejected; when the
	// checks could not reach the network, or when we kept the previous
	// credentials rather than the import, that message sends the operator
	// after the wrong problem.
	//
	// A rollback and an inconclusive check can be true at once — in fact the
	// pure-network case is exactly that, since a check that cannot complete
	// is itself a reason to keep the previous credentials. Blaming the
	// profile there would send a container operator off to re-export one
	// that is perfectly fine, which is the misattribution verifyUnknown
	// exists to prevent. So that combination gets its own message carrying
	// both facts: what we kept, and why.
	//
	// Two different questions, so two different sources. `inconclusive`
	// describes the credentials in force NOW, which after a rollback are the
	// restored ones — that is the right input for the no-rollback branches
	// below. rollbackWasInconclusive describes why the IMPORT was rejected, and
	// only the check that rejected it can answer that: the re-verification
	// overwrote postYT/postTW, so reading them would attribute the rollback to
	// a check performed afterwards on different cookies. When the restored
	// credentials then verify conclusively-false — the ordinary outcome once a
	// dead-but-configured platform can reach arm 2 at all — that misattribution
	// prints "the mounted browser profile did not verify" about a profile that
	// was never evaluated.
	inconclusive := postYT.state == verifyUnknown || postTW.state == verifyUnknown
	rollbackWasInconclusive := false
	for _, platform := range restoredPlatforms {
		if importCheck[platform].state == verifyUnknown {
			rollbackWasInconclusive = true
		}
	}
	// rollbackHedge is the (network?) hedge's replacement across whichever
	// restored platforms are inconclusive. combinedInconclusiveHedge folds
	// them to ONE hedge when they agree (the common case: at most one
	// platform is usually restored at all) and to a per-platform breakdown
	// when they do not — see its doc for why collapsing a disagreement to
	// one hedge would assert a cause about a platform the code knows is
	// false. Reviewer round 1 finding 1 caught the previous AND/OR
	// tie-break doing exactly that for this arm's twin below; this arm
	// shares the same fix rather than getting its own tie-break.
	rollbackHedge, _ := combinedInconclusiveHedge(restoredPlatforms, importCheck)
	// inconclusiveHedge/inconclusiveAgree is rollbackHedge's twin for the
	// no-rollback arm below, over postYT/postTW instead of importCheck.
	inconclusiveHedge, inconclusiveAgree := combinedInconclusiveHedge(
		[]string{"youtube", "twitch"},
		map[string]platformAuth{"youtube": postYT, "twitch": postTW})
	var errMsg string
	switch {
	case len(restoredPlatforms) > 0 && rollbackWasInconclusive:
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — " + rollbackHedge + ", so the imported profile was not accepted"
	case len(restoredPlatforms) > 0:
		rejected := "the refreshed browser cookies"
		if importedFromProfile {
			rejected = "the mounted browser profile"
		}
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — " + rejected + " did not verify"
	case inconclusive && inconclusiveAgree:
		// Single hedge, one sentence — the shape every existing test and
		// every single-platform (the overwhelmingly common) inconclusive
		// check already expects.
		errMsg = strings.Join(failed, " + ") + " auth could not be verified — " + inconclusiveHedge
	case inconclusive:
		// The platforms disagree on why, so the "<platforms> auth could not
		// be verified —" lead-in is dropped rather than paired with a
		// per-platform breakdown that already names each platform: the
		// combined hedge IS the message.
		errMsg = inconclusiveHedge
	case emptyBrowserProfile:
		errMsg = strings.Join(failed, " + ") + " auth verification failed, and the browser profile contained " +
			"no cookies to refresh from — check whether the browser is clearing cookies on exit"
	case fetchedNoCredential:
		// PLACED HERE, immediately above default, and the position is the
		// constraint rather than an aesthetic choice: this case carves its state
		// out of `default` and out of nothing else.
		//
		// It cannot overlap emptyBrowserProfile above — that arm is only set on
		// a read that produced no text at all, so fetchedRows is 0 there and
		// this flag is false. It is kept BELOW the two inconclusive arms
		// deliberately: "the browser is signed out" is a strong claim about the
		// profile, and a check that could not reach the site has not earned it.
		// Moving it up would silently change what those arms cover, which is
		// exactly what this case was forbidden from doing.
		//
		// NAMES THE PLATFORMS, like every sibling arm. It did not, and was the
		// only arm in this switch that did not: with two platforms configured
		// and one of them failing, a message that opens on the browser leaves
		// the operator to guess which session the verdict is about — and this
		// arm is reachable in exactly that mixed state.
		errMsg = fmt.Sprintf("%s auth verification failed, and the browser profile returned %d "+
			"cookies but none of them is a session credential — the browser is signed out",
			strings.Join(failed, " + "), fetchedRows)
	default:
		errMsg = strings.Join(failed, " + ") + " auth verification failed — manual re-login required"
	}
	// A platform can be LOST while another is merely rejected, and the
	// rejection message would otherwise be the only thing said — naming the
	// surviving platform's problem while the other one's credentials
	// silently left the file.
	if len(lost) > 0 {
		errMsg = cookiesLostMessage(lost) + ". " + errMsg
	}
	s.setError(errMsg)
	s.logger.Warn("refresh completed but auth verification failed",
		"platforms", strings.Join(failed, ","), "lost", strings.Join(lost, ","), "detail", errMsg)
	return result, nil
}
