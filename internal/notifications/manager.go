// Package notifications provides event-based notification dispatch for Moombox.
package notifications

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// discordWebhookRe validates standard Discord webhook URLs (HTTPS only).
var discordWebhookRe = regexp.MustCompile(`^https://(?:\w+\.)?discord\.com/api/webhooks/\d+/[\w-]+`)

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
type Field struct {
	Name   string
	Value  string
	Inline bool
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
			parts := strings.Join(segments[:min(len(segments), 2)], "/")
			webhookURL := "https://discord.com/api/webhooks/" + parts
			s = &DiscordWebhook{URL: webhookURL, Logger: logger}

		case discordWebhookRe.MatchString(url):
			s = &DiscordWebhook{URL: url, Logger: logger}

		case strings.Contains(url, "discord.com/api/webhooks"):
			logger.Warn("rejected invalid Discord webhook URL (must be HTTPS with valid ID/token)", "url", url)
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

// Wait blocks until all in-flight notification goroutines have finished.
// Call during graceful shutdown to avoid losing notifications.
func (m *Manager) Wait() {
	m.wg.Wait()
}

// HasTargets returns true if any notification targets are configured.
func (m *Manager) HasTargets() bool {
	return len(m.targets) > 0
}
