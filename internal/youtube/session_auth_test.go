package youtube

import (
	"bytes"
	"testing"
)

// TestWatchPageSessionAuth pins the contract of the watch-page login
// detector, which is deliberately NOT "anything that isn't logged in is
// logged out".
//
// A 200 that lacks the marker is not proof of an anonymous session: consent
// interstitials, edge error pages and A/B shells all answer 200 with no
// ytcfg. Before this state was surfaced to the user that mistake was
// invisible; now it would print "your cookies are dead" at an operator whose
// cookies are fine — the exact failure mode this package exists to remove.
// So logged-out is claimed only from a recognisable watch-page shell (the
// explicit negative marker, or the ytcfg bootstrap every real watch page
// carries), and everything else falls back to unknown, which already has a
// safe generic message behind it.
func TestWatchPageSessionAuth(t *testing.T) {
	tests := []struct {
		name string
		html string
		want SessionAuthState
	}{
		{
			name: "ytcfg LOGGED_IN marker",
			html: `<script>ytcfg.set({"LOGGED_IN":true,"VISITOR_DATA":"x"});</script>`,
			want: SessionAuthLoggedIn,
		},
		{
			name: "camelCase isLoggedIn marker",
			html: `<script>window.x = {"isLoggedIn":true};</script>`,
			want: SessionAuthLoggedIn,
		},
		{
			name: "explicit LOGGED_IN false",
			html: `<script>ytcfg.set({"LOGGED_IN":false});</script>`,
			want: SessionAuthLoggedOut,
		},
		{
			name: "camelCase negative marker",
			html: `<script>window.x = {"isLoggedIn":false};</script>`,
			want: SessionAuthLoggedOut,
		},
		{
			name: "real watch-page shell with no LOGGED_IN key at all",
			html: `<script nonce="x">ytcfg.set({"VISITOR_DATA":"x","INNERTUBE_API_KEY":"k"});</script>`,
			want: SessionAuthLoggedOut,
		},
		{
			name: "consent interstitial / edge error page is NOT evidence of a dead session",
			html: `<html><body>Before you continue to YouTube</body></html>`,
			want: SessionAuthUnknown,
		},
		{
			name: "empty body",
			html: ``,
			want: SessionAuthUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watchPageSessionAuth(tt.html); got != tt.want {
				t.Errorf("watchPageSessionAuth = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWithAttestationPropagatesSessionAuth is the whole point of the field:
// the login state observed on the watch page must survive onto the VideoInfo
// the worker classifies, so a members-only failure can say whether the
// request was even signed in.
//
// The "watch page fetch failed" case is the one that must NOT collapse into
// "logged out": GetVideoInfoAuthenticated substitutes a synthetic
// WatchPageResult on fetch failure, and reporting that as a dead session
// would send the operator chasing cookies that may be perfectly fine.
func TestWithAttestationPropagatesSessionAuth(t *testing.T) {
	tests := []struct {
		name string
		wp   *WatchPageResult
		want SessionAuthState
	}{
		{
			name: "logged in",
			wp:   &WatchPageResult{Ytcfg: DefaultYtcfg(), SessionAuth: SessionAuthLoggedIn},
			want: SessionAuthLoggedIn,
		},
		{
			name: "logged out",
			wp:   &WatchPageResult{Ytcfg: DefaultYtcfg(), SessionAuth: SessionAuthLoggedOut},
			want: SessionAuthLoggedOut,
		},
		{
			name: "watch page fetch failed (synthetic result) stays unknown",
			wp:   &WatchPageResult{Ytcfg: DefaultYtcfg()},
			want: SessionAuthUnknown,
		},
		{
			name: "no watch page at all stays unknown",
			wp:   nil,
			want: SessionAuthUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := withAttestation(&VideoInfo{}, tt.wp, "vid123")
			if info.SessionAuth != tt.want {
				t.Errorf("SessionAuth = %q, want %q", info.SessionAuth, tt.want)
			}
		})
	}
}

// TestWithAttestationSessionAuthDrivesBinding guards the refactor that
// replaced WatchPageResult.IsLoggedIn with SessionAuth: the GVS content
// binding must still pick the datasyncID branch for a signed-in session and
// the visitorData branch otherwise (including "unknown", which is what the
// old failed-fetch path passed as false).
func TestWithAttestationSessionAuthDrivesBinding(t *testing.T) {
	cfg := &YtcfgData{DataSyncID: "ds", VisitorData: "vd"}

	in := withAttestation(&VideoInfo{}, &WatchPageResult{Ytcfg: cfg, SessionAuth: SessionAuthLoggedIn}, "vid123")
	if in.GvsBindingKind != BindingDataSyncID {
		t.Errorf("logged in: binding kind = %q, want %q", in.GvsBindingKind, BindingDataSyncID)
	}

	for _, state := range []SessionAuthState{SessionAuthLoggedOut, SessionAuthUnknown} {
		out := withAttestation(&VideoInfo{}, &WatchPageResult{Ytcfg: cfg, SessionAuth: state}, "vid123")
		if out.GvsBindingKind != BindingVisitorData {
			t.Errorf("%q: binding kind = %q, want %q", state, out.GvsBindingKind, BindingVisitorData)
		}
	}
}

// TestSessionAuthFromBytesMatchesStringVersion pins the two detectors to
// identical behaviour. They exist separately only because the membership
// path holds []byte and must not pay a ~1MB string copy — the SEMANTICS
// must never diverge, or a page read as logged-in on one path is read as
// dead on the other.
func TestSessionAuthFromBytesMatchesStringVersion(t *testing.T) {
	cases := []string{
		`<html>ytcfg.set({"LOGGED_IN":true});</html>`,
		`<html>ytcfg.set({"LOGGED_IN":false});</html>`,
		`<html>{"isLoggedIn":true}</html>`,
		`<html>{"isLoggedIn":false}</html>`,
		`<html>ytcfg.set({"OTHER":1});</html>`,
		`<html>consent interstitial, no ytcfg at all</html>`,
		``,
	}
	for _, html := range cases {
		want := watchPageSessionAuth(html)
		got := sessionAuthFromBytes([]byte(html))
		if got != want {
			t.Errorf("sessionAuthFromBytes(%q) = %q, watchPageSessionAuth = %q", html, got, want)
		}
	}
}

// TestSessionAuthFromBytesDoesNotOverClaim is the property the membership
// probe depends on: an unrecognisable page must be Unknown, never
// LoggedOut. Asserting death on a consent wall would alarm an operator
// whose cookies are fine.
func TestSessionAuthFromBytesDoesNotOverClaim(t *testing.T) {
	for _, html := range []string{
		"",
		"<html>502 Bad Gateway</html>",
		"<html>Before you continue to YouTube</html>",
	} {
		if got := sessionAuthFromBytes([]byte(html)); got != SessionAuthUnknown {
			t.Errorf("sessionAuthFromBytes(%q) = %q, want Unknown", html, got)
		}
	}
}

// TestLivenessVerdictRefusesTheYtcfgFallback: the whole point. A page with a
// ytcfg bootstrap and no login key is Unknown to the probe, even though
// sessionAuthFromBytes calls it LoggedOut for watch-page purposes.
func TestLivenessVerdictRefusesTheYtcfgFallback(t *testing.T) {
	shell := []byte(`<html>ytcfg.set({"OTHER":1});</html>`)
	if got := sessionAuthFromBytes(shell); got != SessionAuthLoggedOut {
		t.Fatalf("precondition: sessionAuthFromBytes = %q, want LoggedOut", got)
	}
	if got := livenessVerdict(shell); got != SessionAuthUnknown {
		t.Errorf("livenessVerdict = %q, want Unknown — a consent shell must not read as a dead session", got)
	}
	// Explicit markers still pass through unchanged.
	if got := livenessVerdict([]byte(`{"LOGGED_IN":true}`)); got != SessionAuthLoggedIn {
		t.Errorf("livenessVerdict(logged-in) = %q, want LoggedIn", got)
	}
	if got := livenessVerdict([]byte(`{"LOGGED_IN":false}`)); got != SessionAuthLoggedOut {
		t.Errorf("livenessVerdict(logged-out) = %q, want LoggedOut", got)
	}
}

// TestSessionAuthFromBytesDoesNotAllocate guards the reason this function
// exists: the membership probe holds a ~1MB page as []byte and reads the
// login flag off it once per channel per monitor cycle, so the marker
// conversions must stay off the heap.
//
// TWO independent things keep the conversions off the heap, and a failure
// here means one of them stopped holding:
//
//   - The markers are compile-time CONSTANTS. For a constant the compiler
//     emits an exact-size stack array, so no size ceiling applies at all.
//   - bytes.Index and bytes.Contains do not retain their argument, so escape
//     analysis keeps the conversion local.
//
// The 32-byte ceiling is real but applies only to the NON-constant case:
// runtime.stringtoslicebyte (runtime/string.go:224) uses its caller's stack
// tmpBuf when len(s) <= tmpStringBufSize (32) and calls rawbyteslice
// otherwise. Measured on go1.26.1, 2026-08-26: const at 31/32/33/66/310 bytes
// all allocate 0; a package var allocates 0 at 31 and 32 bytes and 1 at 33 and
// above; a conversion made to escape allocates 1 at any length.
//
// So read a failure here as either "a converted marker now escapes" or "a
// marker stopped being a constant AND is longer than 32 bytes" — not as
// marker length alone, and not as escape alone.
func TestSessionAuthFromBytesDoesNotAllocate(t *testing.T) {
	page := append(bytes.Repeat([]byte("x"), 900<<10), []byte(`ytcfg.set({"LOGGED_IN":true});`)...)
	if n := testing.AllocsPerRun(200, func() { _ = sessionAuthFromBytes(page) }); n != 0 {
		t.Errorf("allocs/op = %v, want 0 — did a converted marker escape, or stop being a const while exceeding 32 bytes?", n)
	}
}

// TestLivenessVerdictDoesNotAllocate is the same guard one layer up, and the
// hotter path: livenessVerdict is what the membership probe calls, and it
// pays TWO []byte conversions of its own in the guard before delegating to
// sessionAuthFromBytes' three. The guard's MISS path is the worst case — both
// bytes.Contains scans run the full page before it returns Unknown.
//
// It is a separate pin because it covers separate conversions: making only
// livenessVerdict's escape leaves TestSessionAuthFromBytesDoesNotAllocate
// green and fails here.
//
// Same two failure causes as that test — see its comment for the measured
// const/var × length matrix and the runtime source behind it.
func TestLivenessVerdictDoesNotAllocate(t *testing.T) {
	hit := append(bytes.Repeat([]byte("x"), 900<<10), []byte(`ytcfg.set({"LOGGED_IN":true});`)...)
	if n := testing.AllocsPerRun(200, func() { _ = livenessVerdict(hit) }); n != 0 {
		t.Errorf("allocs/op (marker present) = %v, want 0 — did a converted marker escape, or stop being a const while exceeding 32 bytes?", n)
	}
	miss := bytes.Repeat([]byte("x"), 900<<10)
	if n := testing.AllocsPerRun(200, func() { _ = livenessVerdict(miss) }); n != 0 {
		t.Errorf("allocs/op (no marker) = %v, want 0 — did a converted marker escape, or stop being a const while exceeding 32 bytes?", n)
	}
}
