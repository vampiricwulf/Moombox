package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const discordTimeout = 15 * time.Second

// discordHTTPClient is a shared HTTP client for Discord webhook requests.
// Per-request timeout is enforced by the context; the client-level Timeout
// is a belt-and-suspenders cap in case a retry/redirect chain slips past
// the request context.
var discordHTTPClient = &http.Client{Timeout: 2 * discordTimeout}

// DiscordWebhook sends notifications via Discord webhook.
type DiscordWebhook struct {
	URL string
}

type discordPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	URL         string              `json:"url,omitempty"`
	Fields      []discordField      `json:"fields,omitempty"`
	Thumbnail   *discordImage       `json:"thumbnail,omitempty"`
	Image       *discordImage       `json:"image,omitempty"`
	Footer      *discordFooter      `json:"footer,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
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

// Send sends a Discord webhook embed.
func (d *DiscordWebhook) Send(title, description string, color int, fields []Field, opts SendOptions) error {
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

	payload := discordPayload{
		Embeds: []discordEmbed{embed},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), discordTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := discordHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("discord webhook request: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// Handle Discord rate limiting (429) with retry-after
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")
		secs, parseErr := strconv.ParseFloat(retryAfter, 64)
		if parseErr != nil || secs <= 0 || secs > 30 {
			// retry-after missing, malformed, or beyond our ceiling — surface the 429 directly
			return fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
		}
		time.Sleep(time.Duration(secs * float64(time.Second)))
		// Retry once with a fresh context and request (original body/context were consumed)
		retryCtx, retryCancel := context.WithTimeout(context.Background(), discordTimeout)
		defer retryCancel()
		retryReq, retryErr := http.NewRequestWithContext(retryCtx, http.MethodPost, d.URL, bytes.NewReader(body))
		if retryErr != nil {
			return fmt.Errorf("create discord retry request: %w", retryErr)
		}
		retryReq.Header.Set("Content-Type", "application/json")
		resp2, doErr := discordHTTPClient.Do(retryReq)
		if doErr != nil {
			return fmt.Errorf("discord webhook retry request: %w", doErr)
		}
		defer func() {
			io.Copy(io.Discard, resp2.Body)
			resp2.Body.Close()
		}()
		if resp2.StatusCode < 400 {
			return nil
		}
		if resp2.StatusCode == http.StatusTooManyRequests {
			// second 429 — report the repeated rate limit with the new retry-after
			return fmt.Errorf("discord rate limited after retry (retry-after: %s)", resp2.Header.Get("Retry-After"))
		}
		return fmt.Errorf("discord webhook returned %d after retry", resp2.StatusCode)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord webhook returned %d", resp.StatusCode)
	}

	return nil
}
