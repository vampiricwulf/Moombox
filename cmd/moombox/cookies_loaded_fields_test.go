package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestCookiesLoadedFieldsCarryTheHorizons pins the startup line's field list.
//
// The line is built inside initServices, which stands up the whole service
// graph and cannot be driven from a test — so the field list is its own pure
// function and this asserts it. Q5 puts the horizons here; Q7 adds the Twitch
// login expiry beside them.
//
// The four pre-existing fields are asserted too: they answer questions the
// horizons do not, and dropping one while adding three would be a silent
// regression in the one line an operator reads at boot.
//
// Mutations: drop any horizon pair; format a zero horizon as a date; carry
// jar.GetSapisid() as a value.
func TestCookiesLoadedFieldsCarryTheHorizons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	body := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1788000000\tSAPISID\tsapisid-secret-value\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-secret\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t1789000000\tauth-token\ttoken-secret-value\n" +
		".twitch.tv\tTRUE\t/\tFALSE\t0\tlogin\tarchiveraccount\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	fields := cookiesLoadedFields(jar, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())

	// Flatten both shapes the logger accepts, as formatLogLine does.
	got := map[string]string{}
	for i := 0; i < len(fields); {
		if attr, ok := fields[i].(slog.Attr); ok {
			got[attr.Key] = attr.Value.String()
			i++
			continue
		}
		if i+1 >= len(fields) {
			t.Fatalf("field %v has no value — the list is not well formed", fields[i])
		}
		key, _ := fields[i].(string)
		val, _ := fields[i+1].(string)
		got[key] = val
		i += 2
	}

	for key, want := range map[string]string{
		"completeAuthSet":    "true",
		"anyAuthCookie":      "true",
		"expiredYouTubeAuth": "0",
		"expiredTwitchAuth":  "0",
		"youtubeAuthHorizon": "2026-08-29T10:40:00Z",
		"twitchAuthHorizon":  "2026-09-10T00:26:40Z",
		"twitchLoginExpiry":  "none",
	} {
		if got[key] != want {
			t.Errorf("field %q = %q, want %q", key, got[key], want)
		}
	}
	if len(got) != 7 {
		t.Errorf("the startup line carries %d fields (%v), want the four predicates plus the three horizons", len(got), got)
	}
	for _, secret := range []string{"sapisid-secret-value", "login-info-secret", "token-secret-value", "archiveraccount"} {
		for key, val := range got {
			if strings.Contains(val, secret) {
				t.Errorf("field %q carries a cookie value (%q) — this line is timestamps and counts only", key, val)
			}
		}
	}
}
