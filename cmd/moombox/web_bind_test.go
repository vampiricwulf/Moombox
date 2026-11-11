package main

import (
	"errors"
	"testing"
	"time"
)

// A bind failure reaches the failure arm however late it arrives: run()
// exits headless, or marks the dashboard failed, on it. Under the old fixed
// 500 ms wait a later failure was only logged. The wait is on the signals
// alone, so the test holds the error back on a channel instead of sleeping.
//
// Mutant: return webBindUndecided from awaitWebBind before either signal.
func TestAwaitWebBindWaitsForALateFailure(t *testing.T) {
	startErr := make(chan error, 1)
	want := errors.New("TLS setup: boom")
	type result struct {
		outcome webBindOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := awaitWebBind(startErr, make(chan struct{}), 30*time.Second)
		done <- result{outcome, err}
	}()
	// Nothing has been signalled, so it must still be waiting.
	select {
	case got := <-done:
		t.Fatalf("returned %v, %v before any signal", got.outcome, got.err)
	case <-time.After(50 * time.Millisecond):
	}
	startErr <- want
	select {
	case got := <-done:
		if got.outcome != webStartReturned || !errors.Is(got.err, want) {
			t.Fatalf("outcome %v, err %v; want webStartReturned with the Start error", got.outcome, got.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("awaitWebBind did not return on the Start error")
	}
}

// A bound server ends the wait on the signal, not on the backstop (30 s
// here): a healthy boot no longer pays a fixed delay.
func TestAwaitWebBindReturnsWhenBound(t *testing.T) {
	startErr := make(chan error, 1)
	bound := make(chan struct{})
	close(bound)
	outcome, err := awaitWebBind(startErr, bound, 30*time.Second)
	if outcome != webBound || err != nil {
		t.Fatalf("outcome %v, err %v; want webBound, nil", outcome, err)
	}
}

// Start returning nil (a shutdown before the bind was read) is reported as a
// return with no error, which run() treats as nothing to do.
func TestAwaitWebBindReportsACleanReturn(t *testing.T) {
	startErr := make(chan error, 1)
	startErr <- nil
	outcome, err := awaitWebBind(startErr, make(chan struct{}), 30*time.Second)
	if outcome != webStartReturned || err != nil {
		t.Fatalf("outcome %v, err %v; want webStartReturned, nil", outcome, err)
	}
}

// The backstop is not a failure: with neither signal it reports undecided
// and no error, so run() goes on exactly as it did after the old wait and a
// slow but healthy boot is never turned into a startup error.
func TestAwaitWebBindBackstopIsNotAFailure(t *testing.T) {
	outcome, err := awaitWebBind(make(chan error, 1), make(chan struct{}), 20*time.Millisecond)
	if outcome != webBindUndecided || err != nil {
		t.Fatalf("outcome %v, err %v; want webBindUndecided, nil", outcome, err)
	}
}
