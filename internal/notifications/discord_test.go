package notifications

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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

// TestDiscordWebhook4xxErrorQuotesTheBody pins T4-35: a rejected webhook must
// say WHY. Discord's 4xx bodies name the offending field
// ({"embeds": ["Must be 10 or fewer in length."]}); without them an operator
// sees only "discord webhook returned 400", which is unactionable.
//
// Mutants this fails on:
//   - draining the body into io.Discard without capturing it: no quote.
//   - quoting it raw: the embedded newline survives, and a remote body that
//     can forge a log line is a log-injection vector.
func TestDiscordWebhook4xxErrorQuotesTheBody(t *testing.T) {
	shrinkBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusBadRequest)
		io.WriteString(rw, "{\"embeds\":\n[\"Must be 10 or fewer in length.\"]}")
	}))
	t.Cleanup(srv.Close)

	err := (&DiscordWebhook{URL: srv.URL}).Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("400: want error, got nil")
	}
	if !strings.Contains(err.Error(), "Must be 10 or fewer in length.") {
		t.Errorf("error = %q, want the response body quoted", err)
	}
	if strings.ContainsAny(err.Error(), "\r\n") {
		t.Errorf("error = %q, must be a single line — a remote body must not forge a log line", err)
	}
}

// TestDiscordWebhookErrorBodyIsBounded pins the cap: the quote is a hint, not
// a transcript, and an error page can be arbitrarily large.
//
// Mutant: io.ReadAll without the LimitReader puts the whole page in the error
// (and in every log line and HTTP response it reaches).
func TestDiscordWebhookErrorBodyIsBounded(t *testing.T) {
	shrinkBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
		io.WriteString(rw, strings.Repeat("a", 8192))
	}))
	t.Cleanup(srv.Close)

	err := (&DiscordWebhook{URL: srv.URL}).Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("403: want error, got nil")
	}
	if n := len(err.Error()); n > discordErrBodyBytes+64 {
		t.Errorf("error is %d bytes, want <= %d — the body quote must be bounded", n, discordErrBodyBytes+64)
	}
}

// TestDiscordErrSnippet pins the sanitiser directly: control characters
// collapse to single spaces, runs collapse, and the result is trimmed.
func TestDiscordErrSnippet(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  plain  ", "plain"},
		{"line one\nline two", "line one line two"},
		{"a\r\n\tb", "a b"},
		{"{\"message\": \"Unknown Webhook\", \"code\": 10015}", `{"message": "Unknown Webhook", "code": 10015}`},
	}
	for _, tc := range cases {
		if got := discordErrSnippet([]byte(tc.in)); got != tc.want {
			t.Errorf("discordErrSnippet(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Every case above is valid UTF-8, so none of them exercise the
	// "|| r == utf8.RuneError" arm. Deleting that arm leaves the raw byte's
	// decoded rune in the snippet (written out verbatim by sb.WriteRune
	// instead of collapsed to a space) — these cases pin that a body
	// containing invalid or truncated UTF-8 still comes out clean.
	invalidCases := []struct {
		name     string
		in       []byte
		want     string
		badBytes []byte // raw bytes that must not survive into the snippet
		words    []string
	}{
		{
			name:     "raw invalid byte between words",
			in:       []byte("foo \xff bar"),
			want:     "foo bar",
			badBytes: []byte{0xff},
			words:    []string{"foo", "bar"},
		},
		{
			// "café" with the trailing "é" (0xc3 0xa9) truncated after its
			// lead byte, as a 256-byte LimitReader cut would do mid-rune.
			name:     "multi-byte rune cut in half at the end",
			in:       []byte("err: caf\xc3"),
			want:     "err: caf",
			badBytes: []byte{0xc3},
			words:    []string{"err:", "caf"},
		},
		{
			name:     "control character and invalid byte together",
			in:       []byte("tab\t\xff word"),
			want:     "tab word",
			badBytes: []byte{0xff},
			words:    []string{"tab", "word"},
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			got := discordErrSnippet(tc.in)
			if got != tc.want {
				t.Errorf("discordErrSnippet(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("discordErrSnippet(%q) = %q, not valid UTF-8", tc.in, got)
			}
			for _, bad := range tc.badBytes {
				if bytes.IndexByte([]byte(got), bad) != -1 {
					t.Errorf("discordErrSnippet(%q) = %q, raw byte %#x survived", tc.in, got, bad)
				}
			}
			for _, word := range tc.words {
				if !strings.Contains(got, word) {
					t.Errorf("discordErrSnippet(%q) = %q, missing surrounding word %q", tc.in, got, word)
				}
			}
		})
	}
}

// TestDiscordWebhookErrorBodyAtCapIsNotTruncated pins the boundary: a body of
// EXACTLY discordErrBodyBytes must appear whole in the error, not truncated
// by an off-by-one in the LimitReader size.
func TestDiscordWebhookErrorBodyAtCapIsNotTruncated(t *testing.T) {
	shrinkBackoff(t)
	body := strings.Repeat("a", discordErrBodyBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
		io.WriteString(rw, body)
	}))
	t.Cleanup(srv.Close)

	err := (&DiscordWebhook{URL: srv.URL}).Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("403: want error, got nil")
	}
	if !strings.Contains(err.Error(), body) {
		t.Errorf("error = %q, want the full %d-byte body quoted untruncated", err, discordErrBodyBytes)
	}
}

// TestDiscordWebhookErrorBodyCapSplitsAMultiByteRune pins the case where the
// discordErrBodyBytes cut lands mid-rune: 255 ASCII bytes followed by "é" (a
// 2-byte rune) means the LimitReader hands discordErrSnippet only "é"'s lead
// byte. The error must still be valid UTF-8 and must not end with that
// stray lead byte.
func TestDiscordWebhookErrorBodyCapSplitsAMultiByteRune(t *testing.T) {
	shrinkBackoff(t)
	prefix := strings.Repeat("a", discordErrBodyBytes-1)
	body := prefix + "é" // total 257 bytes; the 256-byte cut lands after "é"'s lead byte
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
		io.WriteString(rw, body)
	}))
	t.Cleanup(srv.Close)

	err := (&DiscordWebhook{URL: srv.URL}).Send("t", "d", 0, nil, SendOptions{})
	if err == nil {
		t.Fatal("403: want error, got nil")
	}
	if !utf8.ValidString(err.Error()) {
		t.Errorf("error = %q, not valid UTF-8", err)
	}
	if !strings.Contains(err.Error(), prefix) {
		t.Errorf("error = %q, missing the 255-byte ASCII prefix", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte{0xc3}) {
		t.Errorf("error = %q, ends with a stray lead byte of the truncated multi-byte rune", err)
	}
}
