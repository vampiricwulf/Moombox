package redact

import (
	"strings"
	"testing"
)

// TestURLOrigin pins W24-09's rule: whatever the shape of the URL, a
// credential in it — userinfo, a token in the authority, a query, a webhook
// path — never survives, and an http(s) URL still names its host.
//
// Mutants (run):
//   - the old cut at the first '/' after "://" (scheme + "://" + that host
//     slot): the userinfo, authority-token and path-less-query rows keep
//     their secret.
//   - returning u.Host instead of u.Hostname(): the port row keeps ":8443".
//   - dropping the non-http(s) arm, so every scheme keeps its host: the
//     "tgram, token as the host" row keeps its token.
//   - dropping the isScheme check: "user:SECRET@host://x" keeps everything
//     before its "://" as a "scheme".
func TestURLOrigin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://discord.com/api/webhooks/123/SECRETTOKEN", "https://discord.com/…<redacted>"},
		{"https://moombox:hunter2SECRET@ntfy.example.com/alerts", "https://ntfy.example.com/…<redacted>"},
		{"https://SECRETUSER@ntfy.example.com/alerts", "https://ntfy.example.com/…<redacted>"},
		{"https://hooks.example.com?token=SECRETQ", "https://hooks.example.com/…<redacted>"},
		{"https://hooks.example.com#SECRETFRAG", "https://hooks.example.com/…<redacted>"},
		{"https://hooks.example.com:8443/x", "https://hooks.example.com/…<redacted>"},
		{"HTTPS://Hooks.Example.com/SECRETPATH", "https://Hooks.Example.com/…<redacted>"},
		{"http://[::1]:8080/SECRETPATH", "http://[::1]/…<redacted>"},
		{"  https://u:SECRETPW@h.example/x  ", "https://h.example/…<redacted>"},
		{"ntfys://tk_SECRETTOKEN@ntfy.sh/moombox", "ntfys://…<redacted>"},
		{"tgram://123456789:AAH-SECRETBOTTOKEN/987654321", "tgram://…<redacted>"},
		{"tgram://SECRETBOTTOKEN/987654321", "tgram://…<redacted>"},
		{"discord://123/SECRETTOKEN", "discord://…<redacted>"},
		{"https:///SECRETPATH", "https://…<redacted>"},
		{"user:SECRET@host/path", "…<redacted>"},
		{"SECRETTOKENWITHNOSCHEME", "…<redacted>"},
		{"https://u:SECRET@h ost/x", "https://…<redacted>"},
		{"user:SECRET@host://x", "…<redacted>"},
		{"1abc://SECRET/x", "…<redacted>"},
		{"", "…<redacted>"},
	}
	for _, tc := range cases {
		got := URLOrigin(tc.in)
		if got != tc.want {
			t.Errorf("URLOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "SECRET") {
			t.Errorf("URLOrigin(%q) = %q keeps the secret", tc.in, got)
		}
	}
}
