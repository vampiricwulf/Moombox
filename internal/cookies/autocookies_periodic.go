package cookies

// autocookies_periodic.go — the periodic scheduler: whether a tick should run
// at all, the one-shot startup profile seed, and the repeating refresh timer.

import (
	"context"
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// shouldSkipPeriodicRefresh decides whether the periodic ticker should fire.
// Two skip conditions:
//
//  1. No active jobs (existing) — when nothing is downloading or live, an
//     auth-token refresh isn't urgent. Headless Chrome launch is ~1-5s of
//     CPU + ~150MB RAM; not worth it for an idle session. With
//     HasActiveJobs nil this condition never skips.
//  2. Recent successful refresh (audit reports/cookies.md #23) — if we
//     refreshed within `interval/2`, the next tick is too close to be
//     useful. Trips when the user just-now used "refresh now" or a job-
//     level recovery path triggered a refresh between ticks.
//
// Either condition skips the tick.
func (s *AutoCookieService) shouldSkipPeriodicRefresh(interval time.Duration) bool {
	if s.HasActiveJobs != nil && !s.HasActiveJobs() {
		return true
	}
	s.mu.Lock()
	last := s.lastRefresh
	s.mu.Unlock()
	if last != nil && time.Since(*last) < interval/2 {
		return true
	}
	return false
}

// periodicRefreshHasSource reports whether a periodic tick has anything to work
// with: the browser profile directory must exist. Both refresh mechanisms need
// it — the headless browser is launched against it, and the browser-free import
// reads out of it — so RefreshCookiesDetailed returns without doing anything
// useful when it is absent.
//
// This used to be answered ONCE, by an os.Stat in main.go, before the periodic
// goroutine was allowed to start at all. That silently punished the operator it
// was meant to serve: turn cookies.auto_enabled on, complete setup — which is
// what CREATES the directory — and the timer that the setting exists to start
// stayed unstarted until the next restart, with nothing saying so. No setting
// had changed, so even the restart-required labelling never fired.
//
// Asked per tick instead, so a setup completed at runtime is picked up by the
// next one. The other side of that trade is the reason this is a quiet skip
// rather than a pass that fails: on a flag-on install where setup has never
// been run, a real pass would call setError("browser profile not found — run
// setup first") and log a warning on every interval forever, putting a
// permanent error on the settings page for a state the operator has not been
// asked to fix yet.
func (s *AutoCookieService) periodicRefreshHasSource() bool {
	_, err := statProfileDir(s.profileDir)
	return err == nil
}

// notePassCompleted fires OnPassCompleted if one is wired.
//
// A named method rather than an inline nil check so the decision has a seam a
// test can drive: the tick that calls it needs a browser profile, a browser
// and a network, so the branch is otherwise unreachable offline.
func (s *AutoCookieService) notePassCompleted() {
	if s.OnPassCompleted != nil {
		s.OnPassCompleted()
	}
}

// profileImportStartupDelay is how long the browserless startup import waits
// before running. RefreshCookies verifies the imported cookies over the
// network, and firing that the instant the process comes up — before DNS,
// the network stack, or a VPN sidecar is ready — would report a false
// "auth verification failed" and flag a re-login the user does not need.
const profileImportStartupDelay = 15 * time.Second

// StartProfileSeed runs AT MOST ONE browser-free import out of the configured
// browser profile, shortly after start, on an install that has no cookies to
// lose. It returns immediately; the import happens on its own goroutine.
//
// NOT gated on cookies.auto_enabled, and that separation is the point. The flag
// owns the periodic timer — a repeating read of a profile nothing changes
// between ticks, which is why the operator triggers those reads with R F. This
// is not that. It is once per boot, and a boot is the moment a mounted profile
// most plausibly DID change: the container was down while somebody replaced it.
//
// The condition that keeps it safe is the cookie file, not the flag. A cold
// start with no usable cookies.txt has nothing to lose and everything to gain
// from reading the profile once; an install that already has cookies is never
// touched here, because whatever is on disk may be working credentials and R F
// is the way to replace those deliberately. decideStartupSeed owns that call.
func (s *AutoCookieService) StartProfileSeed(ctx context.Context) {
	switch d := s.decideStartupSeed(); d {
	case autoImportOK:
	case autoImportCookieFileUnreadable:
		// The one stand-down that is operator-actionable, and the one that
		// must never be silent — see ErrCookieFileUnreadable. An unreadable
		// file is not an absent one: it may hold working credentials for a
		// platform this process has not looked at, so it is left alone.
		s.logger.Warn("startup browser-profile cookie import stood down — the existing cookies.txt "+
			"could not be read, so it was left untouched rather than imported over. Fix the "+
			"permission or mount problem; nothing here needs replacing.",
			"path", s.cookiePath)
		return
	default:
		s.logger.Debug("startup browser-profile cookie import not applicable", "reason", d.String())
		return
	}

	s.logger.Info("browser-free import path and no cookies to lose — seeding cookies from the configured browser profile",
		"profile_dir", s.profileDir, "cookie_file", s.cookiePath,
		"delay", profileImportStartupDelay.String())

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in startup cookie seed goroutine", "panic", fmt.Sprintf("%v", r))
			}
		}()
		if err := utils.Sleep(ctx, profileImportStartupDelay); err != nil {
			return
		}
		// Asked again, because the whole gate is "there is nothing here to
		// lose" and the wait is long enough for that to stop being true — an
		// interactive setup finishing, or an operator dropping a hand-exported
		// cookies.txt in, both write the file this decision was made about.
		if s.decideStartupSeed() != autoImportOK {
			s.logger.Debug("startup browser-profile cookie import stood down — cookies appeared while it waited")
			return
		}
		seedCtx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
		// RefreshCookies, i.e. gateApplies: this is not the timer, so it has no
		// claim on the exemption. The two policies remain provably identical
		// here — decideStartupSeed reaches this point only when the pass will
		// take the import branch (no browser resolvable, or acquisition =
		// "profile" forcing it), and the gate's only power is to turn a non-nil
		// browser into nil — so gateExempt would buy nothing and would blur
		// what it means.
		//
		// Detailed rather than the RefreshCookies wrapper, which returns
		// AnyVerified() and DISCARDS Ran. ok below is that same bool, so the
		// three log arms are unchanged; Ran is the extra fact, and it is the
		// one that decides whether anything was written.
		result, err := s.refreshCookiesDetailed(seedCtx, gateApplies)
		cancel()
		ok := result.AnyVerified()
		switch {
		case err != nil:
			s.logger.Warn("startup browser-profile cookie import failed", "err", err)
		case ok:
			s.logger.Info("startup browser-profile cookie import succeeded")
		default:
			s.logger.Warn("startup browser-profile cookie import did not authenticate any platform")
		}
		// The second site of the seam, and the one Arc 10 missed. This is the
		// container install's ONLY credential writer — nothing here will ever
		// run a browser — and at boot it lands 15 s after the refresh service
		// took its first status over an empty jar. Without this the seed repairs
		// the credentials, a Twitch job that already went anonymous has marked
		// the platform, and nothing compares the fingerprint, clears that mark
		// or reconnects the live chat session until the 30-minute ticker.
		//
		// Gated on Ran and not on ok, for the reason the tick is: a pass that
		// ran and produced a dead pair moved the fingerprint exactly as a
		// working one did. Below the log chain, also for the tick's reason —
		// the hook may run a full in-process re-check, and the operator should
		// not read the import's verdict half a minute after the re-check's
		// output.
		if result.Ran {
			s.notePassCompleted()
		}
	}()
}

// StartPeriodicRefresh starts a background goroutine that periodically
// refreshes cookies via headless browser visit. When HasActiveJobs is set,
// ticks where it returns false are skipped to avoid spawning a headless
// browser when nothing needs authenticated YouTube/Twitch access.
//
// The one-shot startup import used to live in here, which coupled it to
// cookies.auto_enabled for no reason it could justify. It is StartProfileSeed
// now, and main.go calls that unconditionally.
//
// EVERY pass this goroutine runs is gateExempt, and that is the whole reason
// the policy exists. main.go starts this loop only when cookies.auto_enabled
// was true at boot, so the flag has already been consulted; re-consulting it
// per pass would mean an operator who switched it off without restarting kept
// the timer AND had it quietly change mechanism, importing an unchanged browser
// profile on a schedule. Moombox's answer for a profile the operator updates by
// hand is the manual trigger — R F in the TUI, shift+click on the dashboard —
// because a refresh is only meaningful when something changed the profile, and
// the operator is the only thing that can.
func (s *AutoCookieService) StartPeriodicRefresh(ctx context.Context, interval time.Duration) {
	s.logger.Info("auto-cookie periodic refresh enabled", "interval", interval.String())
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in periodic cookie refresh goroutine", "panic", fmt.Sprintf("%v", r))
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runPeriodicTick(ctx, interval)
			}
		}
	}()
}

// runPeriodicTick runs one tick under its own recover, so a panic costs that
// tick and not the timer: the goroutine's recover above sits outside the loop
// and would end it, stopping the browser refresh for the life of the process.
func (s *AutoCookieService) runPeriodicTick(ctx context.Context, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic in periodic cookie refresh tick", "panic", fmt.Sprintf("%v", r))
		}
	}()
	s.periodicTick(ctx, interval)
}

// periodicTick is one tick of StartPeriodicRefresh's timer.
func (s *AutoCookieService) periodicTick(ctx context.Context, interval time.Duration) {
	if !s.periodicRefreshHasSource() {
		s.logger.Debug("periodic auto-cookie refresh skipped — no browser profile directory yet",
			"profile_dir", s.profileDir)
		return
	}
	// THE RULE, at its second automatic site: a browser-free import
	// runs only when there is no cookies.txt to lose.
	//
	// Scoped to a tick that would BE a browser-free import, and the
	// scope is load-bearing. Refreshing a LIVE cookies.txt through a
	// headless browser is what this timer is for, so a host with a
	// browser must keep doing exactly that. Only the browserless
	// pass is an import, and an import over an existing cookie file
	// is the thing the owner ruled out: nothing between two ticks
	// changes a mounted profile, so it re-reads identical bytes over
	// credentials that may be working — browserless because no
	// browser resolves, or because cookies.acquisition = "profile"
	// makes the pass an import regardless of the host. The second is
	// the desktop case, where the profile IS the operator's real one
	// and a scheduled re-read over live credentials is precisely what
	// this rule refuses.
	//
	// gateExempt to match the pass this tick would actually run —
	// asking with a different policy could answer "browser" here
	// and "no browser" three lines down.
	if s.refreshBrowser(gateExempt) == nil || s.resolvedAcquisition() == AcquisitionProfile {
		if v := s.automaticImportGuard(); v != autoImportOK {
			s.logger.Debug("periodic auto-cookie refresh skipped — a browser-free import "+
				"may only run when there is nothing to lose", "reason", v.String())
			return
		}
	}
	if s.shouldSkipPeriodicRefresh(interval) {
		s.logger.Debug("periodic auto-cookie refresh skipped — no active jobs or recent refresh")
		return
	}
	s.logger.Debug("periodic auto-cookie refresh triggered")
	refreshCtx, cancel := context.WithTimeout(ctx, refreshOverallBudget)
	// Detailed, not the bool wrapper: only the full result carries
	// Ran, and Ran is what decides whether anything was written.
	result, err := s.refreshCookiesDetailed(refreshCtx, gateExempt)
	cancel()
	ok := result.AnyVerified()
	if err != nil {
		s.logger.Warn("periodic auto-cookie refresh failed", "err", err)
	} else if ok {
		// Debug, and deliberately not "succeeded": RefreshCookies
		// has just logged the one line that knows whether this pass
		// RENEWED the credentials or merely found the previous ones
		// still alive — at Info when it did, at Warn when it did
		// not. Repeating "succeeded" here would contradict the
		// second case and put the false claim back a line later.
		s.logger.Debug("periodic auto-cookie refresh tick finished with authenticated cookies on disk")
	}
	// AFTER the verdict above, not before it. The hook may run a
	// full in-process re-check — two validate round-trips, ~30 s at
	// the client timeout — and everything RefreshService.refresh
	// logs on the way lands in between. Firing it first buried this
	// tick's own "failed"/"finished" line half a minute down the log,
	// underneath output about a different pass.
	//
	// Gated on Ran, NOT on success. A pass that ran and failed still
	// rewrote cookies.txt — a browser refresh that produced a
	// new-but-dead pair moves the credential fingerprint exactly as a
	// working one does — so firing on success only would leave the
	// Twitch auth mark keyed to a pair that is no longer on disk. A
	// DECLINED pass (eight refreshDeclined() exits) wrote nothing, so
	// there is nothing to re-read.
	if result.Ran {
		s.notePassCompleted()
	}
}
