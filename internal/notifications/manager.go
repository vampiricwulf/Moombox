// Package notifications provides event-based notification dispatch for Moombox.
package notifications

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// discordWebhookRe validates standard Discord webhook URLs (HTTPS only).
var discordWebhookRe = regexp.MustCompile(`^https://(?:\w+\.)?discord\.com/api/webhooks/\d+/[\w-]+`)

// discordWebhookPath matches the path prefix of a Discord webhook URL so we
// can redact everything after /api/webhooks/ in error logs.
var discordWebhookPath = regexp.MustCompile(`(?i)(discord\.com/api/webhooks)/[^\s]*`)

// redactDiscordWebhookURL strips the ID/token segments after /api/webhooks/
// so a rejected-webhook log can still indicate "it was a discord URL" without
// copying the secret portion verbatim.
func redactDiscordWebhookURL(url string) string {
	return discordWebhookPath.ReplaceAllString(url, "$1/<redacted>")
}

// NotificationType represents the visual style of a notification.
type NotificationType int

const (
	TypeInfo NotificationType = iota
	TypeSuccess
	TypeWarning
	TypeError
	TypeDownload
	TypeMuxing
	TypeCancelled
)

// Color returns the Discord embed color for this notification type.
func (t NotificationType) Color() int {
	switch t {
	case TypeInfo:
		return 0x3498db // Blue
	case TypeSuccess:
		return 0x2ecc71 // Green
	case TypeWarning:
		return 0xf1c40f // Yellow
	case TypeError:
		return 0xe74c3c // Red
	case TypeDownload:
		return 0x1abc9c // Teal
	case TypeMuxing:
		return 0x9b59b6 // Purple
	case TypeCancelled:
		return 0xe67e22 // Orange
	default:
		return 0x3498db
	}
}

// Field is a key-value pair displayed inline in a notification embed.
// JSON tags exist so notifications/discord.go can alias this type for
// outbound payloads without maintaining a parallel struct.
type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// SendOptions provides optional parameters for a notification.
type SendOptions struct {
	URL       string // Link URL for the embed title
	Event     string // Event name for filtering (e.g. "download_start")
	Thumbnail string // Thumbnail image URL
	Image     string // Full-width image URL
}

// Manager dispatches notifications to configured targets.
type Manager struct {
	targets []notificationTarget
	wg      sync.WaitGroup
	logger  interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

type notificationTarget struct {
	sender sender
	events map[string]bool // nil means all events
}

type sender interface {
	Send(title, description string, color int, fields []Field, opts SendOptions) error
}

// NewManager creates a new notification manager from config.
//
// **Discord-only**: the only URL scheme handled today is Discord webhook
// (either `discord://ID/TOKEN` or a full `https://discord.com/api/webhooks/...`
// URL). Anything else is logged at Warn and skipped — this is intentional,
// not a TODO. If/when another transport (e.g. ntfy) is added, register a
// new sender in the URL-scheme switch below. Audit reports/small-packages.md.
func NewManager(cfg *config.MoomboxConfig, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Manager {
	m := &Manager{logger: logger}

	for _, nc := range cfg.Notifications {
		url := nc.URL
		if url == "" {
			continue
		}

		var s sender

		// Parse URL scheme
		switch {
		case strings.HasPrefix(url, "discord://"):
			// discord://ID/TOKEN -> https://discord.com/api/webhooks/ID/TOKEN
			raw := strings.TrimPrefix(url, "discord://")
			// Only use the first two path segments (ID/TOKEN), matching TS behavior
			segments := strings.SplitN(raw, "/", 3)
			if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
				logger.Warn("invalid discord:// URL: expected discord://ID/TOKEN", "url", url)
				continue
			}
			parts := strings.Join(segments[:2], "/")
			webhookURL := "https://discord.com/api/webhooks/" + parts
			s = &DiscordWebhook{URL: webhookURL}

		case discordWebhookRe.MatchString(url):
			s = &DiscordWebhook{URL: url}

		case strings.Contains(url, "discord.com/api/webhooks"):
			// Log with the token segment redacted. Even a near-valid URL
			// carries a real secret; a rejection log that copies it verbatim
			// ends up in every log-collection store that tails the file.
			logger.Warn("rejected invalid Discord webhook URL (must be HTTPS with valid ID/token)", "url", redactDiscordWebhookURL(url))
			continue

		default:
			logger.Warn("unsupported notification URL scheme", "url", url)
			continue
		}

		// Build event filter
		var events map[string]bool
		if len(nc.Events) > 0 {
			events = make(map[string]bool, len(nc.Events))
			for _, e := range nc.Events {
				events[e] = true
			}
		}

		m.targets = append(m.targets, notificationTarget{
			sender: s,
			events: events,
		})
	}

	if len(m.targets) > 0 {
		logger.Info("notifications initialized", "targets", len(m.targets))
	}

	return m
}

// Send dispatches a notification to all matching targets asynchronously.
func (m *Manager) Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions) {
	if len(m.targets) == 0 {
		return
	}

	color := ntype.Color()

	for _, target := range m.targets {
		// Check event filter
		if target.events != nil && opts.Event != "" {
			if !target.events[opts.Event] {
				continue
			}
		}

		// Send asynchronously (tracked by WaitGroup for graceful shutdown)
		m.wg.Add(1)
		go func(s sender) {
			defer m.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					m.logger.Error("panic in notification sender", "panic", fmt.Sprint(r))
				}
			}()
			if err := s.Send(title, description, color, fields, opts); err != nil {
				m.logger.Error("notification send failed", "err", err)
			}
		}(target.sender)
	}
}

// Wait blocks until all in-flight notification goroutines have finished
// or a 30-second timeout expires, whichever comes first.
// Call during graceful shutdown to avoid losing notifications.
//
// **Single-call**: Wait drains the WaitGroup once. Subsequent Send calls
// after Wait returns will spawn new goroutines that no future Wait will
// drain. The graceful-shutdown sequence in cmd/moombox stops the worker
// (which is the dominant Send caller) before invoking Wait, so this is
// the correct ordering — calling Wait again after that is a no-op.
// Audit reports/small-packages.md.
func (m *Manager) Wait() {
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil && m.logger != nil {
				m.logger.Error("panic in notification wait", "panic", r)
			}
		}()
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		if m.logger != nil {
			m.logger.Warn("notification wait timed out after 30s")
		}
	}
}

// HasTargets returns true if any notification targets are configured.
func (m *Manager) HasTargets() bool {
	return len(m.targets) > 0
}
