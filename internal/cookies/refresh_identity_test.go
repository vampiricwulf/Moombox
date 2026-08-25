package cookies

import (
	"errors"
	"testing"
)

// TestYouTubeIdentity: the fingerprint must separate accounts without ever
// being the credential itself.
func TestYouTubeIdentity(t *testing.T) {
	jarWith := func(kv map[string]string) *CookieJar {
		j := NewCookieJar()
		for k, v := range kv {
			j.cookies[k] = v
		}
		return j
	}

	if got := (*CookieJar)(nil).YouTubeIdentity(); got != "" {
		t.Errorf("nil jar identity = %q, want empty", got)
	}
	if got := NewCookieJar().YouTubeIdentity(); got != "" {
		t.Errorf("empty jar identity = %q, want empty", got)
	}

	a := jarWith(map[string]string{"SAPISID": "account-A-secret"}).YouTubeIdentity()
	b := jarWith(map[string]string{"SAPISID": "account-B-secret"}).YouTubeIdentity()
	if a == "" || b == "" {
		t.Fatal("a jar holding SAPISID must produce an identity")
	}
	if a == b {
		t.Error("two different accounts produced the same identity — the sweep could never tell them apart")
	}
	// Stable: the same account re-read must fingerprint the same, or every
	// refresh cycle would look like an account change.
	if again := jarWith(map[string]string{"SAPISID": "account-A-secret"}).YouTubeIdentity(); again != a {
		t.Error("identity is not stable for the same SAPISID")
	}
	// Never the raw secret. This value is compared, logged around, and held
	// for the life of the process; the cookie it derives from is the highest
	// value credential the app holds.
	for _, id := range []string{a, b} {
		if id == "account-A-secret" || id == "account-B-secret" {
			t.Error("identity leaks the raw SAPISID")
		}
	}

	// __Secure-3PAPISID is the documented fallback (mirrors GetSapisid), and
	// a jar that has both must not fingerprint differently from one that
	// resolves to the same value — otherwise adding the mirror cookie to an
	// export would read as an account change.
	fallback := jarWith(map[string]string{"__Secure-3PAPISID": "account-A-secret"}).YouTubeIdentity()
	if fallback != a {
		t.Error("__Secure-3PAPISID fallback must fingerprint identically to SAPISID")
	}
}

// TestShouldFireIdentityChange pins when a YouTube account swap counts as
// "the operator supplied different credentials".
//
// This is the resume trigger for membership-parked jobs, and it exists
// because OnAuthRecovered cannot be: a not-a-member job parks while the
// session is HEALTHY, so there is no not-authenticated → authenticated
// transition to ride when the operator later swaps to the member account.
func TestShouldFireIdentityChange(t *testing.T) {
	cases := []struct {
		name     string
		prev     string
		now      string
		nowAuth  bool
		checkErr error
		want     bool
	}{
		{
			name:    "different account with working auth fires",
			prev:    "aaa",
			now:     "bbb",
			nowAuth: true,
			want:    true,
		},
		{
			// The steady state. Every 30-minute check must be silent, or the
			// membership job goes right back to being retried forever.
			name:    "same account does not fire",
			prev:    "aaa",
			now:     "aaa",
			nowAuth: true,
			want:    false,
		},
		{
			// First conclusive observation in this process: there is no
			// baseline, so a restart must not read as an account swap.
			name:    "no previous identity does not fire",
			prev:    "",
			now:     "bbb",
			nowAuth: true,
			want:    false,
		},
		{
			// Cookies removed. Nothing to resume INTO — and this is the auth
			// LOSS path, which OnRecoveryNeeded owns.
			name:    "identity disappearing does not fire",
			prev:    "aaa",
			now:     "",
			nowAuth: false,
			want:    false,
		},
		{
			// A different account whose cookies do not actually authenticate
			// is not a fix. Resuming into it burns an extraction attempt and
			// re-parks; OnAuthRecovered picks the jobs up if it later works.
			name:    "different account that does not authenticate does not fire",
			prev:    "aaa",
			now:     "bbb",
			nowAuth: false,
			want:    false,
		},
		{
			// A network error means we learned nothing this cycle.
			name:     "inconclusive check never fires",
			prev:     "aaa",
			now:      "bbb",
			nowAuth:  true,
			checkErr: errors.New("dial tcp: timeout"),
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldFireIdentityChange(tc.prev, tc.now, tc.nowAuth, tc.checkErr)
			if got != tc.want {
				t.Errorf("shouldFireIdentityChange = %v, want %v", got, tc.want)
			}
		})
	}
}
