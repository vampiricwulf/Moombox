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
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/httpx"
)

const (
	discordTimeout = 15 * time.Second
	// discordMaxAttempts bounds total delivery attempts (first try +
	// retries) across transport errors, Discord 5xx, and 429 responses.
	discordMaxAttempts = 3
	// discordRetryAfterCap rejects absurd 429 Retry-After values rather
	// than sleeping the goroutine (and its manager semaphore slot) into
	// next week.
	discordRetryAfterCap = 30 * time.Second
	// discordMaxSleepTotal caps CUMULATIVE inter-attempt sleep, bounding
	// one notification's worst-case semaphore hold at ~3×15s requests +
	// 30s sleep. Two large Retry-After waits would exceed it — a webhook
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
type DiscordWebhook struct {
	URL string
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
	status, retryAfter, snippet, err := d.post(body)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case status < 400:
		return nil
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
	default:
		return discordStatusErr(status, snippet)
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
		status, retryAfter, snippet, err := d.post(body)

		var delay time.Duration
		switch {
		case err != nil:
			lastErr = fmt.Errorf("discord webhook request: %w", err)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		case status < 400:
			return nil
		case status == http.StatusTooManyRequests:
			// Validate in FLOAT space before converting: values past ~9.2e9s
			// (or Inf) overflow time.Duration to a NEGATIVE on amd64, which
			// would slip past a Duration-space cap check and turn the sleep
			// into a zero-delay hammer. !(secs > 0) is deliberately NaN-proof.
			secs, parseErr := strconv.ParseFloat(retryAfter, 64)
			if parseErr != nil || !(secs > 0) || secs > discordRetryAfterCap.Seconds() {
				// Missing, malformed, or absurd Retry-After — surface the
				// 429 directly rather than guessing a sleep.
				return fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
			}
			lastErr = fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
			delay = time.Duration(secs * float64(time.Second))
		case status >= 500:
			lastErr = discordStatusErr(status, snippet)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		default:
			return discordStatusErr(status, snippet)
		}

		if attempt == discordMaxAttempts {
			return fmt.Errorf("%w (gave up after %d attempts)", lastErr, discordMaxAttempts)
		}
		if slept+delay > discordMaxSleepTotal {
			// Cumulative-sleep budget exhausted (e.g. a second large
			// Retry-After) — bound the semaphore-slot hold instead of
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

// post performs one webhook POST attempt, returning the HTTP status (0 on
// transport error), the Retry-After header value, and — for a >=400 status —
// a sanitised prefix of the response body.
func (d *DiscordWebhook) post(body []byte) (status int, retryAfter, snippet string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), discordTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := discordHTTPClient.Do(req)
	if err != nil {
		// Transport errors are *url.Error, whose Error() embeds the FULL
		// request URL — i.e. the webhook token. Redact before the error
		// reaches any log line or HTTP response body (the manager's async
		// failure log, SendTest's route response, retry-loop wrap all flow
		// through here).
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			return 0, "", "", fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
		}
		return 0, "", "", err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		// Read the reason BEFORE the deferred drain throws the rest away.
		// The read error is intentionally ignored: a partial read (e.g. the
		// connection drops mid-body) still yields whatever prefix arrived,
		// which is a usable snippet — better than discarding it outright.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, discordErrBodyBytes))
		snippet = discordErrSnippet(b)
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), snippet, nil
}
