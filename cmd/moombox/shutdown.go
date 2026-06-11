package main

import (
	"fmt"
	"os"
	"time"
)

// shutdown runs the orderly stop sequence after run()'s main event loop
// exits (either via Ctrl-C / SIGTERM, TUI quit, or triggerRestart). Order
// is consumers-first so producers keep firing into live consumers until the
// consumers drain: monitors → worker → notifications → cookie refresh →
// PO-token provider → web server → log/DB unsubscribe → database. A
// 10-second force-exit timer closes rate limiters + the logger and calls
// os.Exit(1) as a backstop — every individual stop is isolated with panic
// recovery so one failing service does not block the others.
//
// Returns true when a restart was requested via s.triggerRestart, so the
// caller (main) can re-invoke run() with the same configPath.
func (s *runState) shutdown() bool {
	s.log.Info("Shutdown signal received, shutting down gracefully...")

	// 10-second force-exit timer. closeLimiters / closeDB / closeLog must run
	// here too because os.Exit skips remaining defers; all are sync.Once-
	// guarded so the concurrently-running deferred cleanup doesn't
	// double-close. The exit CODE must honor restartRequested: this backstop
	// fires routinely (worker stop alone can legitimately take its full 10s
	// while a background segment mux drains), and exiting 1 during an
	// update/config restart makes the launcher terminate instead of
	// respawning — turning a self-update into a daemon outage with the new
	// binary already swapped on disk but never started.
	forceExit := time.AfterFunc(10*time.Second, func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "force-exit handler panic: %v\n", r)
			}
		}()
		s.log.Error("Graceful shutdown timed out, forcing exit")
		s.closeLimiters()
		s.closeDB()  // final WAL checkpoint — committed data survives either way
		s.closeLog() // flush buffered logs before force exit
		if s.restartRequested.Load() {
			os.Exit(exitCodeRestart)
		}
		os.Exit(1)
	})
	defer forceExit.Stop()

	// stopService isolates each individual Stop call with panic recovery +
	// "Stopped X" debug log. Mirrors the TS stopService pattern.
	stopService := func(name string, fn func()) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error(fmt.Sprintf("[Moombox] Error stopping %s: %v", name, r))
			}
		}()
		fn()
		s.log.Debug(fmt.Sprintf("[Moombox] Stopped %s", name))
	}

	// 1. Stop monitors
	stopService("TwitchMonitor", s.twitchMon.Stop)
	stopService("DecapiMonitor", s.decapiMon.Stop)
	stopService("FeedMonitor", s.feedMon.Stop)

	// 2. Stop worker (waits for active downloads to save state)
	stopService("DownloadWorker", s.dlWorker.Stop)

	// 3. Flush in-flight notifications (may have been fired during worker stop)
	stopService("Notifications", s.notifyMgr.Wait)

	// 4. Stop cookie refresh and auto-cookie service
	stopService("CookieRefresh", s.cookieRefresh.Stop)
	stopService("AutoCookies", s.autoCookieSvc.Stop)

	// 5. Cleanup PO token provider
	stopService("PotProvider", s.potProvider.Cleanup)

	// 5b. Stop BotGuard sidecar subprocess (when running). Job Object
	// pinning means the child dies regardless if Moombox is killed
	// hard, but a graceful shutdown lets us drain stdout cleanly and
	// avoid an unsightly EOF warning in the logs.
	if s.bgSidecar != nil {
		stopService("BgSidecar", func() { _ = s.bgSidecar.Stop() })
	}

	// 6. Stop web server
	stopService("WebServer", s.webServer.Stop)

	// 7. Unsubscribe log forwarder and DB event subscribers (nil-safe — the
	// inline wiring that sets these happens after initServices but before the
	// main loop, so in theory they're always populated at this point. Guard
	// anyway so an early-exit shutdown path does not NPE).
	if s.logSub != nil {
		s.log.Unsubscribe(s.logSub)
	}
	if s.unsubWSJobUpdate != nil {
		s.unsubWSJobUpdate()
	}
	if s.unsubWSJobAdded != nil {
		s.unsubWSJobAdded()
	}
	if s.unsubWSJobDeleted != nil {
		s.unsubWSJobDeleted()
	}
	if s.unsubWSTrimsChanged != nil {
		s.unsubWSTrimsChanged()
	}
	if s.unsubWSJobsChange != nil {
		s.unsubWSJobsChange()
	}

	// 8. Flush database (sync.Once-guarded — also fires from defer s.closeDB)
	stopService("Database", s.closeDB)

	if s.restartRequested.Load() {
		s.log.Info("Shutdown complete, restarting...")
	} else {
		s.log.Info("Shutdown complete")
	}

	return s.restartRequested.Load()
}
