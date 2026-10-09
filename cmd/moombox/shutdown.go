package main

import (
	"fmt"
	"os"
	"time"

	"github.com/vampiricwulf/Moombox/internal/worker"
)

// forceExitAfter is the shutdown backstop. It must outlast the worker's whole
// Stop (worker.StopBudget: the in-flight wait, then mux cancellation and its
// grace) plus the stops ahead of it — the monitors, and the trim service's
// wait for the trims it cancels (up to worker.TrimStopWait). Its clock starts
// first, so a backstop equal to the worker's own wait fired before Stop
// reached CancelMuxes, and FFmpeg outlived the process writing into staging
// the restarted child re-muxes with -y — the very thing owner decision O-E
// cancels muxes to prevent. The margin covers the steps ahead of the worker
// and lets the ones after it start: what the trim wait and the worker leave
// of it is the notification drain's, as little as 1 s. It is not widened for
// the trim wait: the owner's ruling caps a graceful shutdown at 15 s.
const forceExitAfter = worker.StopBudget + 3*time.Second

// shutdown runs the orderly stop sequence after run()'s main event loop
// exits (either via Ctrl-C / SIGTERM, TUI quit, or triggerRestart). Order
// is consumers-first so producers keep firing into live consumers until the
// consumers drain: monitors → trims → worker → notifications → cookie refresh →
// PO-token provider → web server → log/DB unsubscribe → database. A
// force-exit timer (forceExitAfter) closes rate limiters, the database and
// the logger and exits as a backstop — with exitCodeRestart when a restart is
// pending, else 0 (see the timer for why never 1). Every individual stop is
// isolated with panic recovery so one failing service does not block the
// others.
//
// Returns true when a restart was requested via s.triggerRestart, so the
// caller (main) can re-invoke run() with the same configPath.
func (s *runState) shutdown() bool {
	s.log.Info("Shutdown signal received, shutting down gracefully...")

	// Force-exit timer (forceExitAfter). closeLimiters / closeDB / closeLog
	// must run here too because os.Exit skips remaining defers; all are
	// sync.Once-guarded so the concurrently-running deferred cleanup doesn't
	// double-close. The exit CODE must honor restartRequested: this backstop
	// fires routinely (worker stop alone can legitimately take its whole
	// budget while a background segment mux drains), and exiting 1 during an
	// update/config restart makes the launcher terminate instead of
	// respawning — turning a self-update into a daemon outage with the new
	// binary already swapped on disk but never started.
	forceExit := time.AfterFunc(forceExitAfter, func() {
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
		// Exit 0, not 1: this backstop fires routinely on slow-but-USER-
		// INTENDED shutdowns (worker stop legitimately eats its whole budget
		// draining a segment mux). The launcher's crash supervision treats
		// abnormal codes from a long-lived child as crashes and respawns —
		// exiting 1 here would resurrect a daemon the user just quit.
		os.Exit(0)
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

	// Single-attempt notifications from here on. The force-exit above fires
	// forceExitAfter from now and routinely does (a worker stop can
	// legitimately spend most of the window draining a segment mux), so the three-attempt ladder
	// with its 2 s + 5 s backoff cannot finish — an embed emitted during the
	// stop would be retried into a process that is already gone. One attempt
	// is what fits; operations.md documents the cap rather than promising a
	// drain that cannot happen (owner ruling).
	//
	// Through stopService like every other step: it is the only thing in this
	// function with panic recovery, and a shutdown path that can panic its way
	// past the remaining teardown is exactly what that closure exists to
	// prevent — cheap here, since the call is one atomic store.
	stopService("Notifications (single-attempt mode)", s.notifyMgr.BeginShutdown)

	// 1. Stop monitors
	stopService("TwitchMonitor", s.twitchMon.Stop)
	stopService("DecapiMonitor", s.decapiMon.Stop)
	stopService("FeedMonitor", s.feedMon.Stop)

	// 1b. Stop the trim service: cancels the trims it runs — a dashboard's
	// detached one, a TUI's in-process one, a finished job's post-download
	// one — and waits (briefly: a killed FFmpeg exits at once) for each to
	// remove its partial file. The wait, up to worker.TrimStopWait for an
	// FFmpeg that will not die, starts the worker's budget that much later,
	// so it comes out of the force-exit margin the notification flush
	// (step 3) drains in. A stopped trim sends nothing, it did not fail;
	// a stopped post-download trim sends Trim Failed, since nobody asked
	// for it from a dialog.
	if s.trimSvc != nil {
		stopService("TrimService", s.trimSvc.Stop)
	}

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
		// The per-job line router goes with the forwarder it was wired beside
		// (wireLogForwarding): lines logged after this reach the file, the
		// ring and the TUI, not a database that is about to close.
		s.log.SetLineRouter(nil)
		s.log.UnsubscribeLines(s.logSub)
		if s.logSubDone != nil {
			close(s.logSubDone)
		}
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
