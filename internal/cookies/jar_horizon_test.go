package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// horizonJar builds a jar from literal Netscape rows so the expiries under test
// are exactly the ones written here. No real cookie file is ever read.
func horizonJar(t *testing.T, rows ...string) *CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	body := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestAuthHorizonStringIsATimestampOrNone. Zero is not a timestamp —
// AuthCookieHorizonFor returns it for a jar of session cookies and for an
// empty jar alike, and rendering that as 1970-01-01 would tell an operator
// their credentials expired 56 years ago.
//
// Mutation: format unconditionally.
func TestAuthHorizonStringIsATimestampOrNone(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int64
		want string
	}{
		{"no expiry to run out", 0, "none"},
		{"a negative expiry is not a date either", -1, "none"},
		{"a real expiry renders as UTC RFC3339", 1788000000, "2026-08-29T10:40:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthHorizonString(tc.in); got != tc.want {
				t.Errorf("AuthHorizonString(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTwitchLoginExpiryReadsTheLoginRow is Q7's helper. `login` is deliberately
// NOT in twitchAuthCookieNames — adding it would make a file holding only
// `login` fire "twitch auth lost" on the first check of every start — so
// AuthCookieHorizonFor cannot see it and this reads the row directly. A
// DIAGNOSTIC; it drives no alarm.
//
// Mutation: read "auth-token" instead of "login".
func TestTwitchLoginExpiryReadsTheLoginRow(t *testing.T) {
	const tokenRow = "#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t1788000000\tauth-token\ttest-token"

	t.Run("a login row with an expiry", func(t *testing.T) {
		jar := horizonJar(t, tokenRow, ".twitch.tv\tTRUE\t/\tFALSE\t1787000000\tlogin\tarchiveraccount")
		if got := jar.TwitchLoginExpiry(); got != 1787000000 {
			t.Errorf("TwitchLoginExpiry = %d, want 1787000000 — it must read the login row, not the token's", got)
		}
	})
	t.Run("a session-scoped login row", func(t *testing.T) {
		jar := horizonJar(t, tokenRow, ".twitch.tv\tTRUE\t/\tFALSE\t0\tlogin\tarchiveraccount")
		if got := jar.TwitchLoginExpiry(); got != 0 {
			t.Errorf("TwitchLoginExpiry = %d, want 0 — a session cookie has no expiry to run out", got)
		}
	})
	t.Run("no login row at all", func(t *testing.T) {
		if got := horizonJar(t, tokenRow).TwitchLoginExpiry(); got != 0 {
			t.Errorf("TwitchLoginExpiry = %d on a jar with no login row, want 0", got)
		}
	})
	t.Run("a nil jar", func(t *testing.T) {
		var jar *CookieJar
		if got := jar.TwitchLoginExpiry(); got != 0 {
			t.Errorf("TwitchLoginExpiry on a nil jar = %d, want 0", got)
		}
	})
}

// TestHorizonLogFieldsCarryTimestampsAndNothingElse is the leak scan, and the
// reason the three fields have ONE producer rather than a spelling at each of
// the two log sites. Every value must be "none" or a parseable RFC3339 stamp:
// a cookie value reaching a log line is the failure this subsystem is
// disciplined against, and a horizon field is exactly where one arrives by
// accident (`entry` instead of `entry.expiry`).
//
// Mutation: add a fourth pair carrying jar.GetTwitchAuthToken().
func TestHorizonLogFieldsCarryTimestampsAndNothingElse(t *testing.T) {
	jar := horizonJar(t,
		".youtube.com\tTRUE\t/\tTRUE\t1788000000\tSAPISID\tsapisid-secret-value",
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t1789000000\tauth-token\ttoken-secret-value",
		".twitch.tv\tTRUE\t/\tFALSE\t1787000000\tlogin\tarchiveraccount",
	)

	fields := jar.HorizonLogFields()
	if len(fields)%2 != 0 {
		t.Fatalf("HorizonLogFields returned %d entries — it must be alternating key/value pairs", len(fields))
	}
	want := map[string]string{
		"youtubeAuthHorizon": "2026-08-29T10:40:00Z",
		"twitchAuthHorizon":  "2026-09-10T00:26:40Z",
		"twitchLoginExpiry":  "2026-08-17T20:53:20Z",
	}
	got := map[string]string{}
	for i := 0; i < len(fields); i += 2 {
		key, _ := fields[i].(string)
		val, _ := fields[i+1].(string)
		got[key] = val
		if val == "none" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, val); err != nil {
			t.Errorf("field %q carries %q, which is not a timestamp", key, val)
		}
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("field %q = %q, want %q", key, got[key], wantVal)
		}
	}
	if len(got) != len(want) {
		t.Errorf("HorizonLogFields carries %d fields (%v), want exactly the three horizons", len(got), got)
	}
	for _, secret := range []string{"sapisid-secret-value", "token-secret-value", "archiveraccount"} {
		for key, val := range got {
			if strings.Contains(val, secret) {
				t.Errorf("field %q carries a cookie value (%q)", key, val)
			}
		}
	}
}
