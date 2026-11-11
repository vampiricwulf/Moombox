package main

import (
	"errors"
	"testing"
	"time"
)

// A bind failure that arrives later than the old fixed 500 ms wait still
// reaches the failure arm: run() exits headless, or marks the dashboard
// failed, on it. Under the fixed wait it was only logged.
//
// Mutant: put a 500 ms timer back in awaitWebBind (the outcome is
// webBindUndecided).
func TestAwaitWebBindSeesAFailureSlowerThanHalfASecond(t *testing.T) {
	startErr := make(chan error, 1)
	bound := make(chan struct{})
	want := errors.New("TLS setup: boom")
	go func() {
		time.Sleep(700 * time.Millisecond)
		startErr <- want
	}()
	outcome, err := awaitWebBind(startErr, bound, 30*time.Second)
	if outcome != webStartReturned || !errors.Is(err, want) {
		t.Fatalf("outcome %v, err %v; want webStartReturned with the Start error", outcome, err)
	}
}

// A bound server ends the wait at once: a healthy boot no longer pays a
// fixed delay.
func TestAwaitWebBindReturnsAtOnceWhenBound(t *testing.T) {
	startErr := make(chan error, 1)
	bound := make(chan struct{})
	close(bound)
	begin := time.Now()
	outcome, err := awaitWebBind(startErr, bound, 30*time.Second)
	if outcome != webBound || err != nil {
		t.Fatalf("outcome %v, err %v; want webBound, nil", outcome, err)
	}
	if took := time.Since(begin); took > 250*time.Millisecond {
		t.Errorf("a bound server was waited on for %v", took)
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
