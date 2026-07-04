package notifications

import (
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

type testLogger struct{}

func (l testLogger) Debug(msg string, args ...any) {}
func (l testLogger) Info(msg string, args ...any)  {}
func (l testLogger) Warn(msg string, args ...any)  {}
func (l testLogger) Error(msg string, args ...any) {}

// recordingSender captures delivered titles so filter tests can assert
// which notifications actually reached a target.
type recordingSender struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordingSender) Send(title, _ string, _ int, _ []Field, _ SendOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, title)
	return nil
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
	m := &Manager{
		logger:    testLogger{},
		semaphore: make(chan struct{}, maxInflightNotifications),
		targets: []notificationTarget{
			{sender: rec, events: map[string]bool{"disk_warning": true}},
		},
	}

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
	m2 := &Manager{
		logger:    testLogger{},
		semaphore: make(chan struct{}, maxInflightNotifications),
		targets: []notificationTarget{
			{sender: rec, events: map[string]bool{"": true}},
		},
	}
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
	m := &Manager{
		targets: []notificationTarget{
			{sender: nil},
		},
	}
	if !m.HasTargets() {
		t.Error("expected HasTargets() == true when targets exist")
	}
}
