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
//
// ANCHORED at both ends: without the trailing `$` the pattern matched a
// PREFIX, so anything glued after the token came along to the wire — a ")"
// copied out of a Markdown link, a trailing space or newline from a paste, or
// a "/../evil" that resolves onto another path entirely.
//
// The tail is deliberately not a bare `$`: an optional trailing slash is what
// a browser's address bar hands back, and an optional query is the forum-
// thread form "?thread_id=…" that Discord documents and that execWaitURL and
// messageURL are both written around. A fragment is not admitted — it never
// reaches the server, and both builders drop it. Nor is whitespace or a
// control character in the query: net/url refuses to build a request from
// one, and its parse error quotes the whole URL, token and all — every send
// logged it, and the test route answered it, while validation had passed.
var discordWebhookRe = regexp.MustCompile(`^https://(?:\w+\.)?discord(?:app)?\.com/api/webhooks/\d+/[\w-]+/?(?:\?[^#\s\x00-\x1f\x7f]*)?$`)

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

// Notifier is the OWNER surface: a Sender plus the calls only cmd/moombox
// makes — the cost gate before building an embed (HasTargets), the config
// hot-apply (Reload), the shutdown pair (BeginShutdown then Wait), and the
// deleted-job hooks (ForgetJob, RetainJobs).
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
	ForgetJob(jobID string)
	RetainJobs(live map[string]struct{})
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
// Exported because Arc N2b resolves one per (target, event) and puts it onto
// the Message; MentionParse (discord.go) is the resolver.
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

	// There is deliberately NO mention here. A ping is per TARGET and per
	// MESSAGE, never per producer and never per embed: Manager.Send resolves
	// it from targetQueue.mentionFor and writes it onto the Message, because
	// `content` and `allowed_mentions` are the level Discord applies them at
	// and an embed can never ping anyone. See Message (message.go).
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
	// publicURL is network.public_url: the dashboard base every job embed's
	// title links into. Guarded by targetsMu like the targets themselves, and
	// written BEFORE applyTargets by both NewManager and Reload, so a Send
	// that already sees the new targets can never still see the old base.
	publicURL string
	// shuttingDown flips once, in BeginShutdown; every queue holds a pointer
	// to it and reads it per delivery.
	shuttingDown atomic.Bool
	// waitTimeout bounds Wait; zero means defaultWaitTimeout (test literals
	// omit it). Set once at construction, never written afterwards.
	waitTimeout time.Duration
	// clock is the time source every target's coalescing window is armed from
	// (batch.go). Set once at construction and never written afterwards, so
	// applyTargets can read it under targetsMu like waitTimeout; nil means
	// realBatchClock{}, which is what a hand-built Manager in a test gets.
	clock  batchClock
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
	// lifecycle holds the per-(job, target) message ids edit-mode targets
	// rewrite. Created lazily by tracker() so the package's bare &Manager{...}
	// test literals need no extra field.
	lifecycle   *lifecycleTracker
	trackerOnce sync.Once
}

// notificationTarget is one destination as buildTargets resolved it, before
// the manager gives it a queue and a goroutine.
type notificationTarget struct {
	sender sender
	events map[string]bool // nil means all events
	// key is the RESOLVED webhook URL: the dedupe identity, and what Reload
	// matches a surviving target on.
	key string
	// mention is the canonical ping text ("" = this target never pings),
	// mentionAllowed the allowed_mentions object MentionParse resolved for it
	// ONCE at build time, and mentionEvents the filter that decides which
	// events carry the ping (nil whenever mention is "").
	mention        string
	mentionAllowed *AllowedMentions
	mentionEvents  map[string]bool
	// mode is "separate" (default) or "edit" — see lifecycle.go.
	mode string
	// msgKey is targetMsgKey(resolved webhook URL): the stable key this
	// target's lifecycle message ids are stored under. Empty for a transport
	// with no resolved URL, which disables edit mode for it.
	//
	// Precomputed rather than derived from `key` at each send. `key` IS the
	// resolved URL and targetMsgKey hashes it, so deriving lazily would run a
	// SHA-256 per lifecycle embed on the sender goroutine and would keep
	// handing the raw webhook URL — the credential — to the hot path the
	// hashing exists to keep it out of.
	msgKey string
}

// sender is one delivery destination.
//
// Send runs the full retry ladder; SendOnce makes exactly one attempt — used
// during shutdown (the 15s force-exit cannot accommodate a 2s+5s ladder) and by
// SendTest, where an interactive caller wants the immediate outcome.
// Both take a whole Message — one POST, one to ten embeds — because Discord's
// content, allowed_mentions and 6000-character total are all per MESSAGE.
type sender interface {
	Send(msg Message) error
	SendOnce(msg Message) error
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
		resolved := "https://discord.com/api/webhooks/" + strings.Join(segments[:2], "/")
		// The resolved URL passes the same anchored check as the https
		// spelling. Without it this form took anything — a ")" from a
		// Markdown link, a trailing space or newline from a paste, a
		// non-numeric ID — so ValidateURL accepted a broken paste at save
		// time and every send then failed (a 404 Unknown Webhook, dropped as
		// permanent after one attempt, or a request http.NewRequest refused).
		if !discordWebhookRe.MatchString(resolved) {
			return nil, fmt.Errorf("invalid discord:// URL: expected discord://ID/TOKEN with a numeric ID")
		}
		return &DiscordWebhook{URL: resolved}, nil

	case discordWebhookRe.MatchString(url):
		return &DiscordWebhook{URL: canonicalDiscordURL(url)}, nil

	case strings.Contains(url, "discord.com/api/webhooks"), strings.Contains(url, "discordapp.com/api/webhooks"):
		return nil, fmt.Errorf("invalid Discord webhook URL: must be HTTPS with a numeric ID and token")

	default:
		return nil, fmt.Errorf("unsupported notification URL scheme (Discord webhooks only)")
	}
}

// canonicalDiscordURL is the one spelling of an https webhook URL that
// discordWebhookRe has accepted: host discord.com, whatever subdomain or
// legacy discordapp.com it was given as, and no trailing slash on the path,
// with the query kept. Load-bearing twice over. buildTargets dedupes on the
// RESOLVED URL and targetMsgKey hashes it, so "…/TOKEN", "…/TOKEN/" and
// "ptb.discord.com/…/TOKEN" built three targets that posted every embed three
// times — and a slash added in edit mode opened new messages for every job in
// progress. And Go's http.Client turns a 301/302 on a POST into a GET, so
// following discordapp.com's redirect would drop the body.
func canonicalDiscordURL(raw string) string {
	rest := strings.TrimPrefix(raw, "https://")
	path := rest[strings.Index(rest, "/"):] // the pattern guarantees a path
	query := ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path, query = path[:i], path[i:]
	}
	return "https://discord.com" + strings.TrimSuffix(path, "/") + query
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
	return s.SendOnce(One("Test Notification",
		"Moombox notifications are configured correctly",
		TypeSuccess.Color(),
		[]Field{{Name: "Status", Value: "Working", Inline: true}},
		SendOptions{}))
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

		// A disabled target is kept in the config, with its filter and its
		// mention intact, and delivers nothing. This is the mute an operator
		// previously had to fake by deleting the webhook (the web UI's
		// "untick every event" route silently subscribed them to EVERYTHING
		// instead — an empty filter means all events).
		//
		// Before parseTarget and before the dedupe, so a disabled entry can
		// neither shadow its enabled twin nor warn about a URL nobody uses.
		if !nc.IsEnabled() {
			logger.Info("notification target disabled — skipping", "url", redactURLForLog(url))
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

		// Resolve the ping once per config load: the canonical text, the wire
		// object, and the filter that says which events carry it.
		var (
			mention        string
			mentionAllowed *AllowedMentions
			mentionEvents  map[string]bool
		)
		// TWO parsers, two jobs. config.ParseMention validates the operator's
		// string and hands back its canonical form — it lives in
		// internal/config because that is where validateOrNormalize and both
		// Settings editors need it, and internal/config cannot import this
		// package (the import runs the other way). MentionParse
		// (internal/notifications/discord.go) turns that canonical string into
		// the wire object; N1 wrote it for this call and its doc comment says
		// so. Do NOT rebuild the object from (form, id): MentionParse returns
		// Parse: []string{} for the role and user forms, and Parse is
		// json:"parse" WITHOUT omitempty precisely so an empty list is on the
		// wire — a nil there marshals to "parse": null and re-widens the ping
		// to the webhook default.
		if canonical, _, _, err := config.ParseMention(nc.Mention); err == nil && canonical != "" {
			mention = canonical
			mentionAllowed = MentionParse(canonical)
		}
		if mention != "" && mentionAllowed != nil {
			// ResolveMentionEvents encodes the three states: the default six
			// when the key was never written, the stored list otherwise, and
			// an explicit empty list as "never". An empty non-nil map is what
			// carries "never" through to mentionFor — nil there would read as
			// "no mention configured".
			resolved := nc.ResolveMentionEvents()
			mentionEvents = make(map[string]bool, len(resolved))
			for _, e := range resolved {
				if e == "" {
					logger.Warn("notification target mentions on an empty event name — ignored",
						"url", redactURLForLog(url))
					continue
				}
				// Only an OPERATOR-written list is vocabulary-checked. The
				// default list is ours, so warning about it would be noise
				// about our own defaults at every startup.
				if nc.MentionEvents != nil && !KnownEvents[e] {
					logger.Warn("notification target mentions on an unknown event — it will never match",
						"event", e, "url", redactURLForLog(url))
				}
				mentionEvents[e] = true
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
				// the config asked for. The MENTION is not unioned: the first
				// occurrence's ping wins outright, like its sender and its
				// slot, because two mentions have no wider form to merge into
				// and pinging both would double one alert's noise.
				//
				// The MODE is not unioned either, for the same reason and by
				// the same rule: the first occurrence's `mode` (and the
				// msgKey derived from the shared resolved URL) is what
				// survives, so a webhook listed twice cannot be half edited
				// and half separate.
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
			sender:         s,
			events:         events,
			key:            key,
			mention:        mention,
			mentionAllowed: mentionAllowed,
			mentionEvents:  mentionEvents,
			mode:           normalizeTargetMode(nc.Mode),
			msgKey:         targetMsgKey(key),
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
//
// A target with an EMPTY key is outside the diff on both sides: it is never
// matched as a survivor (so a Reload rebuilds it) and it never lands in byKey
// (so a later Reload cannot find it to retire). The next Reload drops it from
// m.targets without ever telling it to stop, and Wait only closes what is IN
// m.targets, so its goroutine would outlive both. Unreachable today:
// parseTarget is Discord-only and every target it
// returns carries the resolved webhook URL as its key. A second target kind
// that cannot name itself must either be given a synthetic key or retired
// here explicitly.
func (m *Manager) applyTargets(built []notificationTarget) {
	// The mode each queue must end up in, applied AFTER the lock is dropped:
	// setMode flushes an open coalescing window, which reaches q.enqueue and
	// can log — the same two reasons the retired-target flush below is outside
	// the lock (and queue.go's pop() note: a lock held across a queue Warn is
	// what a logger re-entering the manager deadlocks on).
	modes := make([]struct {
		q    *targetQueue
		mode string
	}, 0, len(built))

	m.targetsMu.Lock()
	previous := m.byKey
	next := make([]*targetQueue, 0, len(built))
	byKey := make(map[string]*targetQueue, len(built))
	for _, t := range built {
		// bind installs THIS target's POST-or-PATCH decision on whichever
		// queue it ends up with, and records the mode its batcher must move
		// to. It runs in BOTH arms for the same reason setEvents and
		// setMention do: a survivor keeps its queue and the freshly built
		// notificationTarget is thrown away, so a `mode` flip would otherwise
		// be accepted by both UIs and ignored until restart. `tgt` is the
		// per-iteration copy the closure captures.
		tgt := t
		bind := func(q *targetQueue) {
			// The SENDER comes from the queue, never from the freshly built
			// target: a survivor keeps the sender it already has, and with it
			// the rate bucket that sender has learned (the whole reason the
			// diff keeps the queue). Binding tgt.sender would hand every
			// survivor a brand-new *DiscordWebhook on each unrelated config
			// save, silently resetting its bucket state.
			bound := tgt
			bound.sender = q.sender
			q.setDispatch(func(msg Message, once bool) error {
				return m.dispatchOne(bound, msg, once)
			})
			modes = append(modes, struct {
				q    *targetQueue
				mode string
			}{q, normalizeTargetMode(tgt.mode)})
		}
		if q, survives := previous[t.key]; survives && t.key != "" {
			q.setEvents(t.events)
			// The freshly built target is discarded here, so without this a
			// save that changes ONLY mention or mention_events is accepted by
			// both UIs, written to the file, and then ignored until restart —
			// the same defect setEvents exists to prevent for the filter.
			q.setMention(t)
			bind(q)
			next = append(next, q)
			byKey[t.key] = q
			delete(previous, t.key)
			continue
		}
		q := newTargetQueue(t, m.logger, &m.shuttingDown, m.clock)
		bind(q)
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

	// Outside the lock, and BEFORE the retired targets are stopped: a batcher
	// whose target just switched to edit mode delivers the window it is
	// holding on the flip rather than dropping it — a one-embed window goes
	// out through the already-rebound dispatch and becomes that job's
	// lifecycle message; a multi-embed window posts plain (dispatchOne's
	// single-embed guard). A mode that did not change is a no-op.
	for _, mc := range modes {
		mc.q.batch.setMode(mc.mode)
	}

	// Outside the lock: stopDiscard takes the queue's own mutex and logs, and
	// so does the flush that precedes it.
	for _, q := range retired {
		// Flush the open window before the queue stops accepting. The flushed
		// items land in a queue stopDiscard then drops, with N1's own "target
		// removed — discarding its queued notifications" Warn naming the count
		// (or the drain delivers them first, if it wins the race). That is the
		// honest outcome for a webhook the operator has just deleted; losing
		// them inside the batcher, with no line anywhere, is not.
		q.batch.Stop()
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
		clock:       realBatchClock{},
	}
	// Before applyTargets, which takes the same lock itself — see publicURL.
	m.targetsMu.Lock()
	m.publicURL = cfg.Network.PublicURL
	m.targetsMu.Unlock()
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
	// Before applyTargets, for the same reason NewManager writes it first: a
	// Send that already sees the new targets must never see the old base URL.
	m.targetsMu.Lock()
	m.publicURL = cfg.Network.PublicURL
	m.targetsMu.Unlock()
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
	pub := m.publicURL
	m.targetsMu.RUnlock()
	if len(targets) == 0 {
		return
	}

	// The deep-link rewrite is install-wide, not per target, so it happens
	// once — before the queued item every target receives is built.
	//
	// With no Author there is NO rewrite: the platform page would have nowhere
	// to move to, and trading the only link to the video for a dashboard link
	// is a net loss. N2a gives every job send an Author, so that arm is the
	// pre-N2a and System-send case.
	if pub != "" && opts.JobID != "" && opts.Author != nil {
		// COPY the Author. It is a pointer the producer still owns and every
		// target shares — writing through it would leak this rewrite into the
		// caller's value and into the next send that reuses it.
		author := *opts.Author
		author.URL = opts.URL
		opts.Author = &author
		opts.URL = JobDeepLink(pub, opts.JobID)
	}

	// fields is COPIED here, once for every send rather than once per target.
	// A queued item can now sit for seconds — the old goroutine-per-send held
	// the caller's slice for microseconds — and the usual caller hands over a
	// FieldBuilder's buffer it is free to reuse for its next send. One
	// allocation per send buys the guarantee that what is delivered is what
	// was asked for.
	msg := One(title, description, ntype.Color(), append([]Field(nil), fields...), opts)
	for _, q := range targets {
		if !q.allows(opts.Event) {
			continue
		}
		// The filter runs BEFORE the coalescing stage, so a target never
		// accumulates an embed it would not have sent.
		//
		// The batcher decides immediately-or-coalesce from isBatchable(opts)
		// and re-wraps the embed into a Message on the way out, which is also
		// where the item's tier is derived (enqueueBatch) — from the embeds it
		// actually carries rather than from this one send.
		//
		// The ping lands on the MESSAGE, not on the embed's opts: Discord
		// applies content and allowed_mentions per message, so a batch of ten
		// embeds pings once. The one Embed is shared across every target, which
		// is safe because nothing mutates an Embed after One builds it (its
		// fields were already copied once above); the *AllowedMentions is built
		// once in buildTargets and never written after, so sharing that pointer
		// across targets and sends is safe too.
		mention, allowed := q.mentionFor(opts.Event)
		q.batch.Add(msg.Embeds[0], mention, allowed)
	}
}

// BeginShutdown puts every target into single-attempt mode.
//
// Owner ruling: the 15s force-exit (cmd/moombox/shutdown.go) stays, and a
// worker stop ahead of it can legitimately spend the whole window, so a
// three-attempt ladder with a 2s+5s backoff simply does not fit. One attempt
// per embed is what can be delivered, and operations.md says so rather than
// promising a drain that cannot happen.
func (m *Manager) BeginShutdown() {
	if m == nil {
		return
	}
	// The flag goes FIRST. A flushed batch is enqueued like any other item and
	// the drain goroutine can pop it the instant it lands; storing the flag
	// afterwards leaves a window in which that pop reads false and spends the
	// 2 s + 5 s retry ladder inside the process's 15 s force-exit.
	m.shuttingDown.Store(true)

	// A window open when shutdown begins is delivered, not evaporated. Flushed
	// AFTER the flag so the batch is itself single-attempt, and still before
	// Wait's closeDrain, because enqueue drops with a Warn once q.closing is
	// set (queue.go) — a flush after that would emit the batch straight into
	// the drop path. enqueue never reads shuttingDown; only deliver does (queue.go).
	m.targetsMu.RLock()
	targets := m.targets
	m.targetsMu.RUnlock()
	for _, q := range targets {
		q.batch.Flush()
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

// Wait stops accepting new notifications for every target, drains what is
// already queued, and returns when the last goroutine has exited or the wait
// timeout (defaultWaitTimeout, 30 s, unless injected) expires.
//
// The timeout is ONE deadline shared by every target, not a budget per
// target: it starts before the first queue is waited on, and the first queue
// to reach it aborts the whole wait, logging once. So a single wedged webhook
// can spend the entire window and leave the others undrained — which is the
// intent, because the thing being bounded is how long the PROCESS delays its
// exit, not how patient it is with any one webhook.
//
// **Single-call**: after Wait returns, every queue is closed and later Sends
// are dropped with a Warn. The graceful-shutdown sequence in cmd/moombox stops
// the worker — the dominant Send caller — before invoking Wait, so this is the
// correct ordering. In practice the process's own 15 s force-exit, not this
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
	// The open coalescing windows go out first. The order is not cosmetic:
	// enqueue drops with a Warn once q.closing is set (queue.go), so a flush
	// after closeDrain would emit every open window straight into the drop
	// path — including `moombox add`'s "Video Added", whose whole delivery is
	// this flush.
	for _, q := range targets {
		q.batch.Flush()
	}
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
