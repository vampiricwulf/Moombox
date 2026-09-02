package cookies

import "testing"

// Arc 10 Task 7a. The periodic auto-cookie timer is the one credential-writing
// path whose caller lives INSIDE this package, so it is the one that needs an
// injected seam rather than a call at the caller.

// TestNotePassCompletedFiresTheHook.
//
// The mutation: notePassCompleted not invoking the hook — an inverted nil
// guard, or a body that falls through. Firing once per PROCESS rather than
// once per pass is the second, which is why two calls are made and counted.
//
// What this does NOT catch, and structurally cannot: the TICK failing to call
// notePassCompleted at all. That branch needs a browser profile, a browser and
// a network — the reason the seam is a named method in the first place — so it
// is a field residual, listed as field gate 6(d).
func TestNotePassCompletedFiresTheHook(t *testing.T) {
	calls := 0
	s := &AutoCookieService{OnPassCompleted: func() { calls++ }}

	s.notePassCompleted()
	s.notePassCompleted()

	if calls != 2 {
		t.Errorf("the hook fired %d times for two completed passes, want 2", calls)
	}
}

// TestNotePassCompletedWithNoHookIsSafe. The hook is injected by cmd/moombox;
// every test in this package, and any embedding that does not wire it, has
// none.
//
// The mutation: dropping the nil guard — a panic inside the periodic
// goroutine, which is recovered but kills that timer for the life of the
// process.
func TestNotePassCompletedWithNoHookIsSafe(t *testing.T) {
	s := &AutoCookieService{}
	s.notePassCompleted() // must not panic
}
