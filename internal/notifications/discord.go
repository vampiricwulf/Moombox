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
	"time"

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
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color,omitempty"`
	URL         string         `json:"url,omitempty"`
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

type discordImage struct {
	URL string `json:"url"`
}

type discordFooter struct {
	Text string `json:"text"`
}

// buildPayload assembles the embed JSON shared by Send and sendOnce.
func buildPayload(title, description string, color int, fields []Field, opts SendOptions) ([]byte, error) {
	embed := discordEmbed{
		Title:       title,
		Description: description,
		Color:       color,
		Footer:      &discordFooter{Text: "Moombox Go"},
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	if opts.URL != "" {
		embed.URL = opts.URL
	}

	if opts.Thumbnail != "" {
		embed.Thumbnail = &discordImage{URL: opts.Thumbnail}
	}

	if opts.Image != "" {
		embed.Image = &discordImage{URL: opts.Image}
	}

	if len(fields) > 0 {
		// discordField is a type alias for Field, so this is a direct copy
		// rather than an element-by-element conversion.
		embed.Fields = append([]discordField(nil), fields...)
	}

	body, err := json.Marshal(discordPayload{Embeds: []discordEmbed{embed}})
	if err != nil {
		return nil, fmt.Errorf("marshal discord payload: %w", err)
	}
	return body, nil
}

// sendOnce performs a single delivery attempt with NO retries — used by
// SendTest, where an interactive caller wants the immediate outcome
// (surfacing a 429 beats sleeping through its Retry-After).
func (d *DiscordWebhook) sendOnce(title, description string, color int, fields []Field, opts SendOptions) error {
	body, err := buildPayload(title, description, color, fields, opts)
	if err != nil {
		return err
	}
	status, retryAfter, err := d.post(body)
	switch {
	case err != nil:
		return fmt.Errorf("discord webhook request: %w", err)
	case status < 400:
		return nil
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
	default:
		return fmt.Errorf("discord webhook returned %d", status)
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
		status, retryAfter, err := d.post(body)

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
			lastErr = fmt.Errorf("discord webhook returned %d", status)
			delay = discordRetryBackoff[min(attempt-1, len(discordRetryBackoff)-1)]
		default:
			return fmt.Errorf("discord webhook returned %d", status)
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

// post performs one webhook POST attempt, returning the HTTP status (0 on
// transport error) and the Retry-After header value.
func (d *DiscordWebhook) post(body []byte) (status int, retryAfter string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), discordTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := discordHTTPClient.Do(req)
	if err != nil {
		// Transport errors are *url.Error, whose Error() embeds the FULL
		// request URL — i.e. the webhook token. Redact before the error
		// reaches any log line or HTTP response body (the manager's async
		// failure log, SendTest's route response, retry-loop wrap all flow
		// through here).
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return 0, "", fmt.Errorf("%s %s: %w", uerr.Op, redactURLForLog(uerr.URL), uerr.Err)
		}
		return 0, "", err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	return resp.StatusCode, resp.Header.Get("Retry-After"), nil
}
