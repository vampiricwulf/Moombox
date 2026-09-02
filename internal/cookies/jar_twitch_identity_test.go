package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every credential in this file is synthetic and none is ever logged.

// twitchJarWith writes a Netscape file holding the given Twitch pair and loads
// it. An empty half is written as an ABSENT ROW, not as an empty value — a
// half-written cookies.txt is what mergeCookieFiles' expiry pruning actually
// leaves behind, and it is the state the whole fingerprint exists to tell
// apart.
func twitchJarWith(t *testing.T, token, login string) *CookieJar {
	t.Helper()
	rows := []string{"# Netscape HTTP Cookie File"}
	if token != "" {
		rows = append(rows, strings.Join([]string{"#HttpOnly_.twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", token}, "\t"))
	}
	if login != "" {
		rows = append(rows, strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "login", login}, "\t"))
	}
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestTwitchIdentityMovesWithEitherHalf is the property Arc 10 R4 rests on.
//
// The mutation it catches is the obvious simplification: fingerprinting the
// auth-token alone. That version passes every "a new token is a change" test
// and silently breaks the two downgrade routes this arc exists for — an
// operator who adds the missing `login` row to a file whose token is fine
// would produce an IDENTICAL fingerprint, the mark would never clear, and the
// live chat session would never re-authenticate.
func TestTwitchIdentityMovesWithEitherHalf(t *testing.T) {
	base := twitchJarWith(t, "test-token-aaaa", "archiveraccount").TwitchIdentity()
	newToken := twitchJarWith(t, "test-token-bbbb", "archiveraccount").TwitchIdentity()
	newLogin := twitchJarWith(t, "test-token-aaaa", "otheraccount").TwitchIdentity()

	if base == "" {
		t.Fatal("a complete Twitch pair fingerprinted to the empty string")
	}
	if base == newToken {
		t.Error("a changed auth-token produced the same fingerprint — a re-exported credential would be invisible")
	}
	if base == newLogin {
		t.Error("a changed login produced the same fingerprint — an account switch would be invisible")
	}
	if newToken == newLogin {
		t.Error("changing the token and changing the login produced the SAME fingerprint — the two halves are not separated in the hash input")
	}
}

// TestTwitchIdentityIsEmptyOnlyWithNoCredentialsAtAll pins the deliberate
// divergence from YouTubeIdentity.
//
// The mutation: copying YouTubeIdentity's `if token == "" || login == ""`
// gate. Under it both half-pairs below fingerprint to "", so the transition
// from "token, no login" to "token, login" — the no-login-cookie repair — reads
// as no change at all.
func TestTwitchIdentityIsEmptyOnlyWithNoCredentialsAtAll(t *testing.T) {
	tokenOnly := twitchJarWith(t, "test-token-aaaa", "").TwitchIdentity()
	loginOnly := twitchJarWith(t, "", "archiveraccount").TwitchIdentity()
	neither := twitchJarWith(t, "", "").TwitchIdentity()
	complete := twitchJarWith(t, "test-token-aaaa", "archiveraccount").TwitchIdentity()

	if tokenOnly == "" {
		t.Error("a jar holding an auth-token with no login fingerprinted to \"\" — that is one of the four downgrade routes, not an absence of credentials")
	}
	if loginOnly == "" {
		t.Error("a jar holding a login with no auth-token fingerprinted to \"\"")
	}
	if neither != "" {
		t.Errorf("a jar with no Twitch credentials fingerprinted to %q, want \"\"", neither)
	}
	if tokenOnly == complete || loginOnly == complete || tokenOnly == loginOnly {
		t.Error("two different Twitch credential states share a fingerprint")
	}
}

// TestTwitchIdentityRevealsNoCredential is the security property. The value is
// compared in code paths near logging and is carried on a RefreshService
// field, while both inputs are secrets: one is a bearer token, the other names
// the signed-in account.
//
// The mutation: returning token+"\x00"+login unhashed, or hex-encoding it
// rather than its digest.
func TestTwitchIdentityRevealsNoCredential(t *testing.T) {
	id := twitchJarWith(t, "test-token-aaaa", "archiveraccount").TwitchIdentity()
	if strings.Contains(id, "test-token-aaaa") || strings.Contains(id, "archiveraccount") {
		t.Error("the fingerprint contains a credential verbatim")
	}
	if len(id) != 64 {
		t.Errorf("fingerprint length = %d, want 64 — a SHA-256 digest in lowercase hex", len(id))
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("fingerprint contains a non-hex character %q — it is not a digest", r)
		}
	}
}

// TestTwitchIdentityIsStable: two reads of one unchanged jar must be equal, or
// every refresh pass would look like a credential change and drop every live
// chat session on a 30-minute timer.
//
// The mutation: mixing time.Now, a counter, or a random salt into the hash.
func TestTwitchIdentityIsStable(t *testing.T) {
	jar := twitchJarWith(t, "test-token-aaaa", "archiveraccount")
	if a, b := jar.TwitchIdentity(), jar.TwitchIdentity(); a != b {
		t.Errorf("two reads of one jar differ: %q vs %q", a, b)
	}
}

// TestTwitchIdentityNilReceiver: RefreshService may hold a nil jar in a
// partially constructed process, and YouTubeIdentity is nil-safe for the same
// reason. The mutation: dropping the nil guard (a panic at boot).
func TestTwitchIdentityNilReceiver(t *testing.T) {
	var jar *CookieJar
	if got := jar.TwitchIdentity(); got != "" {
		t.Errorf("nil jar fingerprint = %q, want \"\"", got)
	}
}

// TestTwitchIdentityReadsOnlyTheTwitchJar: Arc 5 split the jar in two, and a
// name-keyed read against the wrong map is the failure that split exists to
// prevent. A YouTube-only file must fingerprint to "".
//
// The mutation: reading j.jarFor(PlatformYouTube), or a pre-Arc-5 single map.
//
// The YouTube-only half of this check can't distinguish "read the right map"
// from "read the wrong map" on its own: "auth-token" and "login" are not in
// essentialYouTubeCookies (jar.go's admission filter), so those names can
// never enter the YouTube map regardless of which platform a row's domain
// names — a jarFor(PlatformYouTube) mistake would ALSO read an empty map here
// and ALSO return "". TestTwitchIdentityIgnoresCoexistingYouTubeCookies below
// closes that gap with a fixture the wrong-map mutation can actually disagree
// with.
func TestTwitchIdentityReadsOnlyTheTwitchJar(t *testing.T) {
	jar := jarWithAuth(t) // YouTube SAPISID + LOGIN_INFO, no Twitch rows
	if got := jar.TwitchIdentity(); got != "" {
		t.Errorf("a YouTube-only jar produced a Twitch fingerprint %q", got)
	}
	if jar.YouTubeIdentity() == "" {
		t.Fatal("the fixture is wrong: this jar was supposed to hold YouTube credentials")
	}
}

// TestTwitchIdentityIgnoresCoexistingYouTubeCookies is the mutation-catching
// half TestTwitchIdentityReadsOnlyTheTwitchJar's own fixture cannot provide.
//
// A jar loaded from a file carrying valid cookies for BOTH platforms must
// fingerprint by the Twitch pair alone, ignoring the YouTube cookies present
// in the same file. Under a j.jarFor(PlatformYouTube) mistake, the read lands
// on a YouTube map that structurally can never hold "auth-token" or "login"
// (see the admission-filter note above), so it returns "" here instead of the
// real Twitch fingerprint.
//
// want is hardcoded rather than sampled from a second TwitchIdentity() call:
// under the wrong-map mutation EVERY call is broken the same way, so a
// "reference" jar built with twitchJarWith would also fingerprint to "" and
// the comparison would spuriously agree instead of catching anything.
func TestTwitchIdentityIgnoresCoexistingYouTubeCookies(t *testing.T) {
	const want = "c11e916137204630a34d329e411c25cba0f14303aaa10683459bb8e3b25b7f2b" // sha256("test-token-aaaa\x00archiveraccount"), computed independently of this package
	rows := []string{
		"# Netscape HTTP Cookie File",
		strings.Join([]string{".youtube.com", "TRUE", "/", "TRUE", "0", "SAPISID", "sapisid-value"}, "\t"),
		strings.Join([]string{".youtube.com", "TRUE", "/", "TRUE", "0", "LOGIN_INFO", "login-info-value"}, "\t"),
		strings.Join([]string{"#HttpOnly_.twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", "test-token-aaaa"}, "\t"),
		strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "login", "archiveraccount"}, "\t"),
	}
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	if got := jar.TwitchIdentity(); got != want {
		t.Errorf("TwitchIdentity() = %q with coexisting YouTube cookies in the same file, want %q (the Twitch-pair fingerprint) — it may have read the YouTube jar", got, want)
	}
}
