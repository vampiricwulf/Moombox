package notifications

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// failingSender refuses every delivery, as a deleted Discord webhook does.
type failingSender struct{}

func (failingSender) Send(Message) error     { return errors.New("discord webhook returned 404") }
func (failingSender) SendOnce(Message) error { return errors.New("discord webhook returned 404") }

// errorArgsLogger keeps each Error line's message and arguments as a
// key=value map, so a test can read what one line named.
type errorArgsLogger struct {
	mu     sync.Mutex
	errors []map[string]string
	all    []string
}

func (l *errorArgsLogger) note(msg string, args []any) {
	l.all = append(l.all, msg+" "+fmt.Sprint(args...))
}
func (l *errorArgsLogger) Debug(msg string, args ...any) {}
func (l *errorArgsLogger) Info(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.note(msg, args)
}
func (l *errorArgsLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.note(msg, args)
}
func (l *errorArgsLogger) Error(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.note(msg, args)
	kv := map[string]string{"msg": msg}
	for i := 0; i+1 < len(args); i += 2 {
		kv[fmt.Sprint(args[i])] = fmt.Sprint(args[i+1])
	}
	l.errors = append(l.errors, kv)
}

func (l *errorArgsLogger) sendFailures() []map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]string
	for _, e := range l.errors {
		if e["msg"] == "notification send failed" {
			out = append(out, e)
		}
	}
	return out
}

// TestSendFailedNamesEventTitleAndConfigPosition is R4-8: with several
// webhooks, a failed delivery's log line has to say which notification was
// lost and which webhook lost it. The target is named by its 1-based place in
// [[notifications]] — counted over the whole list, so a muted entry ahead of
// it still counts — and restamped when a Reload moves the entry. Never the
// URL or its token.
//
// Mutants: the bare `"err", err` line (no target/event/title); a position
// taken from the built-targets slice (#1, the muted entry dropped); no
// restamp on Reload (#2 after the move).
func TestSendFailedNamesEventTitleAndConfigPosition(t *testing.T) {
	lg := &errorArgsLogger{}
	muted := config.NotificationConfig{URL: "discord://111/MUTEDSECRET", Enabled: new(false)}
	broken := config.NotificationConfig{URL: "discord://222/BROKENSECRET"}
	healthy := config.NotificationConfig{URL: "discord://333/HEALTHYSECRET"}
	cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{muted, broken, healthy}}
	rec := &recordingSender{}
	m := newTestManagerWithClockFromConfig(t, realBatchClock{}, lg, cfg, failingSender{}, rec)

	m.Send("Job Failed", "", TypeError, nil, SendOptions{Event: "error"})
	waitFor(t, "the first send failure", func() bool { return len(lg.sendFailures()) == 1 })
	got := lg.sendFailures()[0]
	want := map[string]string{"target": "#2", "event": "error", "title": "Job Failed"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("send-failed %s = %q, want %q (line %v)", k, got[k], v, got)
		}
	}

	// The broken webhook moves to the top of the list; its queue survives the
	// Reload (same key), and its label must follow the move.
	m.Reload(&config.MoomboxConfig{Notifications: []config.NotificationConfig{broken, healthy}})
	m.Send("Disk Critical", "", TypeInfo, nil, SendOptions{Event: "error"})
	waitFor(t, "the second send failure", func() bool { return len(lg.sendFailures()) == 2 })
	if got := lg.sendFailures()[1]["target"]; got != "#1" {
		t.Errorf("after the Reload moved it, the target is named %q, want #1", got)
	}

	lg.mu.Lock()
	defer lg.mu.Unlock()
	for _, line := range lg.all {
		if strings.Contains(line, "SECRET") {
			t.Errorf("a log line carries the webhook URL or token: %q", line)
		}
	}
}
