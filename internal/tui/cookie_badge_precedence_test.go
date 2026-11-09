package tui

import "testing"

// TestCookieBadgePrecedence pins the one written order (owner ruling R-D4),
// the contested pair included: an unreadable cookies.txt outranks a parked
// job, and a parked job outranks a healthy check.
//
// Mutants: the list reordered (parked ahead of fileUnreadable); the parked
// arm dropped.
func TestCookieBadgePrecedence(t *testing.T) {
	for _, c := range []struct {
		check  CookieStatus
		parked bool
		want   string
	}{
		{CookieStatusRelogin, true, "relogin"},
		{CookieStatusFileUnreadable, true, "fileUnreadable"},
		{CookieStatusFileUnreadable, false, "fileUnreadable"},
		{CookieStatusOK, true, "parked"},
		{CookieStatusUnknown, true, "parked"},
		{CookieStatusNone, true, "parked"},
		{CookieStatusCookiesOnly, false, "rejected"},
		{CookieStatusUnknown, false, "unknown"},
		{CookieStatusNone, false, "none"},
		{CookieStatusOK, false, "ok"},
	} {
		if got := cookieBadgeState(c.check, c.parked); got != c.want {
			t.Errorf("cookieBadgeState(%v, parked %v) = %q, want %q", c.check, c.parked, got, c.want)
		}
	}
}
