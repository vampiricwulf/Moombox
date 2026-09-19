package routes

import (
	"testing"
	"time"
)

// TestCallDebouncerWindow pins the shape three routes share: the first call is
// allowed and stamps the clock, a call inside the window is refused with the
// remaining wait, and the first call after it is allowed again.
//
// THE MUTANTS:
//   - stamp on a REFUSED call: the window never expires under a held key, and
//     the endpoint is dead forever (assertion 4).
//   - `<=` instead of `<` on the window compare: a call at exactly the boundary
//     is refused with wait 0, which a client reads as "retry immediately" and
//     spins (assertion 3).
//   - never stamp: the debounce does nothing (assertion 2).
func TestCallDebouncerWindow(t *testing.T) {
	d := newCallDebouncer(30 * time.Second)
	t0 := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	if ok, _ := d.allow(t0); !ok {
		t.Fatal("the first call must be allowed")
	}
	if ok, wait := d.allow(t0.Add(5 * time.Second)); ok {
		t.Error("a call 5s into a 30s window must be refused")
	} else if wait != 25*time.Second {
		t.Errorf("retry-after: want 25s, got %v", wait)
	}
	if ok, _ := d.allow(t0.Add(30 * time.Second)); !ok {
		t.Error("a call at exactly the window edge must be allowed — refusing it with wait 0 tells the " +
			"client to retry immediately")
	}
	// The refused call must not have moved the clock: the window that just
	// expired was measured from t0, not from the refusal at t0+5s.
	if ok, _ := d.allow(t0.Add(59 * time.Second)); ok {
		t.Error("the window must restart from the ALLOWED call at t0+30s, not from a refusal")
	}
	if ok, _ := d.allow(t0.Add(60 * time.Second)); !ok {
		t.Error("the window must expire 30s after the allowed call")
	}
}

// TestCallDebouncerFirstCallIsNeverRefused — a zero stamp must not read as
// "called at the epoch, so refuse everything until 30s past 1970".
//
// MUTANT: drop the zero-stamp check from allow and compare the clock against
// the stored nanos unconditionally — on a host whose clock is near the epoch
// (a container before NTP lands) the very first manual check is refused, and
// the user is told to wait for a call nobody made.
func TestCallDebouncerFirstCallIsNeverRefused(t *testing.T) {
	d := newCallDebouncer(30 * time.Second)
	if ok, _ := d.allow(time.Unix(0, 0)); !ok {
		t.Error("the very first call must be allowed whatever the clock reads")
	}
}
