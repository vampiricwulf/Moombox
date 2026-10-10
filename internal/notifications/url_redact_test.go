package notifications

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// allLinesLogger records every line at every level, message and arguments
// both, so a test can assert that none of them carried a secret.
type allLinesLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *allLinesLogger) add(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg+" "+fmt.Sprint(args...))
}
func (l *allLinesLogger) Debug(msg string, args ...any) { l.add("DEBUG", msg, args...) }
func (l *allLinesLogger) Info(msg string, args ...any)  { l.add("INFO", msg, args...) }
func (l *allLinesLogger) Warn(msg string, args ...any)  { l.add("WARN", msg, args...) }
func (l *allLinesLogger) Error(msg string, args ...any) { l.add("ERROR", msg, args...) }

func (l *allLinesLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestBuildTargetsLogsNoURLCredential pins W24-09 at every buildTargets log
// site that names a URL. A hand-edited config.toml entry is never validated
// before buildTargets — config.Validate does not check notification URLs, and
// a disabled entry skips parseTarget — so it is logged on every boot and
// every Reload as "rejected notification URL…" or "notification target
// disabled — skipping". The URL redactor cut at the first '/' after "://",
// so it kept userinfo, a token in the authority, and a query with no path in
// front of it.
//
// Mutant (run): redact.URLOrigin back to the old first-'/' cut — the
// rejected, disabled and unknown-event lines carry SECRET.
func TestBuildTargetsLogsNoURLCredential(t *testing.T) {
	lg := &allLinesLogger{}
	mentionEvents := []string{"", "no_such_event"}
	cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		// rejected: not a Discord URL
		{URL: "https://moombox:hunter2SECRET@ntfy.example.com/alerts"},
		{URL: "ntfys://tk_SECRETTOKEN@ntfy.sh/moombox"},
		{URL: "tgram://123456789:AAH-SECRETBOTTOKEN/987654321"},
		{URL: "https://hooks.example.com?token=SECRETQ"},
		// disabled: logged before it is ever parsed
		{URL: "https://moombox:hunter3SECRET@discord.com/api/webhooks/1/SECRETTOKEN", Enabled: boolPtr(false)},
		// accepted, with a filter and a mention list that warn
		{URL: "https://discord.com/api/webhooks/1/SECRETTOKEN2", Events: []string{"", "no_such_event"}},
		{URL: "https://discord.com/api/webhooks/2/SECRETTOKEN3", Mention: "@here", MentionEvents: &mentionEvents},
	}}
	buildTargets(cfg, lg)

	lines := lg.snapshot()
	for _, want := range []string{
		"rejected notification URL",
		"notification target disabled — skipping",
		"notification target filters on empty event name",
		"notification target filters on unknown event",
		"notification target mentions on an empty event name",
		"notification target mentions on an unknown event",
	} {
		found := false
		for _, l := range lines {
			found = found || strings.Contains(l, want)
		}
		if !found {
			t.Errorf("no %q line — the test no longer reaches that log site; got %q", want, lines)
		}
	}
	for _, l := range lines {
		if strings.Contains(l, "SECRET") {
			t.Errorf("a log line carries a credential: %q", l)
		}
	}
}

// TestDiscordTransportErrorCarriesNoURLCredential covers the other
// redact.URLOrigin site: a Discord delivery that fails at the transport is a
// *url.Error quoting the request URL, and its text reaches the queue's
// failure log and the test route's response. net/http masks a password in
// that URL but keeps the user name, and the path is the webhook token.
//
// Mutant (run): discord.go returning the *url.Error unredacted.
func TestDiscordTransportErrorCarriesNoURLCredential(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	d := &DiscordWebhook{URL: "http://SECRETUSER:SECRETPW@" + addr + "/api/webhooks/1/SECRETTOKEN"}
	_, err = d.do("POST", d.URL, []byte("{}"), false)
	if err == nil {
		t.Fatal("a refused port answered")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("transport error carries a credential: %v", err)
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1/…<redacted>") {
		t.Errorf("error = %v, want the URL reduced to scheme://host", err)
	}
}
