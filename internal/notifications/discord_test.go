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

// TestDiscordWebhookRateLimitRefusesUnreasonableRetryAfter verifies
// that a Retry-After beyond the 30s ceiling returns an error rather
// than sleeping for an absurd duration.
func TestDiscordWebhookRateLimitRefusesUnreasonableRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.Header().Set("Retry-After", "9999") // far past the 30s cap
		rw.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	d := &DiscordWebhook{URL: srv.URL}
	start := time.Now()
	err := d.Send("t", "d", 0, nil, SendOptions{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Retry-After=9999: want error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server should NOT be retried with bogus Retry-After: hits=%d", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Send slept for %v despite refusing the Retry-After value", elapsed)
	}
}
