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

// discordHTTPClient is a shared HTTP client with a timeout for Discord webhook requests.
var discordHTTPClient = &http.Client{Timeout: 30 * time.Second}

// DiscordWebhook sends notifications via Discord webhook.
type DiscordWebhook struct {
	URL    string
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
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

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

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
		embed.Fields = make([]discordField, len(fields))
		for i, f := range fields {
			embed.Fields[i] = discordField{
				Name:   f.Name,
				Value:  f.Value,
				Inline: f.Inline,
			}
		}
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
		if secs, parseErr := strconv.ParseFloat(retryAfter, 64); parseErr == nil && secs > 0 && secs <= 30 {
			time.Sleep(time.Duration(secs * float64(time.Second)))
			// Retry once with a fresh context and request (original body/context were consumed)
			retryCtx, retryCancel := context.WithTimeout(context.Background(), discordTimeout)
			defer retryCancel()
			retryReq, retryErr := http.NewRequestWithContext(retryCtx, http.MethodPost, d.URL, bytes.NewReader(body))
			if retryErr == nil {
				retryReq.Header.Set("Content-Type", "application/json")
				resp2, doErr := discordHTTPClient.Do(retryReq)
				if doErr == nil {
					defer func() {
						io.Copy(io.Discard, resp2.Body)
						resp2.Body.Close()
					}()
					if resp2.StatusCode < 400 {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("discord rate limited (retry-after: %s)", retryAfter)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord webhook returned %d", resp.StatusCode)
	}

	return nil
}
