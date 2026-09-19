// Package notifications provides event-based notification dispatch for Moombox.
package notifications

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// discordWebhookRe validates standard Discord webhook URLs (HTTPS only).
var discordWebhookRe = regexp.MustCompile(`^https://(?:\w+\.)?discord\.com/api/webhooks/\d+/[\w-]+`)

// redactURLForLog reduces an arbitrary notification URL to scheme://host for
// log lines. Webhook URLs routinely embed secrets in their path or query
// (Discord tokens, Slack /services/ paths, ntfy tokens) — a rejection log
// that copies one verbatim ends up in every store that tails the log file.
func redactURLForLog(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		// No scheme — show only a short prefix, cut on a rune boundary so a
		// multi-byte character at the edge doesn't log invalid UTF-8.
		if len(raw) > 16 {
			cut := 16
			for cut > 0 && !utf8.RuneStart(raw[cut]) {
				cut--
			}
			return raw[:cut] + "…<redacted>"
		}
		return raw
	}
	host, _, _ := strings.Cut(rest, "/")
	return scheme + "://" + host + "/…<redacted>"
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

// FieldBuilder accumulates notification Fields via chainable calls, reducing
// the repetitive `if cond { fields = append(fields, Field{...}) }` pattern
// that appears ~30 times in internal/worker callers. Each method returns
// the builder so calls can chain. Use Build() to extract the final slice
// (audit reports/worker.md F38).
type FieldBuilder struct {
	fields []Field
}

// NewFieldBuilder returns an empty builder.
func NewFieldBuilder() *FieldBuilder { return &FieldBuilder{} }

// Add appends a full-width (block) field.
func (b *FieldBuilder) Add(name, value string) *FieldBuilder {
	b.fields = append(b.fields, Field{Name: name, Value: value})
	return b
}

// AddInline appends a half-width (side-by-side) field.
func (b *FieldBuilder) AddInline(name, value string) *FieldBuilder {
	b.fields = append(b.fields, Field{Name: name, Value: value, Inline: true})
	return b
}

// AddIf conditionally appends a block field.
func (b *FieldBuilder) AddIf(cond bool, name, value string) *FieldBuilder {
	if cond {
		b.Add(name, value)
	}
	return b
}

// AddInlineIf conditionally appends an inline field.
func (b *FieldBuilder) AddInlineIf(cond bool, name, value string) *FieldBuilder {
	if cond {
		b.AddInline(name, value)
	}
	return b
}

// Build returns the accumulated fields. The builder should not be reused
// afterwards — callers that need multiple builds should start a fresh one.
func (b *FieldBuilder) Build() []Field {
	return b.fields
}

// SendOptions provides optional parameters for a notification.
type SendOptions struct {
	URL       string // Link URL for the embed title
	Event     string // Event name for filtering (e.g. "download_start")
	Thumbnail string // Thumbnail image URL
	Image     string // Full-width image URL
}

// maxInflightNotifications caps the number of concurrent notification
// goroutines. Beyond this, Send drops the notification with a Warn log
// rather than spawning unbounded goroutines under load — Discord
// rate-limits each webhook to 30 req/min anyway, so a higher cap would
// just queue requests for the rate-limiter to throttle. 16 is well
// above the steady-state for a healthy Moombox instance and gives
// enough headroom for a brief burst (e.g. multiple job-completion
// events firing in the same second). Audit reports/small-packages.md.
const maxInflightNotifications = 16

// defaultWaitTimeout bounds Manager.Wait during graceful shutdown. Tests
// inject a shorter waitTimeout; a Manager built without one uses this.
const defaultWaitTimeout = 30 * time.Second

// Manager dispatches notifications to configured targets.
type Manager struct {
	// targetsMu guards targets: Reload (config hot-apply) rebuilds the
	// slice while Send/HasTargets read it from worker goroutines.
	targetsMu sync.RWMutex
	targets   []notificationTarget
	wg        sync.WaitGroup
	semaphore chan struct{}
	// waitTimeout bounds Wait; zero means defaultWaitTimeout (test literals
	// omit it). Set once at construction, never written afterwards.
	waitTimeout time.Duration
	logger      interface {
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

// parseTarget resolves a configured notification URL into a sender.
//
// **Discord-only**: the only URL scheme handled today is Discord webhook
// (either `discord://ID/TOKEN` or a full `https://discord.com/api/webhooks/...`
// URL). Anything else errors — this is intentional, not a TODO. If/when
// another transport (e.g. ntfy) is added, register a new sender here.
// Audit reports/small-packages.md.
//
// Error messages never echo the URL (webhook paths are secrets) — callers
// that log attach a redacted form themselves.
func parseTarget(url string) (sender, error) {
	switch {
	case strings.HasPrefix(url, "discord://"):
		// discord://ID/TOKEN -> https://discord.com/api/webhooks/ID/TOKEN
		raw := strings.TrimPrefix(url, "discord://")
		// Only use the first two path segments (ID/TOKEN), matching TS behavior
		segments := strings.SplitN(raw, "/", 3)
		if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
			return nil, fmt.Errorf("invalid discord:// URL: expected discord://ID/TOKEN")
		}
		parts := strings.Join(segments[:2], "/")
		return &DiscordWebhook{URL: "https://discord.com/api/webhooks/" + parts}, nil

	case discordWebhookRe.MatchString(url):
		return &DiscordWebhook{URL: url}, nil

	case strings.Contains(url, "discord.com/api/webhooks"):
		return nil, fmt.Errorf("invalid Discord webhook URL: must be HTTPS with a numeric ID and token")

	default:
		return nil, fmt.Errorf("unsupported notification URL scheme (Discord webhooks only)")
	}
}

// ValidateURL reports whether a notification URL would be accepted by the
// manager. Exposed for save-time validation in the web/TUI settings flows —
// previously a broken paste was accepted with a success toast and then
// silently warn-skipped at the next startup.
func ValidateURL(url string) error {
	_, err := parseTarget(url)
	return err
}

// SendTest synchronously delivers a single test embed to url with NO
// retries — the caller is an interactive settings flow, where surfacing a
// 429/error immediately beats sleeping through backoff. Bounded by the
// single-attempt request timeout (~15s).
func SendTest(url string) error {
	s, err := parseTarget(url)
	if err != nil {
		return err
	}
	title := "Test Notification"
	desc := "Moombox notifications are configured correctly"
	fields := []Field{{Name: "Status", Value: "Working", Inline: true}}
	if d, ok := s.(*DiscordWebhook); ok {
		return d.sendOnce(title, desc, TypeSuccess.Color(), fields, SendOptions{})
	}
	return s.Send(title, desc, TypeSuccess.Color(), fields, SendOptions{})
}

// buildTargets converts the configured notification list into live targets,
// warn-skipping invalid URLs (with secrets redacted) and warning on
// unknown event-filter entries. Shared by NewManager and Reload.
//
// Duplicates collapse on the RESOLVED webhook URL. parseTarget normalises
// discord://ID/TOKEN and the full https://discord.com/api/webhooks/ID/TOKEN
// form to the same DiscordWebhook.URL, so the two spellings of one webhook —
// and a hand-edited literal duplicate — are ONE destination that used to build
// two targets and post every embed twice.
func buildTargets(cfg *config.MoomboxConfig, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) []notificationTarget {
	var targets []notificationTarget
	// resolved webhook URL -> index into targets. Discord is the only sender
	// today, so every built target has a key; a future sender without one
	// simply never dedupes rather than colliding on "".
	seen := make(map[string]int, len(cfg.Notifications))
	collapsed := 0
	for _, nc := range cfg.Notifications {
		url := nc.URL
		if url == "" {
			continue
		}

		s, err := parseTarget(url)
		if err != nil {
			// Redacted: even a near-valid URL carries a real secret; a
			// rejection log that copies it verbatim ends up in every
			// log-collection store that tails the file.
			logger.Warn("rejected notification URL: "+err.Error(), "url", redactURLForLog(url))
			continue
		}

		// Build event filter
		var events map[string]bool
		if len(nc.Events) > 0 {
			events = make(map[string]bool, len(nc.Events))
			for _, e := range nc.Events {
				// Drop empty entries outright (hand-edited TOML / raw API):
				// stored, an "" key would combine with the alias lookup's
				// zero-value miss to match everything. The map stays non-nil
				// so a filter of ONLY garbage entries matches nothing rather
				// than falling back to "all events".
				if e == "" {
					logger.Warn("notification target filters on empty event name — ignored",
						"url", redactURLForLog(url))
					continue
				}
				// A typo'd event name would otherwise be silently filtered
				// forever — the allowlist never matches, no error anywhere.
				if !KnownEvents[e] {
					logger.Warn("notification target filters on unknown event — it will never match",
						"event", e, "url", redactURLForLog(url))
				}
				events[e] = true
			}
		}

		// Dedupe on the RESOLVED webhook URL, not the configured string: the
		// two spellings of one webhook differ as text and resolve to the same
		// destination. The FIRST occurrence wins — its sender and its slot in
		// the ordered list are what survive.
		key := ""
		if d, ok := s.(*DiscordWebhook); ok {
			key = d.URL
		}
		if key != "" {
			if idx, dup := seen[key]; dup {
				collapsed++
				// UNION the one per-target option, with a nil filter winning
				// outright. nil means "every event", so a webhook listed once
				// unfiltered and once filtered keeps the wider subscription the
				// operator configured; narrowing it would silently drop alerts
				// the config asked for.
				switch {
				case targets[idx].events == nil || events == nil:
					targets[idx].events = nil
				default:
					for e := range events {
						targets[idx].events[e] = true
					}
				}
				continue
			}
			seen[key] = len(targets)
		}

		targets = append(targets, notificationTarget{
			sender: s,
			events: events,
		})
	}
	// One line per config load, carrying the COUNT and nothing else. The
	// webhook path IS the credential, and even the redacted scheme://host form
	// is the same string for every Discord webhook — it would name nothing
	// while inviting a later edit to log the real URL "just this once".
	if collapsed > 0 {
		logger.Info("collapsed duplicate notification targets — a webhook listed more than once posts once",
			"duplicates", collapsed, "targets", len(targets))
	}
	return targets
}

// NewManager creates a new notification manager from config. See
// parseTarget for the accepted URL forms (Discord-only, intentionally).
func NewManager(cfg *config.MoomboxConfig, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Manager {
	m := &Manager{
		logger:      logger,
		semaphore:   make(chan struct{}, maxInflightNotifications),
		targets:     buildTargets(cfg, logger),
		waitTimeout: defaultWaitTimeout,
	}

	if len(m.targets) > 0 {
		logger.Info("notifications initialized", "targets", len(m.targets))
	}

	return m
}

// Reload rebuilds the target list from the current config so notification
// edits apply immediately — previously they silently required a process
// restart (and neither UI flagged the section as restart-required).
// In-flight sends keep the sender they captured; new Sends see the new list.
func (m *Manager) Reload(cfg *config.MoomboxConfig) {
	targets := buildTargets(cfg, m.logger)
	m.targetsMu.Lock()
	m.targets = targets
	m.targetsMu.Unlock()
	m.logger.Info("notification targets reloaded", "targets", len(targets))
}

// Send dispatches a notification to all matching targets asynchronously.
func (m *Manager) Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions) {
	// Snapshot under RLock so a concurrent Reload can't swap the slice
	// mid-iteration. The slice is replaced wholesale, never mutated in
	// place, so iterating the snapshot after release is safe.
	m.targetsMu.RLock()
	targets := m.targets
	m.targetsMu.RUnlock()
	if len(targets) == 0 {
		return
	}

	color := ntype.Color()

	for _, target := range targets {
		// Check event filter. An event that split from a broader legacy name
		// (eventAliases) also matches targets allowlisting the old name, so
		// pre-split filters keep receiving the new event after an upgrade.
		// The alias lookup needs the ok-check: a bare eventAliases[...] map
		// miss yields "", and a garbage events=[""] filter entry would then
		// match EVERY non-aliased event, inverting the allowlist.
		if target.events != nil && opts.Event != "" {
			alias, hasAlias := eventAliases[opts.Event]
			if !target.events[opts.Event] && (!hasAlias || !target.events[alias]) {
				continue
			}
		}

		// Bound concurrent senders. Try-send into the semaphore so a
		// burst of events doesn't spawn a goroutine flood that Discord
		// will just throttle anyway. On overflow, drop with a Warn —
		// notifications are non-critical, so dropping is preferable to
		// blocking the caller (which is often a worker on a hot path).
		select {
		case m.semaphore <- struct{}{}:
		default:
			m.logger.Warn("dropping notification — too many in flight",
				"cap", maxInflightNotifications, "title", title, "event", opts.Event)
			continue
		}

		// Send asynchronously (tracked by WaitGroup for graceful shutdown)
		m.wg.Add(1)
		go func(s sender) {
			defer m.wg.Done()
			defer func() { <-m.semaphore }()
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

// effectiveWaitTimeout returns waitTimeout, or defaultWaitTimeout when the
// field was never set (zero or negative).
func (m *Manager) effectiveWaitTimeout() time.Duration {
	if m.waitTimeout <= 0 {
		return defaultWaitTimeout
	}
	return m.waitTimeout
}

// Wait blocks until all in-flight notification goroutines have finished
// or the wait timeout (defaultWaitTimeout, 30 s, unless injected) expires, whichever comes first.
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
	timeout := m.effectiveWaitTimeout()
	select {
	case <-done:
	case <-time.After(timeout):
		if m.logger != nil {
			m.logger.Warn("notification wait timed out", "after", timeout)
		}
	}
}

// HasTargets returns true if any notification targets are configured.
func (m *Manager) HasTargets() bool {
	m.targetsMu.RLock()
	defer m.targetsMu.RUnlock()
	return len(m.targets) > 0
}
