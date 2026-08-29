package cookies

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestYouTubeIdentity: the fingerprint must separate accounts without ever
// being the credential itself.
func TestYouTubeIdentity(t *testing.T) {
	jarWith := func(kv map[string]string) *CookieJar {
		j := NewCookieJar()
		for k, v := range kv {
			j.cookies[k] = cookieEntry{value: v}
		}
		return j
	}

	if got := (*CookieJar)(nil).YouTubeIdentity(); got != "" {
		t.Errorf("nil jar identity = %q, want empty", got)
	}
	if got := NewCookieJar().YouTubeIdentity(); got != "" {
		t.Errorf("empty jar identity = %q, want empty", got)
	}

	a := jarWith(map[string]string{"SAPISID": "session-1", "LOGIN_INFO": "account-A"}).YouTubeIdentity()
	b := jarWith(map[string]string{"SAPISID": "session-2", "LOGIN_INFO": "account-B"}).YouTubeIdentity()
	if a == "" || b == "" {
		t.Fatal("a jar holding SAPISID + LOGIN_INFO must produce an identity")
	}
	if a == b {
		t.Error("two different accounts produced the same identity — the sweep could never tell them apart")
	}
	// Stable: the same account re-read must fingerprint the same, or every
	// refresh cycle would look like an account change.
	if again := jarWith(map[string]string{"SAPISID": "session-1", "LOGIN_INFO": "account-A"}).YouTubeIdentity(); again != a {
		t.Error("identity is not stable for the same cookies")
	}
	// Never the raw secret. This value is compared, logged around, and stored
	// on job rows; the cookies it derives from are the highest-value
	// credentials the app holds.
	for _, id := range []string{a, b} {
		if strings.Contains(id, "session-") || strings.Contains(id, "account-") {
			t.Error("identity leaks a raw cookie value")
		}
	}

	// __Secure-3PAPISID is the documented fallback (mirrors GetSapisid), and
	// a jar that has both must not fingerprint differently from one that
	// resolves to the same value — otherwise adding the mirror cookie to an
	// export would read as an account change.
	fallback := jarWith(map[string]string{"__Secure-3PAPISID": "session-1", "LOGIN_INFO": "account-A"}).YouTubeIdentity()
	if fallback != a {
		t.Error("__Secure-3PAPISID fallback must fingerprint identically to SAPISID")
	}
}

// TestYouTubeIdentitySeparatesAccountsInOneSession is the case the whole
// membership-resume feature exists to serve, and the one SAPISID alone cannot
// see.
//
// SAPISID identifies a Google *session*, not an account: a multi-login browser
// holds N accounts under one cookie session sharing one SAPISID, which is
// exactly why internal/youtube/auth.go has to select the account separately
// with X-Goog-AuthUser (ytcfg SESSION_INDEX) and X-Goog-PageId
// (DELEGATED_SESSION_ID). If SAPISID identified the account those headers
// would be redundant.
//
// The remedy Moombox prints for a not-a-member failure is "switch the browser
// to the account that holds the membership, then export again"
// (stream_processor.go). Doing precisely that changes LOGIN_INFO and leaves
// SAPISID untouched. Fingerprinting SAPISID alone would therefore go blind to
// the single action the error message asks the user to take.
func TestYouTubeIdentitySeparatesAccountsInOneSession(t *testing.T) {
	jarWith := func(kv map[string]string) *CookieJar {
		j := NewCookieJar()
		for k, v := range kv {
			j.cookies[k] = cookieEntry{value: v}
		}
		return j
	}

	const sharedSession = "one-browser-session-sapisid"
	accountA := jarWith(map[string]string{"SAPISID": sharedSession, "LOGIN_INFO": "channel-A-binding"}).YouTubeIdentity()
	accountB := jarWith(map[string]string{"SAPISID": sharedSession, "LOGIN_INFO": "channel-B-binding"}).YouTubeIdentity()

	if accountA == accountB {
		t.Error("switching the browser's active YouTube account left the identity unchanged — " +
			"the resume trigger is blind to the exact remedy the error message tells the user to perform")
	}
}

// TestYouTubeIdentityNeedsBothCookies: a jar missing either half cannot
// identify an account, and must report "unknown" rather than a fingerprint
// that would compare unequal to every real one and fire spurious sweeps.
func TestYouTubeIdentityNeedsBothCookies(t *testing.T) {
	jarWith := func(kv map[string]string) *CookieJar {
		j := NewCookieJar()
		for k, v := range kv {
			j.cookies[k] = cookieEntry{value: v}
		}
		return j
	}
	if got := jarWith(map[string]string{"SAPISID": "s"}).YouTubeIdentity(); got != "" {
		t.Errorf("SAPISID without LOGIN_INFO = %q, want empty", got)
	}
	if got := jarWith(map[string]string{"LOGIN_INFO": "l"}).YouTubeIdentity(); got != "" {
		t.Errorf("LOGIN_INFO without SAPISID = %q, want empty", got)
	}
}

// TestShouldObserveCredentials pins when a check should hand the current
// account identity to the sweep for re-evaluation.
//
// This trigger exists because OnAuthRecovered structurally cannot serve
// membership parks: such a job parks while the session is HEALTHY, so the
// operator swapping to the member account produces no
// not-authenticated → authenticated transition to ride.
//
// The trigger is only a WAKE-UP. The actual resume decision compares each
// job's recorded park identity against the current one, so a missed trigger
// costs a delay, never a permanent strand — which is what lets this predicate
// stay simple.
func TestShouldObserveCredentials(t *testing.T) {
	cases := []struct {
		name     string
		baseline string
		now      string
		nowAuth  bool
		checkErr error
		want     bool
	}{
		{
			name:     "different account with working auth fires",
			baseline: "aaa",
			now:      "bbb",
			nowAuth:  true,
			want:     true,
		},
		{
			// The steady state. Every 30-minute check must be silent, or the
			// sweep runs a full job scan on every cycle for nothing.
			name:     "same account does not fire",
			baseline: "aaa",
			now:      "aaa",
			nowAuth:  true,
			want:     false,
		},
		{
			// First conclusive authenticated check of the process. It MUST
			// fire: an operator who stopped Moombox, replaced the cookies and
			// started it again produces no in-process transition at all, and
			// the per-job comparison is what decides whether anything
			// actually moves. Firing here is how an offline swap is seen.
			name:     "first observation of the process fires",
			baseline: "",
			now:      "bbb",
			nowAuth:  true,
			want:     true,
		},
		{
			// Cookies removed. Nothing to resume INTO — and this is the auth
			// LOSS path, which OnRecoveryNeeded owns.
			name:     "identity disappearing does not fire",
			baseline: "aaa",
			now:      "",
			nowAuth:  false,
			want:     false,
		},
		{
			// Different credentials that do not authenticate are not a fix.
			// Critically, the caller must ALSO not advance its baseline here
			// — see TestBaselineSurvivesUnauthenticatedCheck.
			name:     "different account that does not authenticate does not fire",
			baseline: "aaa",
			now:      "bbb",
			nowAuth:  false,
			want:     false,
		},
		{
			// A network error means we learned nothing this cycle.
			name:     "inconclusive check never fires",
			baseline: "aaa",
			now:      "bbb",
			nowAuth:  true,
			checkErr: errors.New("dial tcp: timeout"),
			want:     false,
		},
		{
			name:     "authenticated with no identity at all does not fire",
			baseline: "aaa",
			now:      "",
			nowAuth:  true,
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldObserveCredentials(tc.baseline, tc.now, tc.nowAuth, tc.checkErr)
			if got != tc.want {
				t.Errorf("shouldObserveCredentials = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBaselineSurvivesUnauthenticatedCheck pins the rule that keeps a stale
// intermediate export from eating the only edge.
//
// The flow is the one Moombox's own advice steers users into — browsing on in
// the source profile invalidates an earlier export (worker.go), so a
// first-attempt export arriving dead is routine:
//
//  1. membership park under account A, auth healthy
//  2. operator drops in account B's export, already stale:
//     conclusive check, ytAuth=false
//  3. operator re-exports B properly: ytAuth=true
//
// If step 2 advanced the baseline to B, step 3 compares B against B, never
// fires, and the job is stranded — OnAuthRecovered skips membership parks by
// design, so nothing else would pick it up. The baseline therefore advances
// only on checks that were BOTH conclusive and authenticated.
func TestBaselineSurvivesUnauthenticatedCheck(t *testing.T) {
	baseline := "identity-A"

	// Step 2: a conclusive check that found the new cookies dead.
	if shouldObserveCredentials(baseline, "identity-B", false, nil) {
		t.Fatal("an unauthenticated check must not fire")
	}
	baseline = advanceIdentityBaseline(baseline, "identity-B", false, nil)
	if baseline != "identity-A" {
		t.Fatalf("baseline = %q after an unauthenticated check, want it held at identity-A — "+
			"advancing here consumes the edge and strands the job at step 3", baseline)
	}

	// Step 3: the same account, now working. This is the fix arriving.
	if !shouldObserveCredentials(baseline, "identity-B", true, nil) {
		t.Error("the working re-export must fire — nothing else can resume a membership park")
	}
	baseline = advanceIdentityBaseline(baseline, "identity-B", true, nil)
	if baseline != "identity-B" {
		t.Errorf("baseline = %q, want identity-B once the account is confirmed working", baseline)
	}

	// And it settles: a permanently broken swap never fires repeatedly, and a
	// working one fires exactly once.
	if shouldObserveCredentials(baseline, "identity-B", true, nil) {
		t.Error("steady state must be silent")
	}

	// An inconclusive check must not move the baseline either.
	if got := advanceIdentityBaseline(baseline, "identity-C", true, errors.New("timeout")); got != "identity-B" {
		t.Errorf("baseline = %q after a network error, want identity-B", got)
	}
}

// TestYouTubeIdentityIsAtomicAcrossReload: the fingerprint must never mix
// halves from two different jar states.
//
// Load builds new maps and swaps them under one Lock, so a reader that takes
// RLock twice — once for SAPISID, once for LOGIN_INFO — can straddle the swap
// and produce a fingerprint of a pairing that never existed on disk. That is
// reachable in production: the worker records a park identity on one goroutine
// while the refresh loop calls Reload on another.
//
// Every observation must therefore equal one of the two real account
// fingerprints, never a third value. Run under -race for the full effect.
func TestYouTubeIdentityIsAtomicAcrossReload(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")

	write := func(path, sapisid, loginInfo string) {
		t.Helper()
		content := "# Netscape HTTP Cookie File\n" +
			".youtube.com\tTRUE\t/\tTRUE\t9999999999\tSAPISID\t" + sapisid + "\n" +
			".youtube.com\tTRUE\t/\tTRUE\t9999999999\tLOGIN_INFO\t" + loginInfo + "\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Both halves differ between the two accounts, so any mixed read produces
	// a fingerprint matching neither.
	write(pathA, "sapisid-A", "login-A")
	write(pathB, "sapisid-B", "login-B")

	jar := NewCookieJar()
	if err := jar.Load(pathA); err != nil {
		t.Fatal(err)
	}
	wantA := jar.YouTubeIdentity()
	if err := jar.Load(pathB); err != nil {
		t.Fatal(err)
	}
	wantB := jar.YouTubeIdentity()
	if wantA == "" || wantB == "" || wantA == wantB {
		t.Fatalf("test setup: need two distinct non-empty identities, got %q / %q", wantA, wantB)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p := pathA
			if i%2 == 1 {
				p = pathB
			}
			_ = jar.Load(p)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20000; i++ {
			got := jar.YouTubeIdentity()
			if got != wantA && got != wantB {
				t.Errorf("observed identity %q, which is neither account — "+
					"the two cookie halves were read from different jar states", got)
				break
			}
		}
		close(stop)
	}()

	wg.Wait()
}
