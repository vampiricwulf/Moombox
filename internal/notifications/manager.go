// Package notifications provides event-based notification dispatch for Moombox.
package notifications

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// discordWebhookRe validates standard Discord webhook URLs (HTTPS only).
// discordapp.com is Discord's legacy domain and still serves webhooks; a URL
// pasted from an old bookmark was rejected outright before it was accepted here
// (audit R6). parseTarget canonicalises it to discord.com.
var discordWebhookRe = regexp.MustCompile(`^https://(?:\w+\.)?discord(?:app)?\.com/api/webhooks/\d+/[\w-]+`)

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

// Sender is the one method every notification producer needs, and the seam
// every producer holds instead of *Manager.
//
// Before it existed, worker, routes and cmd each held the concrete manager,
// whose only constructor resolves Discord webhook URLs — so 43 of the 46
// trigger rows an audit inventoried had no test that could assert the embed,
// and the three that did were pure renderers that never reached a Send.
// internal/notifications/notificationtest.Recorder is the test implementation.
type Sender interface {
	Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions)
}

// Notifier is the OWNER surface: a Sender plus the three lifecycle calls only
// cmd/moombox makes — the cost gate before building an embed (HasTargets), the
// config hot-apply (Reload), and the shutdown pair (BeginShutdown then Wait).
//
// Separate from Sender on purpose. A producer that could call Reload could
// reload the targets from a download goroutine; a producer that could call
// Wait could block a hot path on Discord. Handing every producer the narrow
// half is what keeps that impossible.
type Notifier interface {
	Sender
	HasTargets() bool
	Reload(cfg *config.MoomboxConfig)
	BeginShutdown()
	Wait()
}

// Author is the embed's author line: the channel that produced the job,
// rendered above the title with its avatar. Job.ChannelAvatarURL has existed
// since the rewrite and reached no embed until this field did.
type Author struct {
	Name    string // required by Discord — an author object without one is a 400
	IconURL string
	URL     string
}

// Tier ranks a notification for the per-target queue's overflow policy
// (queue.go). It is NOT a delivery priority: the queue is strictly FIFO, and
// the tier is consulted only when the queue is full and something has to go.
type Tier int

const (
	// TierUnset lets the manager derive the tier from SendOptions.Event. It is
	// the zero value so that the ~36 existing send sites, none of which set a
	// tier, keep getting the right answer.
	TierUnset Tier = iota
	// TierNormal is never dropped while any TierLow entry is queued.
	TierNormal
	// TierLow is the high-volume discovery family. A backfill re-scan (R B)
	// creates a `found` per catalogue row; a dead cookie parks N jobs. Those
	// are what a full queue sheds, never an alert.
	TierLow
)

// lowTierEvents is TierUnset's derivation table. Deliberately small: only the
// four events a single operation can produce in the dozens.
var lowTierEvents = map[string]bool{
	"found":       true,
	"added":       true,
	"scheduled":   true,
	"rescheduled": true,
}

// effectiveTier resolves the tier the queue should use for one send.
func effectiveTier(opts SendOptions) Tier {
	if opts.Tier != TierUnset {
		return opts.Tier
	}
	if lowTierEvents[opts.Event] {
		return TierLow
	}
	return TierNormal
}

// AllowedMentions is Discord's allowed_mentions object: exactly what the
// message `content` is permitted to ping. Parse is NOT omitempty and is always
// non-nil on a sent object — the webhook default is {"parse": ["users"]}, so an
// omitted list silently re-widens a role ping into "every user id in the text".
//
// Exported because Arc N2b resolves one per (target, event) and puts it in
// SendOptions; MentionParse (discord.go) is the resolver.
type AllowedMentions struct {
	Parse []string `json:"parse"`
	Roles []string `json:"roles,omitempty"`
	Users []string `json:"users,omitempty"`
}

// SendOptions provides optional parameters for a notification.
type SendOptions struct {
	URL       string // Link URL for the embed title
	Event     string // Event name for filtering (e.g. "finished")
	Thumbnail string // Thumbnail image URL
	Image     string // Full-width image URL

	// Author is the embed's author line (channel name + avatar + channel page).
	Author *Author
	// Platform and JobID feed the footer ("Moombox · {platform} · {job id}").
	// JobID is also the key Arc N3's edit-in-place mode stores a Discord
	// message id against, which is why it is an option rather than a footer
	// string: a caller must not be able to spell it differently.
	Platform string
	JobID    string
	// Tier ranks this send for the queue's overflow policy. Leave it
	// TierUnset to derive it from Event.
	Tier Tier

	// Mention is the literal ping text ("<@&id>", "<@id>", "@everyone",
	// "@here") a target is configured with, and MentionAllowed is the resolved
	// allowed_mentions object for it — nil when THIS event is not in that
	// target's mention_events, which is what stops the ping. Both are filled
	// by Arc N2b (MentionParse resolves the object from the configured text);
	// N1 defines the fields and the payload shape they produce. Embeds never
	// mention on their own (per Discord API docs), so a ping needs the message
	// `content` plus a matching `allowed_mentions` — see buildPayload.
	Mention        string
	MentionAllowed *AllowedMentions
}

// defaultWaitTimeout bounds Manager.Wait during graceful shutdown. Tests
// inject a shorter waitTimeout; a Manager built without one uses this.
const defaultWaitTimeout = 30 * time.Second

// Manager dispatches notifications to configured targets.
//
// One QUEUE per target, one goroutine per queue. Send is a non-blocking append
// — it is called from worker, monitor and HTTP goroutines and must never wait
// on Discord.
type Manager struct {
	// targetsMu guards targets and byKey: Reload (config hot-apply) rebuilds
	// them while Send/HasTargets read from worker goroutines.
	targetsMu sync.RWMutex
	targets   []*targetQueue
	// byKey indexes targets by resolved webhook URL so Reload can tell a
	// surviving target from a new one.
	byKey map[string]*targetQueue
	// shuttingDown flips once, in BeginShutdown; every queue holds a pointer
	// to it and reads it per delivery.
	shuttingDown atomic.Bool
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

// notificationTarget is one destination as buildTargets resolved it, before
// the manager gives it a queue and a goroutine.
type notificationTarget struct {
	sender sender
	events map[string]bool // nil means all events
	// key is the RESOLVED webhook URL: the dedupe identity, and what Reload
	// matches a surviving target on.
	key string
}

// sender is one delivery destination.
//
// Send runs the full retry ladder; SendOnce makes exactly one attempt — used
// during shutdown (the 10s force-exit cannot accommodate a 2s+5s ladder) and by
// SendTest, where an interactive caller wants the immediate outcome.
type sender interface {
	Send(title, description string, color int, fields []Field, opts SendOptions) error
	SendOnce(title, description string, color int, fields []Field, opts SendOptions) error
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
		// Canonicalise the legacy host. Two reasons, both load-bearing:
		// buildTargets dedupes on the RESOLVED URL, so the two spellings of
		// one webhook would otherwise build two targets and post every embed
		// twice; and Go's http.Client turns a 301/302 on a POST into a GET,
		// so following discordapp.com's redirect would drop the body.
		return &DiscordWebhook{URL: strings.Replace(url, "discordapp.com", "discord.com", 1)}, nil

	case strings.Contains(url, "discord.com/api/webhooks"), strings.Contains(url, "discordapp.com/api/webhooks"):
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
	return s.SendOnce("Test Notification",
		"Moombox notifications are configured correctly",
		TypeSuccess.Color(),
		[]Field{{Name: "Status", Value: "Working", Inline: true}},
		SendOptions{})
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
			key:    key,
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

// applyTargets installs built as the live target set.
//
// The DIFF is on the resolved webhook URL. A target that is still configured
// keeps its existing queue — and therefore its pending items, its in-flight
// delivery and the rate bucket its sender has learned — because a config save
// that touches an unrelated section must not cost every webhook its bucket
// state and its backlog. A target that is gone is told to discard after its
// in-flight item; a new one gets a goroutine.
func (m *Manager) applyTargets(built []notificationTarget) {
	m.targetsMu.Lock()
	previous := m.byKey
	next := make([]*targetQueue, 0, len(built))
	byKey := make(map[string]*targetQueue, len(built))
	for _, t := range built {
		if q, survives := previous[t.key]; survives && t.key != "" {
			q.setEvents(t.events)
			next = append(next, q)
			byKey[t.key] = q
			delete(previous, t.key)
			continue
		}
		q := newTargetQueue(t, m.logger, &m.shuttingDown)
		next = append(next, q)
		if t.key != "" {
			byKey[t.key] = q
		}
		go q.run()
	}
	retired := make([]*targetQueue, 0, len(previous))
	for _, q := range previous {
		retired = append(retired, q)
	}
	m.targets = next
	m.byKey = byKey
	m.targetsMu.Unlock()

	// Outside the lock: stopDiscard takes the queue's own mutex and logs.
	for _, q := range retired {
		q.stopDiscard()
	}
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
		waitTimeout: defaultWaitTimeout,
	}
	m.applyTargets(buildTargets(cfg, logger))

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
	if m == nil {
		return
	}
	m.applyTargets(buildTargets(cfg, m.logger))
	m.targetsMu.RLock()
	n := len(m.targets)
	m.targetsMu.RUnlock()
	m.logger.Info("notification targets reloaded", "targets", n)
}

// Send dispatches a notification to all matching targets asynchronously.
//
// A nil *Manager is a no-op. Every consumer field is the Sender interface now,
// and a TYPED nil — a (*Manager)(nil) assigned into one — produces a NON-nil
// interface holding a nil pointer, which the `if x.notifier != nil` guards at
// ~20 call sites wave straight through (internal/worker/worker.go:313 is the
// live example). Every production assignment today passes a real *Manager, so
// this is insurance against a future typed nil rather than a live bug — and it
// is still required, because nothing else would catch one.
func (m *Manager) Send(title, description string, ntype NotificationType, fields []Field, opts SendOptions) {
	if m == nil {
		return
	}

	// Snapshot under RLock so a concurrent Reload can't swap the slice
	// mid-iteration. The slice is replaced wholesale, never mutated in
	// place, so iterating the snapshot after release is safe.
	m.targetsMu.RLock()
	targets := m.targets
	m.targetsMu.RUnlock()
	if len(targets) == 0 {
		return
	}

	it := queued{
		title:       title,
		description: description,
		color:       ntype.Color(),
		fields:      fields,
		opts:        opts,
		tier:        effectiveTier(opts),
	}
	for _, q := range targets {
		if !q.allows(opts.Event) {
			continue
		}
		q.enqueue(it)
	}
}

// BeginShutdown puts every target into single-attempt mode.
//
// Owner ruling: the 10s force-exit (cmd/moombox/shutdown.go) stays, and a
// worker stop ahead of it can legitimately spend the whole window, so a
// three-attempt ladder with a 2s+5s backoff simply does not fit. One attempt
// per embed is what can be delivered, and operations.md says so rather than
// promising a drain that cannot happen.
func (m *Manager) BeginShutdown() {
	if m == nil {
		return
	}
	m.shuttingDown.Store(true)
}

// effectiveWaitTimeout returns waitTimeout, or defaultWaitTimeout when the
// field was never set (zero or negative).
func (m *Manager) effectiveWaitTimeout() time.Duration {
	if m.waitTimeout <= 0 {
		return defaultWaitTimeout
	}
	return m.waitTimeout
}

// Wait stops accepting new notifications for every target, drains what is
// already queued, and returns when the last goroutine has exited or the wait
// timeout (defaultWaitTimeout, 30 s, unless injected) expires.
//
// **Single-call**: after Wait returns, every queue is closed and later Sends
// are dropped with a Warn. The graceful-shutdown sequence in cmd/moombox stops
// the worker — the dominant Send caller — before invoking Wait, so this is the
// correct ordering. In practice the process's own 10 s force-exit, not this
// timeout, is what bounds the drain; BeginShutdown is what makes the attempts
// fit inside it.
func (m *Manager) Wait() {
	if m == nil {
		return
	}
	m.targetsMu.RLock()
	targets := m.targets
	m.targetsMu.RUnlock()

	timeout := m.effectiveWaitTimeout()
	deadline := time.After(timeout)
	for _, q := range targets {
		q.closeDrain()
	}
	for _, q := range targets {
		select {
		case <-q.done:
		case <-deadline:
			if m.logger != nil {
				m.logger.Warn("notification wait timed out", "after", timeout)
			}
			return
		}
	}
}

// HasTargets returns true if any notification targets are configured.
func (m *Manager) HasTargets() bool {
	if m == nil {
		return false
	}
	m.targetsMu.RLock()
	defer m.targetsMu.RUnlock()
	return len(m.targets) > 0
}
