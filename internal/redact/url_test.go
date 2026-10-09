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
//   - dropping the check for an '@' past net/url's authority: every
//     "delimiter in the userinfo" row keeps the part net/url took for the
//     host ("https://tk_SECRET/…<redacted>").
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
		// A delimiter in the userinfo: net/url ends the authority at the
		// first '/', '?' or '#', so it reads the credential's first half as
		// the host — or, for "u:pw@SECRET/x@host", a password's second.
		{"https://tk_SECRET/half@ntfy.example.com/alerts", "https://…<redacted>"},
		{"https://tk_SECRET?half@ntfy.example.com/alerts", "https://…<redacted>"},
		{"https://tk_SECRET#half@ntfy.example.com/alerts", "https://…<redacted>"},
		{"https://u:pw@SECRET/x@ntfy.example.com/alerts", "https://…<redacted>"},
		// The price: an '@' that is only in a path or a query costs the host.
		{"https://hooks.example.com/x?to=a@b", "https://…<redacted>"},
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

// TestURLUserinfo pins the userinfo-only rule: the user:password@ goes, every
// other byte stays, so a validation message still names the value it means.
//
// Mutants (run):
//   - cutting at the FIRST '@': the "two @" row keeps the half after it.
//   - ending the authority at the first '/' (net/url's rule): the "password
//     with a '/'" row keeps the rest of the password.
//   - searching only ahead of the first '?' or '#' (the rule this replaced):
//     the "password with a '?'" and "password with a '#'" rows come back
//     whole, password and all.
func TestURLUserinfo(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no userinfo", "https://dash.example.com/moombox", "https://dash.example.com/moombox"},
		{"user and password", "https://moombox:hunter2SECRET@dash.example.com/x", "https://<redacted>@dash.example.com/x"},
		{"token as the user", "https://SECRETTOKEN@dash.example.com", "https://<redacted>@dash.example.com"},
		{"two @", "https://SECRET@user:SECRET2@dash.example.com", "https://<redacted>@dash.example.com"},
		{"password with a '/'", "https://u:SECRET/half@dash.example.com/x", "https://<redacted>@dash.example.com/x"},
		{"password with a '?'", "https://u:SECRET?half@dash.example.com", "https://<redacted>@dash.example.com"},
		{"password with a '#'", "https://u:SECRET#half@dash.example.com", "https://<redacted>@dash.example.com"},
		{"port-like password before a '#'", "https://u:1234#SECRET@dash.example.com", "https://<redacted>@dash.example.com"},
		// The price of reading every '@' as the end of a userinfo: one in
		// the query costs what comes before it.
		{"@ in the query", "https://dash.example.com/x?to=a@b", "https://<redacted>@b"},
		{"no scheme", "u:SECRET@dash.example.com/x", "<redacted>@dash.example.com/x"},
		{"unparseable", "https://u:SECRET@dash example.com", "https://<redacted>@dash example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := URLUserinfo(tc.in)
			if got != tc.want {
				t.Fatalf("URLUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "SECRET") {
				t.Fatalf("URLUserinfo(%q) = %q keeps the secret", tc.in, got)
			}
		})
	}
}
