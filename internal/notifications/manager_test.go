package notifications

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

type testLogger struct{}

func (l testLogger) Debug(msg string, args ...any) {}
func (l testLogger) Info(msg string, args ...any)  {}
func (l testLogger) Warn(msg string, args ...any)  {}
func (l testLogger) Error(msg string, args ...any) {}

// notifConfigWithURLs builds a config whose only content is unfiltered
// notification targets, in order.
func notifConfigWithURLs(urls ...string) *config.MoomboxConfig {
	cfg := &config.MoomboxConfig{}
	for _, u := range urls {
		cfg.Notifications = append(cfg.Notifications, config.NotificationConfig{URL: u})
	}
	return cfg
}

// itoa avoids pulling strconv into the queue tests for one call.
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// newTestManager starts a Manager over ready-made senders exactly as
// NewManager does — applyTargets is the only path that creates queues and
// starts goroutines, so a test that built the slice by hand would be testing a
// Manager production never produces.
func newTestManager(t *testing.T, waitTimeout time.Duration, targets ...notificationTarget) *Manager {
	t.Helper()
	return newTestManagerWithLogger(t, testLogger{}, waitTimeout, targets...)
}

func newTestManagerWithLogger(t *testing.T, lg interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, waitTimeout time.Duration, targets ...notificationTarget,
) *Manager {
	t.Helper()
	return newTestManagerWithClockAndLogger(t, realBatchClock{}, lg, waitTimeout, targets...)
}

// newTestManagerWithClock is newTestManager's third form: the same Manager,
// with a fake clock behind every target's coalescing window (batch.go), so a
// batching test closes a window by hand instead of sleeping out batchWindow.
// Production passes realBatchClock{}.
func newTestManagerWithClock(t *testing.T, clk batchClock, waitTimeout time.Duration, targets ...notificationTarget) *Manager {
	t.Helper()
	return newTestManagerWithClockAndLogger(t, clk, testLogger{}, waitTimeout, targets...)
}

// newTestManagerWithClockAndLogger is the one body the three forms share. The
// clock is threaded through applyTargets → newTargetQueue, which is the only
// path that creates queues and starts goroutines.
func newTestManagerWithClockAndLogger(t *testing.T, clk batchClock, lg interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, waitTimeout time.Duration, targets ...notificationTarget,
) *Manager {
	t.Helper()
	m := &Manager{logger: lg, waitTimeout: waitTimeout, clock: clk}
	m.applyTargets(targets)
	t.Cleanup(func() {
		for _, q := range m.targets {
			q.stopDiscard()
		}
	})
	return m
}

// newTestManagerWithClockFromConfig builds the targets from cfg — so a Reload's
// diff has the same RESOLVED keys to match survivors on that production would —
// and then swaps each built target's live DiscordWebhook for the test sender at
// the same index. The swap is what keeps the package off the network: a target
// built from a real discord:// URL would otherwise POST to Discord.
func newTestManagerWithClockFromConfig(t *testing.T, clk batchClock, lg interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, cfg *config.MoomboxConfig, senders ...sender,
) *Manager {
	t.Helper()
	built := buildTargets(cfg, lg)
	if len(built) != len(senders) {
		t.Fatalf("cfg built %d targets but %d test senders were given", len(built), len(senders))
	}
	for i := range built {
		built[i].sender = senders[i]
	}
	return newTestManagerWithClockAndLogger(t, clk, lg, 5*time.Second, built...)
}

// recordingSender captures delivered titles so filter tests can assert
// which notifications actually reached a target.
type recordingSender struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordingSender) Send(msg Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, msg.logTitle())
	return nil
}

func (r *recordingSender) SendOnce(msg Message) error {
	return r.Send(msg)
}

func (r *recordingSender) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

// TestEventAliasRoutesToLegacyFilter pins the disk_critical → disk_warning
// alias: a target allowlisting only the legacy name must receive the newer
// split-off event, while unrelated events stay filtered.
func TestEventAliasRoutesToLegacyFilter(t *testing.T) {
	rec := &recordingSender{}
	m := newTestManager(t, time.Second, notificationTarget{
		sender: rec,
		events: map[string]bool{"disk_warning": true},
		key:    "k",
	})

	m.Send("critical", "", TypeError, nil, SendOptions{Event: "disk_critical"})
	m.Send("unrelated", "", TypeInfo, nil, SendOptions{Event: "finished"})
	m.Wait()

	got := rec.titles()
	if len(got) != 1 || got[0] != "critical" {
		t.Fatalf("expected only the aliased disk_critical delivery, got %v", got)
	}
}

// TestEmptyEventFilterEntryMatchesNothing guards the allowlist-inversion
// bug: an events=[""] filter must NOT match every non-aliased event (the
// alias lookup's zero-value miss would hit an "" key), and buildTargets
// must drop empty entries while keeping the filter active (empty filter ≠
// no filter).
func TestEmptyEventFilterEntryMatchesNothing(t *testing.T) {
	// buildTargets: the "" entry is dropped but the filter stays non-nil.
	cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		{URL: "discord://123/token", Events: []string{""}},
	}}
	m := NewManager(cfg, testLogger{})
	if len(m.targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(m.targets))
	}
	if m.targets[0].events == nil || len(m.targets[0].events) != 0 {
		t.Fatalf("expected a non-nil EMPTY filter (garbage filter must not become all-events), got %v", m.targets[0].events)
	}

	// Send-side guard: even a hostile "" key in the filter must not match
	// non-aliased events through the alias zero-value lookup.
	rec := &recordingSender{}
	m2 := newTestManager(t, time.Second, notificationTarget{
		sender: rec,
		events: map[string]bool{"": true},
		key:    "k",
	})
	m2.Send("leaked", "", TypeInfo, nil, SendOptions{Event: "finished"})
	m2.Wait()
	if got := rec.titles(); len(got) != 0 {
		t.Fatalf("empty-string filter entry must match nothing, got %v", got)
	}
}

// --- NotificationType.Color() tests ---

func TestNotificationTypeColor(t *testing.T) {
	tests := []struct {
		name     string
		ntype    NotificationType
		expected int
	}{
		{"TypeInfo returns blue", TypeInfo, 0x3498db},
		{"TypeSuccess returns green", TypeSuccess, 0x2ecc71},
		{"TypeWarning returns yellow", TypeWarning, 0xf1c40f},
		{"TypeError returns red", TypeError, 0xe74c3c},
		{"TypeDownload returns teal", TypeDownload, 0x1abc9c},
		{"TypeMuxing returns purple", TypeMuxing, 0x9b59b6},
		{"TypeCancelled returns orange", TypeCancelled, 0xe67e22},
		{"unknown type returns default blue", NotificationType(99), 0x3498db},
		{"negative type returns default blue", NotificationType(-1), 0x3498db},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ntype.Color()
			if got != tt.expected {
				t.Errorf("expected 0x%06x, got 0x%06x", tt.expected, got)
			}
		})
	}
}

// --- discordWebhookRe tests ---

func TestDiscordWebhookRegex(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		matches bool
	}{
		{"valid webhook URL", "https://discord.com/api/webhooks/123456/abcdef-token_123", true},
		{"valid webhook with subdomain", "https://canary.discord.com/api/webhooks/999/abc-def", true},
		{"valid webhook with ptb subdomain", "https://ptb.discord.com/api/webhooks/111/tok-en_val", true},
		{"HTTP rejected", "http://discord.com/api/webhooks/123/token", false},
		{"missing ID segment", "https://discord.com/api/webhooks//token", false},
		{"missing token segment", "https://discord.com/api/webhooks/123/", false},
		{"non-numeric ID", "https://discord.com/api/webhooks/abc/token", false},
		{"completely unrelated URL", "https://example.com/webhook", false},
		{"empty string", "", false},
		// net/url refuses to build a request from these, and its error quoted
		// the whole URL, token and all. Mutant: the query class back to [^#].
		{"tab in the query", "https://discord.com/api/webhooks/123/token?thread_id=42\t", false},
		{"control character in the query", "https://discord.com/api/webhooks/123/token?thread_id=4\x012", false},
		{"space in the query", "https://discord.com/api/webhooks/123/token?thread_id=4 2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discordWebhookRe.MatchString(tt.url)
			if got != tt.matches {
				t.Errorf("discordWebhookRe.MatchString(%q) = %v, expected %v", tt.url, got, tt.matches)
			}
		})
	}
}

// TestDiscordWebhookURLIsAnchored: the pattern must match the WHOLE URL, and
// the anchor must still admit the shapes the edit path is built around.
//
// MUTANT: drop the trailing `$`. The regex then matches a PREFIX, so a URL
// with anything glued after the token is accepted and posted to verbatim —
// "…/TOKEN/../evil" reaches another host's path, a trailing ")" copied out of
// Markdown becomes part of the token, and a pasted URL with a trailing space
// or newline is delivered to a path that is not the webhook's.
//
// The other half is the constraint the anchor must not break: "?thread_id="
// is Discord's documented way to post into a forum thread (execWaitURL and
// messageURL are both written around it) and a trailing slash is what a
// browser's address bar hands back — a bare `$` after the token would reject
// a config that works today.
func TestDiscordWebhookURLIsAnchored(t *testing.T) {
	const base = "https://discord.com/api/webhooks/123456789012345678/tok-en_ABC"
	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{"plain", base, true},
		{"legacy discordapp.com host", strings.Replace(base, "discord.com", "discordapp.com", 1), true},
		{"trailing slash", base + "/", true},
		{"thread_id query", base + "?thread_id=123", true},
		{"thread_id on a trailing slash", base + "/?thread_id=123", true},
		{"trailing paren from a Markdown link", base + ")", false},
		{"trailing space", base + " ", false},
		{"trailing newline", base + "\n", false},
		{"path traversal after the token", base + "/../evil", false},
		{"fragment", base + "#frag", false},
		{"another path segment", base + "/messages/999", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := discordWebhookRe.MatchString(tc.url); got != tc.want {
				t.Errorf("discordWebhookRe.MatchString(%q) = %v, want %v", tc.url, got, tc.want)
			}
			// ValidateURL is the seam both editors call, so the rows must
			// hold through it too.
			if got := ValidateURL(tc.url) == nil; got != tc.want {
				t.Errorf("ValidateURL(%q) accepted = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

// --- NewManager tests ---

func TestNewManagerNoNotifications(t *testing.T) {
	cfg := &config.MoomboxConfig{}
	m := NewManager(cfg, testLogger{})
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if m.HasTargets() {
		t.Error("expected HasTargets() == false with no notifications configured")
	}
}

func TestNewManagerWithDiscordScheme(t *testing.T) {
	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{URL: "discord://123456789/my-token_abc"},
		},
	}
	m := NewManager(cfg, testLogger{})
	if !m.HasTargets() {
		t.Error("expected HasTargets() == true for discord:// scheme")
	}
	if len(m.targets) != 1 {
		t.Errorf("expected 1 target, got %d", len(m.targets))
	}
}

func TestNewManagerWithFullWebhookURL(t *testing.T) {
	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{URL: "https://discord.com/api/webhooks/123456/token-abc"},
		},
	}
	m := NewManager(cfg, testLogger{})
	if !m.HasTargets() {
		t.Error("expected HasTargets() == true for full webhook URL")
	}
}

func TestNewManagerSkipsEmptyURL(t *testing.T) {
	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{URL: ""},
		},
	}
	m := NewManager(cfg, testLogger{})
	if m.HasTargets() {
		t.Error("expected HasTargets() == false when URL is empty")
	}
}

func TestNewManagerRejectsInvalidDiscordURL(t *testing.T) {
	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{URL: "https://discord.com/api/webhooks"},
		},
	}
	m := NewManager(cfg, testLogger{})
	if m.HasTargets() {
		t.Error("expected HasTargets() == false for invalid Discord URL")
	}
}

func TestNewManagerRejectsUnsupportedScheme(t *testing.T) {
	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{URL: "https://example.com/notify"},
		},
	}
	m := NewManager(cfg, testLogger{})
	if m.HasTargets() {
		t.Error("expected HasTargets() == false for unsupported URL scheme")
	}
}

func TestNewManagerEventFilter(t *testing.T) {
	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{
				URL:    "discord://111/token",
				Events: []string{"download_start", "download_finish"},
			},
		},
	}
	m := NewManager(cfg, testLogger{})
	if !m.HasTargets() {
		t.Fatal("expected HasTargets() == true")
	}
	target := m.targets[0]
	if target.events == nil {
		t.Fatal("expected events filter to be non-nil")
	}
	if !target.events["download_start"] {
		t.Error("expected download_start in events filter")
	}
	if !target.events["download_finish"] {
		t.Error("expected download_finish in events filter")
	}
	if target.events["other_event"] {
		t.Error("expected other_event to not be in events filter")
	}
}

func TestNewManagerNoEventsFilterPassesAll(t *testing.T) {
	cfg := &config.MoomboxConfig{
		Notifications: []config.NotificationConfig{
			{URL: "discord://222/token"},
		},
	}
	m := NewManager(cfg, testLogger{})
	if !m.HasTargets() {
		t.Fatal("expected HasTargets() == true")
	}
	target := m.targets[0]
	if target.events != nil {
		t.Error("expected events filter to be nil (pass all events)")
	}
}

// --- HasTargets tests ---

func TestHasTargetsEmpty(t *testing.T) {
	m := &Manager{}
	if m.HasTargets() {
		t.Error("expected HasTargets() == false for empty manager")
	}
}

func TestHasTargetsWithTarget(t *testing.T) {
	m := &Manager{targets: []*targetQueue{{}}}
	if !m.HasTargets() {
		t.Error("expected HasTargets() == true when targets exist")
	}
}

// --- dedupe tests ---

// countingLogger records the Info lines buildTargets emits so the dedupe's
// one-per-config-load summary can be asserted — including that it never
// carries a URL (the webhook path IS the secret).
type countingLogger struct {
	mu       sync.Mutex
	infos    []string
	args     [][]any
	warns    []string
	warnArgs [][]any
}

func (l *countingLogger) Debug(string, ...any) {}

func (l *countingLogger) Info(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
	l.args = append(l.args, args)
}

func (l *countingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg+" "+fmt.Sprint(args...))
	l.warnArgs = append(l.warnArgs, args)
}

func (l *countingLogger) Error(string, ...any) {}

// sawWarnContaining reports whether any Warn line (message or args) carries s.
func (l *countingLogger) sawWarnContaining(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}

// warnsContaining counts the Warn lines whose message or args carry s.
func (l *countingLogger) warnsContaining(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.warns {
		if strings.Contains(w, s) {
			n++
		}
	}
	return n
}

// sumWarnArg adds up the int value logged under `key` across every Warn line,
// so a test can assert that coalesced counts still total what was shed.
func (l *countingLogger) sumWarnArg(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for _, args := range l.warnArgs {
		for i := 0; i+1 < len(args); i += 2 {
			if k, ok := args[i].(string); ok && k == key {
				if v, ok := args[i+1].(int); ok {
					total += v
				}
			}
		}
	}
	return total
}

// TestBuildTargetsDedupesByResolvedURL is MON-6. parseTarget already
// normalises discord://ID/TOKEN and the full https:// form to the same
// DiscordWebhook.URL, so a config carrying both spellings — or a hand-edited
// duplicate — built two targets over one webhook and posted every embed twice.
//
// The FIRST occurrence wins: its sender and its slot in the ordered target
// list are what survive. The one per-target option, the event filter, UNIONs,
// and a nil filter wins outright: nil means "every event", so a target listed
// once unfiltered and once filtered must keep the wider subscription the
// operator configured. Narrowing instead would silently drop alerts the config
// asked for.
//
// Mutants:
//   - drop the dedupe -> row 1 builds 2 targets and every embed posts twice.
//   - key on the CONFIGURED url instead of the resolved one -> row 1 builds 2
//     (the two spellings differ) while row 3 still builds 1, so only the
//     literal-duplicate case is fixed.
//   - intersect the filters instead of unioning -> row 2's merged target no
//     longer matches "found".
//   - let a filtered entry narrow a nil one -> row 4 fails.
//   - dedupe two DIFFERENT webhooks together -> row 5 collapses to 1.
func TestBuildTargetsDedupesByResolvedURL(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	full := "https://discord.com/api/webhooks/" + id + "/" + tok
	short := "discord://" + id + "/" + tok

	for _, tc := range []struct {
		name        string
		notifs      []config.NotificationConfig
		wantTargets int
		wantMatches map[string]bool // event -> the single merged target must match it
	}{
		{
			"the two spellings of one webhook",
			[]config.NotificationConfig{{URL: full}, {URL: short}},
			1, map[string]bool{"found": true, "error": true},
		},
		{
			"filters union",
			[]config.NotificationConfig{
				{URL: full, Events: []string{"found"}},
				{URL: short, Events: []string{"error"}},
			},
			1, map[string]bool{"found": true, "error": true},
		},
		{
			"a literal duplicate",
			[]config.NotificationConfig{{URL: full}, {URL: full}},
			1, map[string]bool{"found": true},
		},
		{
			"a nil filter wins over a narrow one, in either order",
			[]config.NotificationConfig{
				{URL: full, Events: []string{"found"}},
				{URL: short},
			},
			1, map[string]bool{"found": true, "error": true},
		},
		{
			"two genuinely different webhooks stay two",
			[]config.NotificationConfig{{URL: full}, {URL: "https://discord.com/api/webhooks/987654321098765432/" + tok}},
			2, nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MoomboxConfig{Notifications: tc.notifs}
			targets := buildTargets(cfg, testLogger{})
			if len(targets) != tc.wantTargets {
				t.Fatalf("built %d targets, want %d — a webhook listed twice receives every embed twice", len(targets), tc.wantTargets)
			}
			for event, want := range tc.wantMatches {
				got := targets[0].events == nil || targets[0].events[event]
				if got != want {
					t.Errorf("merged target matches %q = %v, want %v", event, got, want)
				}
			}
		})
	}
}

// TestBuildTargetsLogsTheCollapsedCountOnce pins the dedupe's report: ONE Info
// line per config load carrying the COUNT, and no URL anywhere in it. The
// webhook path is the credential — a per-duplicate line naming it (even
// redacted to scheme://host, which for Discord is the same string every time)
// tells the operator nothing the count does not.
//
// Mutants:
//   - log per duplicate instead of once -> two lines for three duplicates of
//     one webhook.
//   - pass the url (or its redacted form) as a log arg -> the "no URL in the
//     args" assertion fails.
//   - log unconditionally -> the clean-config subtest sees a line.
func TestBuildTargetsLogsTheCollapsedCountOnce(t *testing.T) {
	const url = "https://discord.com/api/webhooks/123456789012345678/tok-en_ABC"

	t.Run("three duplicates report once, by count", func(t *testing.T) {
		lg := &countingLogger{}
		cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
			{URL: url}, {URL: url}, {URL: url}, {URL: url},
		}}
		if got := len(buildTargets(cfg, lg)); got != 1 {
			t.Fatalf("built %d targets, want 1", got)
		}
		if len(lg.infos) != 1 {
			t.Fatalf("logged %d Info lines, want exactly 1 per config load: %q", len(lg.infos), lg.infos)
		}
		if !strings.Contains(fmt.Sprint(lg.args[0]...), "3") {
			t.Errorf("the summary does not carry the collapsed count 3: %v", lg.args[0])
		}
		for _, a := range lg.args[0] {
			if s, ok := a.(string); ok && strings.Contains(s, "discord.com") {
				t.Errorf("the summary carries a URL (%q) — the webhook path is the secret", s)
			}
		}
	})

	t.Run("a clean config logs nothing", func(t *testing.T) {
		lg := &countingLogger{}
		cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url}}}
		buildTargets(cfg, lg)
		if len(lg.infos) != 0 {
			t.Errorf("a config with no duplicates logged %q", lg.infos)
		}
	})
}

// A URL net/url cannot build a request from fails in http.NewRequest, whose
// parse error is a *url.Error quoting the whole URL. It reached the queue's
// failure log and the test route's response, token included; it is redacted
// like a transport error now.
//
// Mutant: the construction error returned unredacted — the token is in it.
func TestARequestConstructionErrorCarriesNoToken(t *testing.T) {
	d := &DiscordWebhook{URL: "https://discord.com/api/webhooks/123/SECRETtoken?thread_id=4\x012"}
	_, err := d.do("POST", d.URL, []byte("{}"), false)
	if err == nil {
		t.Fatal("a URL with a control character built a request")
	}
	if strings.Contains(err.Error(), "SECRETtoken") {
		t.Errorf("error carries the webhook token: %v", err)
	}
}
