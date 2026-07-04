package notifications

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDiscordWebhookSendsValidPayload verifies that a successful Send
// posts a well-formed Discord embed payload with the expected fields,
// title, description, color, and footer. Audit reports/small-packages.md
// notifications discord httptest.
func TestDiscordWebhookSendsValidPayload(t *testing.T) {
	var captured atomic.Pointer[discordPayload]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p discordPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("server got malformed JSON: %v", err)
			http.Error(rw, "bad", http.StatusBadRequest)
			return
		}
		captured.Store(&p)
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	err := d.Send("My Title", "Some body", 0x123456,
		[]Field{{Name: "k", Value: "v", Inline: true}},
		SendOptions{URL: "https://example.com", Event: "test_event"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := captured.Load()
	if got == nil {
		t.Fatal("server did not capture payload")
	}
	if len(got.Embeds) != 1 {
		t.Fatalf("want 1 embed, got %d", len(got.Embeds))
	}
	e := got.Embeds[0]
	if e.Title != "My Title" {
		t.Errorf("title: want %q, got %q", "My Title", e.Title)
	}
	if e.Description != "Some body" {
		t.Errorf("description: want %q, got %q", "Some body", e.Description)
	}
	if e.Color != 0x123456 {
		t.Errorf("color: want 0x123456, got 0x%06x", e.Color)
	}
	if e.URL != "https://example.com" {
		t.Errorf("url: want example.com, got %q", e.URL)
	}
	if len(e.Fields) != 1 || e.Fields[0].Name != "k" || e.Fields[0].Value != "v" || !e.Fields[0].Inline {
		t.Errorf("fields: want [{k v inline}], got %+v", e.Fields)
	}
	if e.Footer == nil || !strings.Contains(e.Footer.Text, "Moombox") {
		t.Errorf("footer: want 'Moombox' text, got %+v", e.Footer)
	}
}

// TestDiscordWebhookHTTPErrorReturnsErr covers the >= 400 branch with no
// retry-after — Send must surface the status code in the error.
func TestDiscordWebhookHTTPErrorReturnsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		http.Error(rw, "bad", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	err := d.Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("400 response: want error, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should contain status code, got %v", err)
	}
}

// TestDiscordWebhookRateLimitRetriesOnce covers the 429 → wait → retry
// path. The server returns 429 on the first request and 204 on the
// second, with a small Retry-After value to keep the test fast.
func TestDiscordWebhookRateLimitRetriesOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			rw.Header().Set("Retry-After", "0.1")
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	if err := d.Send("t", "d", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("Send (with one retry): %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("expected exactly 2 hits (initial + retry), got %d", got)
	}
}

// shrinkBackoff makes the retry backoff schedule test-fast, restoring it
// on cleanup.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	orig := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{5 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = orig })
}

// TestDiscordWebhook5xxRetriesThenSucceeds covers the transient-server-error
// path: two 500s followed by a 204 must succeed on the third attempt.
func TestDiscordWebhook5xxRetriesThenSucceeds(t *testing.T) {
	shrinkBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	if err := d.Send("t", "d", 0, nil, SendOptions{}); err != nil {
		t.Fatalf("Send (5xx twice then success): %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("expected 3 hits, got %d", got)
	}
}

// TestDiscordWebhook5xxGivesUpAfterMaxAttempts pins the attempt bound: a
// persistently failing server gets exactly discordMaxAttempts requests.
func TestDiscordWebhook5xxGivesUpAfterMaxAttempts(t *testing.T) {
	shrinkBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	err := d.Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("persistent 502: want error, got nil")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error should carry status and give-up note, got %v", err)
	}
	if got := hits.Load(); got != discordMaxAttempts {
		t.Errorf("expected exactly %d hits, got %d", discordMaxAttempts, got)
	}
}

// TestDiscordWebhookTransportErrorRetries covers the connection-level
// failure path: a server that dies after the first attempt must be retried
// (the request errors are transport errors, not HTTP statuses).
func TestDiscordWebhookTransportErrorRetries(t *testing.T) {
	shrinkBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNoContent)
	}))
	url := srv.URL
	srv.Close() // all connections now refused

	d := &DiscordWebhook{URL: url}
	err := d.Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("dead server: want error, got nil")
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Errorf("transport errors should be retried to the attempt bound, got %v", err)
	}
}

// TestDiscordWebhook4xxIsPermanent pins that non-429 4xx responses are NOT
// retried — resending a rejected payload just spams Discord.
func TestDiscordWebhook4xxIsPermanent(t *testing.T) {
	shrinkBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusNotFound) // revoked webhook
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	if err := d.Send("t", "d", 0, nil, SendOptions{}); err == nil {
		t.Fatal("404: want error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("4xx must not retry: hits=%d", got)
	}
}

// TestDiscordWebhookRateLimitRefusesUnreasonableRetryAfter verifies that a
// Retry-After beyond the 30s ceiling fast-fails after exactly one request.
// The overflow cases matter: values past ~9.2e9s (or Inf/NaN) overflow
// time.Duration to a NEGATIVE on amd64 — a Duration-space cap comparison
// silently accepted them and hammered the rate-limited webhook with
// zero-delay retries. The cap must be checked in float space.
func TestDiscordWebhookRateLimitRefusesUnreasonableRetryAfter(t *testing.T) {
	for _, retryAfter := range []string{"9999", "9999999999999", "1e300", "Inf", "NaN", "-5", ""} {
		t.Run("retryAfter="+retryAfter, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				rw.Header().Set("Retry-After", retryAfter)
				rw.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			d := &DiscordWebhook{URL: srv.URL}
			start := time.Now()
			err := d.Send("t", "d", 0, nil, SendOptions{})
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("bogus Retry-After: want error, got nil")
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("server should NOT be retried with bogus Retry-After: hits=%d", got)
			}
			if elapsed > 500*time.Millisecond {
				t.Errorf("Send slept for %v despite refusing the Retry-After value", elapsed)
			}
		})
	}
}
