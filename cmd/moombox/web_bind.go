package main

import "time"

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
