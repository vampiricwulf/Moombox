package cookies

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// itoa keeps the fixture rows below readable.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestTwitchLoginPrunedFromMerge is the predicate behind Q7's single Warn.
//
// The state it names is narrow and the only one worth a line: an expired
// `login` goes on the expiry prune while the `auth-token` beside it survives.
// That leaves a Twitch session every predicate in jar.go reads as configured
// and an IRC handshake that goes fully anonymous WITHOUT attempting the login
// — so no refusal happens and the chat downgrade path's own Warn never runs.
//
// It must stay quiet elsewhere: a login that was never there is not a prune, a
// login that survives is not a prune, and a prune taking the auth-token with it
// is a total credential loss the existing loss reporting already names.
func TestTwitchLoginPrunedFromMerge(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).Unix()
	future := time.Now().Add(24 * time.Hour).Unix()
	row := func(name, value string, expiry int64) string {
		return ".twitch.tv\tTRUE\t/\tFALSE\t" + itoa(expiry) + "\t" + name + "\t" + value
	}
	header := "# Netscape HTTP Cookie File\n"

	for _, tc := range []struct {
		name     string
		previous string
		fetched  string
		want     bool
	}{
		{
			name:     "the expired login goes and the token stays",
			previous: header + row("auth-token", "tok", future) + "\n" + row("login", "archiveraccount", past) + "\n",
			fetched:  header,
			want:     true,
		},
		{
			name:     "the login survives",
			previous: header + row("auth-token", "tok", future) + "\n" + row("login", "archiveraccount", future) + "\n",
			fetched:  header,
			want:     false,
		},
		{
			name:     "there was never a login row",
			previous: header + row("auth-token", "tok", future) + "\n",
			fetched:  header,
			want:     false,
		},
		{
			name:     "both halves expired — a total loss, not a half credential",
			previous: header + row("auth-token", "tok", past) + "\n" + row("login", "archiveraccount", past) + "\n",
			fetched:  header,
			want:     false,
		},
		{
			name:     "the fetched set replaces the expired login",
			previous: header + row("auth-token", "tok", future) + "\n" + row("login", "archiveraccount", past) + "\n",
			fetched:  header + row("login", "archiveraccount", future) + "\n",
			want:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeCookieFiles(tc.previous, tc.fetched)
			if got := twitchLoginPrunedFromMerge(tc.previous, tc.fetched, merged); got != tc.want {
				t.Errorf("twitchLoginPrunedFromMerge = %v, want %v\nmerged:\n%s", got, tc.want, merged)
			}
		})
	}
}

// TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned drives the predicate
// through the real refresh so the Warn is wired, not merely writable. The
// import path (detectBrowser nil) reaches the shared merge site with no
// browser. Asserted: the LINE, once, carrying no value.
//
// Mutation: delete the `if twitchLoginPrunedFromMerge(...)` block.
func TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).Unix()
	previous := "# Netscape HTTP Cookie File\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\t" + goodTwitchToken + "\n" +
		".twitch.tv\tTRUE\t/\tFALSE\t" + itoa(past) + "\tlogin\tarchiveraccount\n"

	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	log := &argRecordingLogger{}
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, jar, log)
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	const marker = "row expired and was pruned while the auth-token survived"
	all := log.all()
	if n := strings.Count(all, marker); n != 1 {
		t.Errorf("the half-credential prune was reported %d times, want exactly 1:\n%s", n, all)
	}
	if strings.Contains(all, "archiveraccount") || strings.Contains(all, goodTwitchToken) {
		t.Errorf("the prune report carries a cookie value:\n%s", all)
	}
}
