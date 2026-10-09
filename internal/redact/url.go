package redact

import (
	"net/url"
	"strings"
)

// URLOrigin reduces a URL whose credential can sit anywhere in it to the part
// that names the service and carries no secret, for a log line or an error.
// A notification URL is what it is for: Discord's webhook token is a path
// segment, and other services put theirs in the userinfo
// (https://user:password@host, ntfys://token@host), in the authority itself
// (tgram://<bot token>/<chat>) or in a query with no path before it
// (https://host?token=…). Cutting at the first '/' after "://" — the rule
// this replaced — kept every one of those but the first.
//
//   - http and https keep the scheme and the host name and nothing else — no
//     userinfo, port, path, query or fragment:
//     "https://ntfy.example.com/…<redacted>".
//   - Any other scheme keeps the scheme alone, because its authority may be
//     the credential: "tgram://…<redacted>". So does an http(s) URL net/url
//     cannot parse.
//   - A string with no "://" in it, or nothing that reads as a scheme before
//     it, keeps nothing: "…<redacted>". ("user:password@host" would parse
//     with "user" as its scheme.)
func URLOrigin(raw string) string {
	const cut = "…" + Marker
	s := strings.TrimSpace(raw)
	scheme, _, ok := strings.Cut(s, "://")
	if !ok || !isScheme(scheme) {
		return cut
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return scheme + "://" + cut
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return scheme + "://" + cut
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6, which Hostname unbrackets
	}
	return scheme + "://" + host + "/" + cut
}

// isScheme reports whether s is an RFC 3986 scheme: a letter, then letters,
// digits, '+', '-' or '.'.
func isScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z':
		case i > 0 && ('0' <= r && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}
