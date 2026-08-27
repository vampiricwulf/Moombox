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

// TestSessionAuthValueForms pins how the marker VALUE is read, across all
// three detectors at once.
//
// The detector used to test the value with HasPrefix against the literal
// `true`, so every value that was not exactly that byte sequence — one space,
// a quoted boolean, a form nobody has seen yet — fell through to LoggedOut.
// That is a false alarm produced by a serialisation change entirely outside
// our control, and it would fire on every authenticated probe on every
// install at once. LoggedOut is the only verdict the arc acts on, so the
// direction of that failure is the worst one available.
//
// The rule now: LoggedIn on a recognised true, LoggedOut on a recognised
// false, and Unknown on anything this reader cannot read. Unknown is the
// safe answer — verified from the consumers, see the note on
// sessionAuthValue.
//
// NONE of these fixtures contain `ytcfg.set`. That is deliberate and is what
// makes the assertion meaningful: LoggedOut has two possible producers in
// the string/bytes detectors (the value read, and the ytcfg shell fallback),
// so a fixture carrying the bootstrap could not tell which one answered.
// With the bootstrap absent, the value read is the only thing under test.
func TestSessionAuthValueForms(t *testing.T) {
	tests := []struct {
		name string
		body string
		want SessionAuthState
	}{
		// Recognised true.
		{"bare true", `{"LOGGED_IN":true}`, SessionAuthLoggedIn},
		{"one space before true", `{"LOGGED_IN": true}`, SessionAuthLoggedIn},
		{"several spaces before true", `{"LOGGED_IN":   true}`, SessionAuthLoggedIn},
		{"newline and tab before true", "{\"LOGGED_IN\":\n\ttrue}", SessionAuthLoggedIn},
		{"double-quoted true", `{"LOGGED_IN":"true"}`, SessionAuthLoggedIn},
		{"spaced double-quoted true", `{"LOGGED_IN": "true"}`, SessionAuthLoggedIn},
		{"single-quoted true", `{"LOGGED_IN":'true'}`, SessionAuthLoggedIn},
		{"camelCase spaced true", `{"isLoggedIn": true}`, SessionAuthLoggedIn},
		{"camelCase quoted true", `{"isLoggedIn":"true"}`, SessionAuthLoggedIn},
		// Whitespace BEFORE the colon. The key used to carry the colon as part
		// of its literal, so these matched no key at all — see
		// TestSpacedKeyOnAWatchPageIsNotReadAsAnonymous for what that cost.
		{"space before the colon", `{"LOGGED_IN" : true}`, SessionAuthLoggedIn},
		{"space before the colon, no space after", `{"LOGGED_IN" :true}`, SessionAuthLoggedIn},
		{"newline before the colon", "{\"LOGGED_IN\"\n\t: true}", SessionAuthLoggedIn},
		{"camelCase space before the colon", `{"isLoggedIn" : true}`, SessionAuthLoggedIn},

		// Recognised false. These are the cases the arc acts on, so they
		// must keep working in every form the true side accepts.
		{"bare false", `{"LOGGED_IN":false}`, SessionAuthLoggedOut},
		{"one space before false", `{"LOGGED_IN": false}`, SessionAuthLoggedOut},
		{"double-quoted false", `{"LOGGED_IN":"false"}`, SessionAuthLoggedOut},
		{"spaced double-quoted false", `{"LOGGED_IN": "false"}`, SessionAuthLoggedOut},
		{"single-quoted false", `{"LOGGED_IN":'false'}`, SessionAuthLoggedOut},
		{"camelCase spaced false", `{"isLoggedIn": false}`, SessionAuthLoggedOut},
		{"space before the colon, false", `{"LOGGED_IN" : false}`, SessionAuthLoggedOut},

		// Unreadable. Every one of these used to answer LoggedOut.
		{"numeric one", `{"LOGGED_IN":1}`, SessionAuthUnknown},
		{"numeric zero", `{"LOGGED_IN":0}`, SessionAuthUnknown},
		{"null", `{"LOGGED_IN":null}`, SessionAuthUnknown},
		{"longer identifier starting with true", `{"LOGGED_IN":truthy}`, SessionAuthUnknown},
		{"longer identifier starting with false", `{"LOGGED_IN":falsey}`, SessionAuthUnknown},
		{"truncated mid-value", `{"LOGGED_IN":tru`, SessionAuthUnknown},
		{"truncated immediately after the colon", `{"LOGGED_IN":`, SessionAuthUnknown},
		{"unterminated quoted value", `{"LOGGED_IN":"true`, SessionAuthUnknown},
		{"quote closed by the wrong quote", `{"LOGGED_IN":"true'}`, SessionAuthUnknown},
		{"whitespace run past the bound", `{"LOGGED_IN":` + "         " + `true}`, SessionAuthUnknown},
		{"whitespace run past the bound BEFORE the colon", `{"LOGGED_IN"` + "         " + `:true}`, SessionAuthUnknown},
		{"key name with no colon after it at all", `{"LOGGED_IN"}`, SessionAuthUnknown},
		{"camelCase unreadable", `{"isLoggedIn":1}`, SessionAuthUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watchPageSessionAuth(tt.body); got != tt.want {
				t.Errorf("watchPageSessionAuth(%q) = %q, want %q", tt.body, got, tt.want)
			}
			if got := sessionAuthFromBytes([]byte(tt.body)); got != tt.want {
				t.Errorf("sessionAuthFromBytes(%q) = %q, want %q", tt.body, got, tt.want)
			}
			if got := livenessVerdict([]byte(tt.body)); got != tt.want {
				t.Errorf("livenessVerdict(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

// TestUnreadableValueDoesNotFallThrough is the other half of the fix, and the
// half that is easy to get wrong.
//
// A body here carries BOTH an unreadable marker value AND the ytcfg
// bootstrap. If the value read fell through on "unreadable" instead of
// returning, the very next branch would answer LoggedOut off the bootstrap
// and re-introduce the exact false alarm the value reader exists to prevent.
// The marker is the authoritative signal; failing to read it is not a licence
// to answer from a weaker one.
//
// A body carrying an unreadable primary key and a READABLE camelCase key is
// pinned for the same reason: the primary key is the answer we went looking
// for, and a second opinion is not a substitute for it.
func TestUnreadableValueDoesNotFallThrough(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "unreadable value on a page that also carries the ytcfg bootstrap",
			body: `<script>ytcfg.set({"LOGGED_IN":1,"VISITOR_DATA":"x"});</script>`,
		},
		{
			name: "value truncated on a page that also carries the ytcfg bootstrap",
			body: `<script>ytcfg.set({"VISITOR_DATA":"x"});</script><script>x={"LOGGED_IN":`,
		},
		{
			name: "unreadable primary key ahead of a readable camelCase key",
			body: `{"LOGGED_IN":1} ... {"isLoggedIn":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watchPageSessionAuth(tt.body); got != SessionAuthUnknown {
				t.Errorf("watchPageSessionAuth = %q, want Unknown — an unreadable marker fell through to a weaker signal", got)
			}
			if got := sessionAuthFromBytes([]byte(tt.body)); got != SessionAuthUnknown {
				t.Errorf("sessionAuthFromBytes = %q, want Unknown — an unreadable marker fell through to a weaker signal", got)
			}
			if got := livenessVerdict([]byte(tt.body)); got != SessionAuthUnknown {
				t.Errorf("livenessVerdict = %q, want Unknown — an unreadable marker fell through to a weaker signal", got)
			}
		})
	}
}

// TestSpacedKeyOnAWatchPageIsNotReadAsAnonymous is the failure that made the
// key read matter as much as the value read.
//
// The value reader was made whitespace-tolerant, but the KEY was still the
// literal `"LOGGED_IN":` — colon included — so `"LOGGED_IN" : true`, with the
// space on the LEFT of the colon, matched nothing. On the liveness path that
// is safe (no key, no verdict, Unknown). On the watch-page path it is not:
// watchPageSessionAuth falls through to the ytcfg-bootstrap branch, and every
// real watch page carries that bootstrap, so a genuinely SIGNED-IN page reads
// as LoggedOut. That is the false-failure direction — and it costs the
// download its authenticated GVS binding, because withAttestation gives the
// datasyncID branch only to SessionAuthLoggedIn.
//
// The fixture carries the bootstrap ON PURPOSE. Without it the page would
// answer Unknown before and LoggedIn after, which is an improvement but not
// this bug; with it, the pre-fix answer is the confident wrong one.
func TestSpacedKeyOnAWatchPageIsNotReadAsAnonymous(t *testing.T) {
	page := `<script nonce="x">ytcfg.set({"LOGGED_IN" : true,"VISITOR_DATA":"x"});</script>`

	if got := watchPageSessionAuth(page); got != SessionAuthLoggedIn {
		t.Errorf("watchPageSessionAuth = %q, want LoggedIn — a space before the colon made an "+
			"authenticated watch page read off the ytcfg bootstrap instead of its own marker", got)
	}
	if got := sessionAuthFromBytes([]byte(page)); got != SessionAuthLoggedIn {
		t.Errorf("sessionAuthFromBytes = %q, want LoggedIn", got)
	}
	if got := livenessVerdict([]byte(page)); got != SessionAuthLoggedIn {
		t.Errorf("livenessVerdict = %q, want LoggedIn", got)
	}

	// The same page signed OUT still reads as signed out: tolerating the space
	// must not have cost the negative verdict, which is the only one the
	// liveness arc acts on.
	out := `<script nonce="x">ytcfg.set({"LOGGED_IN" : false,"VISITOR_DATA":"x"});</script>`
	if got := livenessVerdict([]byte(out)); got != SessionAuthLoggedOut {
		t.Errorf("livenessVerdict(signed out) = %q, want LoggedOut", got)
	}
}

// TestABareKeyOccurrenceDoesNotHideTheRealMarker is a preservation pin, not a
// bug fix: it passed before this change too, and the point is that it still
// does.
//
// Dropping the colon from the key literal means the key scan can now land on
// an occurrence of `"LOGGED_IN"` that is not a marker — a string in some
// unrelated list. Stopping at the first such hit and answering Unknown would
// be a NEW way to lose a LoggedIn read, and a lost LoggedIn costs the
// authenticated GVS binding just as a false LoggedOut does. So the scan
// continues past a non-marker occurrence, exactly as the colon-bearing literal
// used to skip it.
func TestABareKeyOccurrenceDoesNotHideTheRealMarker(t *testing.T) {
	body := `{"fields":["LOGGED_IN","VISITOR_DATA"],"cfg":{"LOGGED_IN":true}}`

	if got := watchPageSessionAuth(body); got != SessionAuthLoggedIn {
		t.Errorf("watchPageSessionAuth = %q, want LoggedIn — the scan stopped at a non-marker occurrence of the key", got)
	}
	if got := sessionAuthFromBytes([]byte(body)); got != SessionAuthLoggedIn {
		t.Errorf("sessionAuthFromBytes = %q, want LoggedIn", got)
	}
	if got := livenessVerdict([]byte(body)); got != SessionAuthLoggedIn {
		t.Errorf("livenessVerdict = %q, want LoggedIn", got)
	}

	// And the consent-shell refusal survives it: a page whose only mention of
	// the key is a non-marker one, plus a ytcfg bootstrap, must still be
	// Unknown to the probe rather than LoggedOut. The old Contains-guard in
	// livenessVerdict would have waved this through to the bootstrap branch
	// once the key literal lost its colon.
	shell := []byte(`<html>ytcfg.set({"schema":["LOGGED_IN"]});</html>`)
	if got := livenessVerdict(shell); got != SessionAuthUnknown {
		t.Errorf("livenessVerdict(shell) = %q, want Unknown — a page with no actual marker must not "+
			"read as a dead session off the bootstrap", got)
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
		// Value forms. The two detectors now share one value reader, so
		// these cannot drift by construction — asserted anyway, because
		// "cannot drift by construction" is a claim about today's code and
		// this test is what makes it hold tomorrow.
		`<html>ytcfg.set({"LOGGED_IN": true});</html>`,
		`<html>ytcfg.set({"LOGGED_IN": "false"});</html>`,
		`<html>ytcfg.set({"LOGGED_IN":'true'});</html>`,
		`<html>ytcfg.set({"LOGGED_IN":1});</html>`,
		`<html>ytcfg.set({"LOGGED_IN":`,
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
//   - Every converted marker is short. The ytcfg mark is still converted from
//     a compile-time CONSTANT at the call site, which gets an exact-size stack
//     array with no size ceiling at all; the two login keys now reach
//     bytes.Index through sessionAuthMarkerInBytes' `key string` PARAMETER, so
//     their conversion is the non-constant case and does have a ceiling — see
//     below. `"LOGGED_IN"` and `"isLoggedIn"` are 11 and 12 bytes.
//   - bytes.Index and bytes.Contains do not retain their argument, so escape
//     analysis keeps the conversion local.
//
// The 32-byte ceiling on the non-constant case:
// runtime.stringtoslicebyte (runtime/string.go:224) uses its caller's stack
// tmpBuf when len(s) <= tmpStringBufSize (32) and calls rawbyteslice
// otherwise. Measured on go1.26.1, 2026-08-26: const at 31/32/33/66/310 bytes
// all allocate 0; a package var allocates 0 at 31 and 32 bytes and 1 at 33 and
// above; a conversion made to escape allocates 1 at any length.
//
// So read a failure here as either "a converted marker now escapes" or "a
// marker exceeded 32 bytes while not being converted from a constant" — not as
// marker length alone, and not as escape alone.
//
// A third cause was added when the value read moved into sessionAuthValue:
// that reader compares byte-by-byte via sessionAuthWordAt precisely so it
// converts nothing. Rewriting it around string(b[i:j]) == "true" would be
// the natural-looking change that breaks this pin.
func TestSessionAuthFromBytesDoesNotAllocate(t *testing.T) {
	page := append(bytes.Repeat([]byte("x"), 900<<10), []byte(`ytcfg.set({"LOGGED_IN":true});`)...)
	if n := testing.AllocsPerRun(200, func() { _ = sessionAuthFromBytes(page) }); n != 0 {
		t.Errorf("allocs/op = %v, want 0 — did a converted marker escape, or stop being a const while exceeding 32 bytes?", n)
	}
}

// TestLivenessVerdictDoesNotAllocate is the same guard one layer up, and the
// hotter path: livenessVerdict is what the membership probe calls, and it runs
// its own two marker scans over the page rather than delegating. The MISS path
// is the worst case — both scans run the full page before it returns Unknown.
//
// It is a separate pin because it covers separate conversions: the []byte(key)
// each scan makes is a conversion of a PARAMETER, not of a constant, so it
// leans on the 32-byte tmpBuf case described below rather than on the
// exact-size stack array the constant form gets. Both marker keys are well
// under that; a longer one added later fails here first.
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
