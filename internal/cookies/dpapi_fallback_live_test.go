package cookies

import "testing"

// TestDpapiFallbackIsReadPerCall: cookies.dpapi_fallback was mirrored onto a
// bool once at boot, so toggling it in either settings UI did nothing until a
// restart nobody was told about. It is a callback now, asked on every use,
// and an unwired service has the fallback off.
func TestDpapiFallbackIsReadPerCall(t *testing.T) {
	s := &AutoCookieService{}
	if s.dpapiFallbackOn() {
		t.Fatal("an unwired service has the fallback on")
	}
	on := false
	s.DpapiFallback = func() bool { return on }
	if s.dpapiFallbackOn() {
		t.Fatal("fallback on while the setting is off")
	}
	on = true // the operator flips it; no restart
	if !s.dpapiFallbackOn() {
		t.Error("a change to the setting was not seen on the next call")
	}
}
