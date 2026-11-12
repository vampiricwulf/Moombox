package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"
)

// webBindBackstop is how long run() waits for the web server to either bind
// or fail before it goes on without an answer. A bind normally answers in
// milliseconds; this only ends the wait on a start so slow that neither
// signal has arrived. It is never read as a failure: a slow but healthy boot
// must not become a startup error.
const webBindBackstop = 30 * time.Second

// webBindOutcome is what awaitWebBind saw first.
type webBindOutcome int

const (
	// webStartReturned: web.Server.Start returned. Its error is the bind (or
	// TLS setup) failure; nil means the server was shut down.
	webStartReturned webBindOutcome = iota
	// webBound: the listener is bound and serving. Start is still running.
	webBound
	// webBindUndecided: the backstop passed with neither answer. Start is
	// still running.
	webBindUndecided
)

// awaitWebBind waits for the web server's bind to resolve: Start's result on
// startErr (it returns only on a failure or a shutdown), or the server's
// Bound signal. It replaces a fixed 500 ms wait that every boot paid in full
// and that a failure arriving later slipped past. The error is meaningful
// only with webStartReturned.
func awaitWebBind(startErr <-chan error, bound <-chan struct{}, backstop time.Duration) (webBindOutcome, error) {
	timer := time.NewTimer(backstop)
	defer timer.Stop()
	select {
	case err := <-startErr:
		return webStartReturned, err
	case <-bound:
		return webBound, nil
	case <-timer.C:
		return webBindUndecided, nil
	}
}

// resolveWebBind is run()'s wait for the web server's bind (awaitWebBind) and
// what each answer means for the boot. webErrCh carries web.Server.Start's
// result and bound is the server's Bound signal. A bind failure in a headless
// run exits with exitCodeStartupError; with a TUI it is logged and reported
// as true, and the boot goes on without the dashboard. A bound server, or
// one that has said nothing by webBindBackstop, reports false, and a later
// error from Start is logged in the background.
func (s *runState) resolveWebBind(webErrCh <-chan error, bound <-chan struct{}) bool {
	log, useTUI := s.log, s.useTUI
	webBindFailed := false
	switch outcome, err := awaitWebBind(webErrCh, bound, webBindBackstop); outcome {
	case webStartReturned:
		if err != nil {
			if !useTUI {
				log.Error("Web server failed to start", slog.String("error", err.Error()))
				// This arm exits without closing the logger, which is what
				// prints the lines still queued for the console.
				log.FlushConsole(consoleBannerFlush)
				fmt.Fprintf(os.Stderr, "\nError: %s\n", err.Error())
				fmt.Fprintln(os.Stderr, "Web dashboard is unavailable.")
				// Deterministic: the port is taken or the bind address is
				// unusable on this host. Never "the update is broken". And
				// no keypress wait: this arm is headless, and a headless run
				// never holds its exit (awaitKeypress).
				os.Exit(exitCodeStartupError)
			}
			log.Warn("Web server failed, continuing in TUI-only mode", slog.String("error", err.Error()))
			webBindFailed = true
		}
	default:
		// The server is bound (or still starting at the backstop): log a
		// later error from Start in the background.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("web error drainer panic", "panic", r)
				}
			}()
			if err := <-webErrCh; err != nil {
				log.Error("Web server error", slog.String("error", err.Error()))
			}
		}()
	}
	return webBindFailed
}
