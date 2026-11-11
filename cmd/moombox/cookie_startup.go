package main

import (
	"context"
	"time"
)

// cookieStartup is what run() read from the config, once and under the store
// lock, for the cookie startup sequence.
type cookieStartup struct {
	// HasFile: a cookie file is configured, so there is a first check to run.
	// Without one run() has already called SkipStartupCheck.
	HasFile bool
	// AutoEnabled and RefreshInterval: the browser refresh timer's whole
	// start condition (cookies.auto_enabled and an interval above zero).
	AutoEnabled     bool
	RefreshInterval time.Duration
}

// cookieShutdownWait is how long shutdown waits, after it has stopped both
// cookie services, for the cookie startup goroutine to return.
const cookieShutdownWait = 2 * time.Second

// startCookieSequence runs the cookie startup sequence on ONE goroutine and
// returns at once; the channel it returns is closed when that goroutine ends.
//
// Owner ruling (amends R-H2): neither UI waits for the first cookie check.
// run() calls this after the web server's bind has resolved and before
// runTUI, so the dashboard and the TUI are both up while the check runs and
// both show "checking cookies" (cookies.AuthStatus.Unchecked) until it
// answers. Downloads and monitors started before it, as they always have.
//
// The order is the one run() had in line, and it is why this is one goroutine
// and not three:
//
//  1. The first check (RefreshService.Start), only with a cookie file
//     configured. Start returns when the first check of the process has
//     FINISHED. That holds when a dashboard recheck won the single-flight
//     ahead of it too: Start then waits for that pass (AwaitPass) where it
//     used to return at once.
//  2. The profile seed: one browser-free import out of the configured browser
//     profile, at most once per boot, on an install that has no cookies to
//     lose. UNCONDITIONAL, and deliberately not under the flag below. It once
//     lived inside the periodic goroutine, which coupled it to
//     cookies.auto_enabled for no reason: the flag governs a REPEATING read of
//     a profile that nothing changes between ticks, and a boot is the one
//     moment a mounted profile plausibly did change. Its safety condition is
//     the cookie file, and that lives in AutoCookieService.decideStartupSeed:
//     no browser, an importable profile, and no cookies.txt worth protecting.
//  3. The headless-browser refresh timer. cookies.auto_enabled IS this timer,
//     so the flag is the whole start condition, with an interval that means
//     something. It does not os.Stat the browser profile directory first:
//     completing setup is what creates that directory, so a boot-time probe
//     gave an operator who turned the setting on and then ran setup no timer
//     until the next restart. periodicRefreshHasSource asks per tick.
//
// Shutdown. The steps run under ctx, and a run that has ended skips the ones
// not yet started. Both services refuse a start that comes after their Stop
// (RefreshService.Start checks its stop latch, AutoCookieService's two
// starters go through startBackground), and a Stop during the first check
// cuts the check short, so no ticker or seed is left running whichever side
// gets there first. shutdown waits for the returned channel, bounded by
// cookieShutdownWait, after it has stopped both services.
//
// Panics. The goroutine carries the inline recover every goroutine must
// (CLAUDE.md, Panic recovery), and each step has its own as well, so a panic
// in one step costs that step and the later ones still start. A first check
// that panics inside its pass is recovered in Start and ends the "checking"
// state through the single-flight's release.
func startCookieSequence(ctx context.Context, start cookieStartup,
	cookieRefresh interface{ Start(context.Context) },
	autoCookieSvc interface {
		StartProfileSeed(context.Context)
		StartPeriodicRefresh(context.Context, time.Duration)
	},
	log interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Error("cookie startup sequence panic", "panic", r)
			}
		}()
		step := func(name string, fn func()) {
			defer func() {
				if r := recover(); r != nil {
					log.Error("cookie startup step panic", "step", name, "panic", r)
				}
			}()
			fn()
		}

		if start.HasFile {
			step("first check", func() { cookieRefresh.Start(ctx) })
		}
		if ctx.Err() != nil {
			return
		}
		step("profile seed", func() { autoCookieSvc.StartProfileSeed(ctx) })
		if ctx.Err() != nil {
			return
		}
		if start.AutoEnabled && start.RefreshInterval > 0 {
			step("refresh timer", func() { autoCookieSvc.StartPeriodicRefresh(ctx, start.RefreshInterval) })
		}
	}()
	return done
}

// awaitCookieStartup waits up to d for the cookie startup goroutine to
// return, and reports whether it had. A nil channel (run() never reached the
// sequence) counts as returned.
func awaitCookieStartup(done <-chan struct{}, d time.Duration) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
