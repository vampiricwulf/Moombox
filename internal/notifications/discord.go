package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/httpx"
)

const (
	discordTimeout = 15 * time.Second
	// discordMaxAttempts bounds total delivery attempts (first try +
	// retries) across transport errors, Discord 5xx, and 429 responses.
	discordMaxAttempts = 3
	// discordRetryAfterCap rejects absurd 429 Retry-After values — and bounds
	// the pre-emptive empty-bucket sleep — rather than parking a target's
	// whole queue into next week.
	discordRetryAfterCap = 30 * time.Second
	// discordMaxSleepTotal caps CUMULATIVE inter-attempt sleep, bounding one
	// notification's worst-case hold on its target's queue at ~3x15s requests
	// + 30s sleep. Two large Retry-After waits would exceed it — a webhook
	// still rate-limited after one honored wait gives up instead.
	discordMaxSleepTotal = 30 * time.Second
	// discordErrBodyBytes bounds how much of a rejected webhook's response
	// body is quoted back in the error. Discord's 4xx bodies are small JSON
	// objects naming what it refused ({"message": "Unknown Webhook", "code":
	// 10015}); without them an operator sees only "discord webhook returned
	// 400" and has nothing to act on. An error PAGE, though, can be
	// arbitrarily large, and this error reaches log files and SendTest's HTTP
	// response.
	discordErrBodyBytes = 256
)

// discordRetryBackoff is the inter-attempt sleep schedule for transport
// errors and 5xx responses (429 uses the server's Retry-After instead).
// Package var so tests can shrink it.
var discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{2 * time.Second, 5 * time.Second}

// discordHTTPClient sends Discord webhook requests via the shared
// httpx transport. Per-request timeout is enforced by the context; the
// client-level Timeout (2 * discordTimeout = 30s) is a belt-and-
// suspenders cap in case a retry/redirect chain slips past the
// request context.
var discordHTTPClient = httpx.Client(2 * discordTimeout)

// DiscordWebhook sends notifications via Discord webhook.
//
// One instance per target, held by that target's queue (manager.go), so the
// bucket state below is learned once and reused for every embed to that
// webhook — including across a config hot-reload, because applyTargets keeps a
// surviving target's queue and therefore this pointer.
type DiscordWebhook struct {
	URL string

	// bucketMu guards bucketRefillsAt. One sender goroutine per target means
	// there is no contention in production; the mutex is for SendTest (which
	// builds its own instance) and for the race detector.
	bucketMu sync.Mutex
	// bucketRefillsAt is when this webhook's rate bucket next has room, set
	// from an X-RateLimit-Remaining: 0 response. Zero means "not known empty".
	bucketRefillsAt time.Time
}

type discordPayload struct {
	// Content is the only place a mention can live: per Discord API docs
	// (resources/message.mdx) allowed_mentions governs "mentions in the
	// message content, or components", so an embed can never ping anyone.
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds"`
	// AllowedMentions is the EXPORTED notifications.AllowedMentions
	// (manager.go), not a payload-private twin: Arc N2b resolves one per
	// (target, event) and hands it over in SendOptions, so the wire shape and
	// the option are the same type by construction.
	AllowedMentions *AllowedMentions `json:"allowed_mentions,omitempty"`
}

type discordEmbed struct {
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color,omitempty"`
	URL         string         `json:"url,omitempty"`
	Author      *discordAuthor `json:"author,omitempty"`
	Fields      []discordField `json:"fields,omitempty"`
	Thumbnail   *discordImage  `json:"thumbnail,omitempty"`
	Image       *discordImage  `json:"image,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

// discordField is a type alias for Field so the Discord JSON encoder can
// share the same struct without keeping a parallel duplicate definition.
// Field carries the JSON tags directly. Audit reports/small-packages.md.
type discordField = Field

type discordAuthor struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

type discordImage struct {
	URL string `json:"url"`
}

type discordFooter struct {
	Text string `json:"text"`
}

// footerText renders the embed footer. "Moombox Go" was a rewrite-era suffix
// that identified nothing; with several channels and several jobs landing in
// one Discord channel, the platform and the job id are what let an operator
// tell two embeds apart without opening the link.
func footerText(opts SendOptions) string {
	text := "Moombox"
	if opts.Platform != "" {
		text += " · " + opts.Platform
	}
	if opts.JobID != "" {
		text += " · " + opts.JobID
	}
	return text
}

// MentionParse maps a configured mention to the allowed_mentions object that
// makes it actually ping, or nil for a form we do not recognise — which keeps
// an unvalidated config string from becoming an unrestricted ping. Arc N2b
// calls it when it fills SendOptions.
func MentionParse(mention string) *AllowedMentions {
	switch {
	case mention == "@everyone" || mention == "@here":
		return &AllowedMentions{Parse: []string{"everyone"}}
	case strings.HasPrefix(mention, "<@&") && strings.HasSuffix(mention, ">"):
		id := strings.TrimSuffix(strings.TrimPrefix(mention, "<@&"), ">")
		if id == "" {
			return nil
		}
		return &AllowedMentions{Parse: []string{}, Roles: []string{id}}
	case strings.HasPrefix(mention, "<@") && strings.HasSuffix(mention, ">"):
		id := strings.TrimSuffix(strings.TrimPrefix(mention, "<@"), ">")
		// <@!id> is the legacy nickname form; Discord still accepts it in
		// content and the id is what allowed_mentions needs either way.
		id = strings.TrimPrefix(id, "!")
		if id == "" {
			return nil
		}
		return &AllowedMentions{Parse: []string{}, Users: []string{id}}
	}
	return nil
}

// buildPayload assembles the embed JSON shared by Send and SendOnce.
func buildPayload(title, description string, color int, fields []Field, opts SendOptions) ([]byte, error) {
	embed := discordEmbed{
		Title:       title,
		Description: description,
		Color:       color,
		Footer:      &discordFooter{Text: footerText(opts)},
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	if opts.URL != "" {
		embed.URL = opts.URL
	}

	// Discord rejects an author object with no name (a permanent 400), so a
	// half-filled Author is dropped rather than sent.
	if opts.Author != nil && opts.Author.Name != "" {
		embed.Author = &discordAuthor{
			Name:    opts.Author.Name,
			URL:     opts.Author.URL,
			IconURL: opts.Author.IconURL,
		}
	}

	if opts.Thumbnail != "" {
		embed.Thumbnail = &discordImage{URL: opts.Thumbnail}
	}

	if opts.Image != "" {
		embed.Image = &discordImage{URL: opts.Image}
	}

	if len(fields) > 0 {
		// discordField is a type alias for Field, so this is a direct copy
		// rather than an element-by-element conversion. It MUST stay a copy:
		// the clamp below rewrites values in place, and the caller's slice is
		// often a FieldBuilder's buffer reused for a later send.
		embed.Fields = append([]discordField(nil), fields...)
	}

	// One clamp for all ~36 send sites. Over ANY Discord limit is a 400, and
	// the ladder below treats a non-429 4xx as permanent, so an unclamped
	// embed is a silently dropped alert.
	clampEmbed(&embed)

	payload := discordPayload{Embeds: []discordEmbed{embed}}

	// A mention rides the message content, never the embed. A nil
	// MentionAllowed is the per-event gate N2b fills from mention_events:
	// no object, no ping. MentionParse is where an unrecognised form becomes
	// that nil.
	if opts.MentionAllowed != nil && opts.Mention != "" {
		payload.Content = opts.Mention
		payload.AllowedMentions = opts.MentionAllowed
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal discord payload: %w", err)
	}
	return body, nil
}

// SendOnce performs a single delivery attempt with NO retries.
//
// Two callers: SendTest, where an interactive settings flow wants the
// immediate outcome (surfacing a 429 beats sleeping through its Retry-After),
// and the per-target queue during shutdown, where the 10s force-exit leaves no
// room for the ladder. Bounded by the single-attempt request timeout (~15s).
func (d *DiscordWebhook) SendOnce(title, description string, color int, fields []Field, opts SendOptions) error {
	body, err := buildPayload(title, description, color, fields, opts)
	if err != nil {
		return err
	}
	d.waitForBucket()
	r, err := d.post(body)
	d.noteBucket(r)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case r.status < 400:
		return nil
	case r.status == http.StatusTooManyRequests:
		return fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
	default:
		return discordStatusErr(r.status, r.snippet)
	}
}

// Send sends a Discord webhook embed.
func (d *DiscordWebhook) Send(title, description string, color int, fields []Field, opts SendOptions) error {
	body, err := buildPayload(title, description, color, fields, opts)
	if err != nil {
		return err
	}

	// Bounded delivery loop: transport errors and Discord 5xx retry on the
	// fixed backoff schedule, 429 honors a validated Retry-After, other 4xx
	// are permanent (bad payload, revoked webhook — retrying just spams).
	// Alerts exist precisely for flaky moments; the previous single-shot
	// behavior dropped e.g. a "recording failed" embed on the first
	// connection reset.
	var lastErr error
	var slept time.Duration
	for attempt := 1; ; attempt++ {
		// Pre-emptive: if the last response said the bucket was empty, wait
		// out its window instead of spending one of three attempts on the 429
		// Discord has already promised.
		d.waitForBucket()
		r, err := d.post(body)
		d.noteBucket(r)

		var delay time.Duration
		switch {
		case err != nil:
			lastErr = fmt.Errorf("discord webhook request: %w", err)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		case r.status < 400:
			return nil
		case r.status == http.StatusTooManyRequests:
			// Validate in FLOAT space before converting: values past ~9.2e9s
			// (or Inf) overflow time.Duration to a NEGATIVE on amd64, which
			// would slip past a Duration-space cap check and turn the sleep
			// into a zero-delay hammer. !(secs > 0) is deliberately NaN-proof.
			secs, parseErr := strconv.ParseFloat(r.retryAfter, 64)
			if parseErr != nil || !(secs > 0) || secs > discordRetryAfterCap.Seconds() {
				// Missing, malformed, or absurd Retry-After — surface the
				// 429 directly rather than guessing a sleep.
				return fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
			}
			lastErr = fmt.Errorf("discord rate limited (retry-after: %s)", r.retryAfter)
			delay = time.Duration(secs * float64(time.Second))
		case r.status >= 500:
			lastErr = discordStatusErr(r.status, r.snippet)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		default:
			return discordStatusErr(r.status, r.snippet)
		}

		if attempt == discordMaxAttempts {
			return fmt.Errorf("%w (gave up after %d attempts)", lastErr, discordMaxAttempts)
		}
		if slept+delay > discordMaxSleepTotal {
			// Cumulative-sleep budget exhausted (e.g. a second large
			// Retry-After) — bound this target queue's hold instead of
			// waiting out an extended rate-limit.
			return fmt.Errorf("%w (retry budget exhausted after %d attempts)", lastErr, attempt)
		}
		slept += delay
		time.Sleep(delay)
	}
}

// discordErrSnippet renders a response-body prefix as a single-line, printable
// error fragment. The body is REMOTE input that lands in log lines and in
// SendTest's HTTP response, so newlines (which would forge a log line), other
// control characters, and the replacement rune a truncated multi-byte tail
// decodes to all collapse to spaces; runs of whitespace collapse to one.
func discordErrSnippet(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, r := range string(b) {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			sb.WriteByte(' ')
			continue
		}
		sb.WriteRune(r)
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// discordStatusErr renders a non-2xx webhook response, quoting the body prefix
// when Discord sent one. Used by every site that reports a status — 4xx and
// 5xx alike: the 5xx text is what an operator sees after the retry budget is
// spent, which is precisely when the reason matters.
func discordStatusErr(status int, snippet string) error {
	if snippet == "" {
		return fmt.Errorf("discord webhook returned %d", status)
	}
	return fmt.Errorf("discord webhook returned %d: %s", status, snippet)
}

// waitForBucket honours what Discord last told us about this webhook's rate
// bucket: an X-RateLimit-Remaining of 0 means the NEXT request 429s until the
// window resets, so sleeping through it costs one wait and saves an attempt
// out of the three this notification has. Per Discord API docs
// (topics/rate-limits.mdx) the bucket is discoverable only from these headers —
// there is no published numeric cap.
//
// Capped by discordRetryAfterCap for the same reason a 429's Retry-After is:
// an absurd reset must not park a target's whole queue into next week.
func (d *DiscordWebhook) waitForBucket() {
	d.bucketMu.Lock()
	until := d.bucketRefillsAt
	d.bucketMu.Unlock()
	if until.IsZero() {
		return
	}
	wait := time.Until(until)
	if wait <= 0 {
		return
	}
	if wait > discordRetryAfterCap {
		wait = discordRetryAfterCap
	}
	time.Sleep(wait)
}

// noteBucket records (or clears) the empty-bucket deadline from one response.
//
// A 429 is deliberately EXCLUDED: Discord sets Remaining: 0 on one, and the
// ladder already honours its Retry-After, so arming the pre-emptive sleep from
// the same response would wait the window twice and spend the cumulative-sleep
// budget on the duplicate.
func (d *DiscordWebhook) noteBucket(r discordResponse) {
	if r.status == http.StatusTooManyRequests || r.rateRemain == "" {
		return
	}
	remaining, err := strconv.Atoi(r.rateRemain)
	if err != nil {
		return
	}
	if remaining > 0 {
		d.bucketMu.Lock()
		d.bucketRefillsAt = time.Time{}
		d.bucketMu.Unlock()
		return
	}
	// Validate in FLOAT space before converting, for the reason the 429 arm
	// documents: a value past ~9.2e9s (or Inf) overflows time.Duration to a
	// NEGATIVE on amd64. !(secs > 0) is deliberately NaN-proof.
	secs, err := strconv.ParseFloat(r.rateReset, 64)
	if err != nil || !(secs > 0) {
		return
	}
	if secs > discordRetryAfterCap.Seconds() {
		secs = discordRetryAfterCap.Seconds()
	}
	d.bucketMu.Lock()
	d.bucketRefillsAt = time.Now().Add(time.Duration(secs * float64(time.Second)))
	d.bucketMu.Unlock()
}

// discordResponse is what one attempt learned. Grouped rather than returned as
// five bare values: the rate-limit pair has to travel with the status, and a
// six-value signature is where a caller starts transposing arguments.
type discordResponse struct {
	status     int    // 0 on a transport error
	retryAfter string // Retry-After, 429 only
	rateRemain string // X-RateLimit-Remaining
	rateReset  string // X-RateLimit-Reset-After, in seconds
	snippet    string // sanitised body prefix, >= 400 only
}

// post performs one webhook POST attempt.
func (d *DiscordWebhook) post(body []byte) (discordResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discordTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return discordResponse{}, fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := discordHTTPClient.Do(req)
	if err != nil {
		// Transport errors are *url.Error, whose Error() embeds the FULL
		// request URL — i.e. the webhook token. Redact before the error
		// reaches any log line or HTTP response body (the queue's failure
		// log, SendTest's route response, retry-loop wrap all flow through
		// here).
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			return discordResponse{}, fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
		}
		return discordResponse{}, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	out := discordResponse{
		status:     resp.StatusCode,
		retryAfter: resp.Header.Get("Retry-After"),
		rateRemain: resp.Header.Get("X-RateLimit-Remaining"),
		rateReset:  resp.Header.Get("X-RateLimit-Reset-After"),
	}
	if resp.StatusCode >= 400 {
		// Read the reason BEFORE the deferred drain throws the rest away.
		// The read error is intentionally ignored: a partial read (e.g. the
		// connection drops mid-body) still yields whatever prefix arrived,
		// which is a usable snippet — better than discarding it outright.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, discordErrBodyBytes))
		out.snippet = discordErrSnippet(b)
	}
	return out, nil
}
