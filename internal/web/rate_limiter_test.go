package web

import (
	"testing"
	"time"
)

func TestRateLimiterUnderLimit(t *testing.T) {
	rl := NewRateLimiter(5, time.Minute)
	defer rl.Close()

	for i := range 5 {
		if !rl.Allow("192.168.1.1") {
			t.Errorf("request %d should be allowed (under limit)", i+1)
		}
	}
}

func TestRateLimiterAtLimit(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)
	defer rl.Close()

	for i := range 3 {
		if !rl.Allow("10.0.0.1") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}

	// 4th request should be denied
	if rl.Allow("10.0.0.1") {
		t.Error("request beyond limit should be denied")
	}
	// 5th request should also be denied
	if rl.Allow("10.0.0.1") {
		t.Error("subsequent request beyond limit should still be denied")
	}
}

func TestRateLimiterDifferentIPs(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute)
	defer rl.Close()

	// Exhaust limit for IP A
	rl.Allow("1.1.1.1")
	rl.Allow("1.1.1.1")
	if rl.Allow("1.1.1.1") {
		t.Error("IP A should be rate limited")
	}

	// IP B should still be allowed
	if !rl.Allow("2.2.2.2") {
		t.Error("IP B should not be affected by IP A's limit")
	}
	if !rl.Allow("2.2.2.2") {
		t.Error("IP B second request should be allowed")
	}
	if rl.Allow("2.2.2.2") {
		t.Error("IP B third request should be denied (limit=2)")
	}
}

func TestRateLimiterWindowExpiry(t *testing.T) {
	rl := NewRateLimiter(2, 50*time.Millisecond)
	defer rl.Close()

	// Use up the limit
	rl.Allow("10.0.0.1")
	rl.Allow("10.0.0.1")
	if rl.Allow("10.0.0.1") {
		t.Error("should be rate limited")
	}

	// Wait for window to expire
	time.Sleep(60 * time.Millisecond)

	// Should be allowed again
	if !rl.Allow("10.0.0.1") {
		t.Error("should be allowed after window expiry")
	}
}

func TestRateLimiterSlidingWindow(t *testing.T) {
	rl := NewRateLimiter(2, 80*time.Millisecond)
	defer rl.Close()

	// First request at t=0
	if !rl.Allow("10.0.0.1") {
		t.Fatal("first request should be allowed")
	}

	// Wait 50ms, second request at t=50ms
	time.Sleep(50 * time.Millisecond)
	if !rl.Allow("10.0.0.1") {
		t.Fatal("second request should be allowed")
	}

	// Third request should be denied (both within 80ms window)
	if rl.Allow("10.0.0.1") {
		t.Error("third request should be denied")
	}

	// Wait 40ms (t=90ms total), first request at t=0 should have expired
	time.Sleep(40 * time.Millisecond)

	// Now one slot should be free (first request expired, second still within window)
	if !rl.Allow("10.0.0.1") {
		t.Error("should be allowed after partial window expiry")
	}
}

func TestRateLimiterClose(t *testing.T) {
	rl := NewRateLimiter(5, time.Minute)

	// Ensure Allow works before close
	if !rl.Allow("1.2.3.4") {
		t.Error("should allow before close")
	}

	// Close should not panic
	rl.Close()
}

func TestRateLimiterLimitOne(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)
	defer rl.Close()

	if !rl.Allow("10.0.0.1") {
		t.Error("first request with limit=1 should be allowed")
	}
	if rl.Allow("10.0.0.1") {
		t.Error("second request with limit=1 should be denied")
	}
}

func TestRateLimiterManyIPs(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)
	defer rl.Close()

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
	for _, ip := range ips {
		if !rl.Allow(ip) {
			t.Errorf("first request from %s should be allowed", ip)
		}
	}
	for _, ip := range ips {
		if rl.Allow(ip) {
			t.Errorf("second request from %s should be denied (limit=1)", ip)
		}
	}
}

// TestRateLimiterBucketsAGlobalIPv6ByItsSlash64: keyed by the full address, a
// client rotating through its own /64 got a fresh bucket per request. LAN
// IPv6 (ULA, link-local) and IPv4 stay exact.
//
// Mutant: drop the bucketKey call from AllowWithRetry — the second address in
// the /64 is allowed.
func TestRateLimiterBucketsAGlobalIPv6ByItsSlash64(t *testing.T) {
	rl := NewRateLimiterCtx(t.Context(), 1, time.Minute)
	if !rl.Allow("2001:db8:1:2::1") {
		t.Fatal("the first request was refused")
	}
	if rl.Allow("2001:db8:1:2:ffff::9") {
		t.Error("a second address in the same /64 got a fresh bucket")
	}
	if !rl.Allow("2001:db8:1:3::1") {
		t.Error("a different /64 was refused")
	}
	for _, pair := range [][2]string{{"fd00::1", "fd00::2"}, {"fe80::1", "fe80::2"}, {"192.168.1.2", "192.168.1.3"}} {
		rl.Allow(pair[0])
		if !rl.Allow(pair[1]) {
			t.Errorf("LAN address %s shares a bucket with %s", pair[1], pair[0])
		}
	}
}
