package notifications

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedReq is one request the fake Discord saw.
type recordedReq struct {
	Method string
	Path   string
	Query  string
	Body   discordPayload
}

// fakeDiscord is an httptest server that behaves like a webhook endpoint:
// it records every request and answers from a per-call script. Shared by this
// suite and lifecycle_state_test.go (Task 7).
type fakeDiscord struct {
	mu   sync.Mutex
	reqs []recordedReq
	srv  *httptest.Server
	// handler answers one request; n is the 0-based call index.
	handler func(n int, r recordedReq, rw http.ResponseWriter)
}

func newFakeDiscord(t *testing.T, handler func(n int, r recordedReq, rw http.ResponseWriter)) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		rec := recordedReq{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery}
		_ = json.Unmarshal(raw, &rec.Body)
		f.mu.Lock()
		n := len(f.reqs)
		f.reqs = append(f.reqs, rec)
		f.mu.Unlock()
		f.handler(n, rec, rw)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) URL() string { return f.srv.URL }

func (f *fakeDiscord) calls() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedReq, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// okCreated answers every POST with a created message carrying id, and every
// PATCH with 204.
func okCreated(id string) func(int, recordedReq, http.ResponseWriter) {
	return func(_ int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch {
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		io.WriteString(rw, `{"id":"`+id+`","type":0,"content":""}`)
	}
}

// TestPostWaitAsksForTheMessageAndReturnsItsID: without ?wait=true Discord
// answers 204 with no body (per the API docs), so the id would be
// unrecoverable and edit mode could never start.
//
// MUTANT: drop the "?wait=true" suffix — the fake sees an empty query and this
// fails on the Query assertion before the id one even runs.
func TestPostWaitAsksForTheMessageAndReturnsItsID(t *testing.T) {
	f := newFakeDiscord(t, okCreated("1418889000000000001"))
	d := &DiscordWebhook{URL: f.URL()}

	body, err := buildPayload(One("t", "d", 0x1abc9c, nil, SendOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.postWait(body)
	if err != nil {
		t.Fatalf("postWait: %v", err)
	}
	if id != "1418889000000000001" {
		t.Errorf("message id = %q, want 1418889000000000001", id)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 request, got %d", len(calls))
	}
	if calls[0].Method != http.MethodPost {
		t.Errorf("method = %s, want POST", calls[0].Method)
	}
	if calls[0].Query != "wait=true" {
		t.Errorf("query = %q, want wait=true", calls[0].Query)
	}
}

// TestPostWaitRejectsAResponseWithNoID: a 2xx whose body carries no id means
// we would store "" and every later PATCH would hit /messages/ — better to
// fail the send and let the next event post again.
func TestPostWaitRejectsAResponseWithNoID(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusOK)
		io.WriteString(rw, `{"type":0}`)
	})
	d := &DiscordWebhook{URL: f.URL()}
	body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
	if _, err := d.postWait(body); err == nil {
		t.Fatal("postWait accepted a response with no id")
	}
}

// TestPatchMessageTargetsTheMessageRoute pins the edit endpoint shape from the
// Discord API docs: PATCH /webhooks/{id}/{token}/messages/{message_id}.
func TestPatchMessageTargetsTheMessageRoute(t *testing.T) {
	f := newFakeDiscord(t, okCreated("x"))
	// A realistic webhook path on the fake's host, so the asserted route is
	// the full .../webhooks/ID/TOKEN/messages/ID shape Discord publishes.
	d := &DiscordWebhook{URL: f.URL() + "/api/webhooks/123/tok"}

	body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
	if err := d.patchMessage("999", body); err != nil {
		t.Fatalf("patchMessage: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 request, got %d", len(calls))
	}
	if calls[0].Method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", calls[0].Method)
	}
	if calls[0].Path != "/api/webhooks/123/tok/messages/999" {
		t.Errorf("path = %q, want /api/webhooks/123/tok/messages/999", calls[0].Path)
	}
	if calls[0].Query != "" {
		t.Errorf("PATCH carried query %q — the edit route takes none", calls[0].Query)
	}
}

// TestPatchMessageUnknownMessageIsRecoverable: 404 + code 10008 is the ONE 4xx
// on the edit path that is not permanent — the caller posts a new message and
// overwrites the id.
//
// MUTANT: treat any 404 as recoverable — the Unknown Webhook row below then
// also reports ErrUnknownMessage, and a revoked webhook would be re-POSTed to
// forever.
func TestPatchMessageUnknownMessageIsRecoverable(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		body           string
		wantErrUnknown bool
	}{
		{"unknown message", http.StatusNotFound, `{"message":"Unknown Message","code":10008}`, true},
		{"unknown webhook", http.StatusNotFound, `{"message":"Unknown Webhook","code":10015}`, false},
		{"bare 404", http.StatusNotFound, ``, false},
		{"bad request", http.StatusBadRequest, `{"message":"Invalid Form Body","code":50035}`, false},
		// A refusal that echoes the requested path: the message snowflake
		// ends in the digits 10008, and a substring test on the digits reads
		// this revoked webhook as recoverable and re-POSTs to it forever.
		{"unknown webhook echoing a 10008 id", http.StatusNotFound, `{"message":"Unknown Webhook","code":10015,"path":"/messages/1418889000000010008"}`, false},
		// The code field still counts when the body is spaced out — and
		// discordErrSnippet collapses whitespace runs, so the matcher has to
		// tolerate what it leaves behind.
		{"spaced code field", http.StatusNotFound, `{"message":"Gone", "code": 10008}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
				rw.WriteHeader(tc.status)
				io.WriteString(rw, tc.body)
			})
			d := &DiscordWebhook{URL: f.URL()}
			body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
			err := d.patchMessage("999", body)
			if err == nil {
				t.Fatal("patchMessage accepted a refusal")
			}
			if got := errors.Is(err, ErrUnknownMessage); got != tc.wantErrUnknown {
				t.Errorf("errors.Is(err, ErrUnknownMessage) = %v, want %v (err = %v)", got, tc.wantErrUnknown, err)
			}
			if n := len(f.calls()); n != 1 {
				t.Errorf("permanent refusal retried: %d requests, want 1", n)
			}
		})
	}
}

// TestEditPathSharesTheRetrySchedule: a 5xx on either verb must be retried on
// the same bounded loop ordinary sends use — the edit rides the same bucket
// and the same three-attempt budget, not a second one.
func TestEditPathSharesTheRetrySchedule(t *testing.T) {
	old := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = old })

	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if n == 0 {
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		okCreated("42")(n, r, rw)
	})
	d := &DiscordWebhook{URL: f.URL()}
	body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
	if err := d.patchMessage("999", body); err != nil {
		t.Fatalf("patchMessage did not retry a 502: %v", err)
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("attempts = %d, want 2 (one 502 then one success)", n)
	}
}

// TestEditOnceVariantsMakeExactlyOneRequest: the shutdown twins must never
// retry. The owner's ruling caps a graceful shutdown at 15 s; one lifecycle
// edit running the 2 s/5 s ladder against a wedged Discord would spend it.
//
// MUTANT: point either Once variant at d.deliver instead of d.do — the 502
// rows below each make three requests and this fails on the count.
func TestEditOnceVariantsMakeExactlyOneRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*DiscordWebhook, []byte) error
	}{
		{"postWaitOnce", func(d *DiscordWebhook, b []byte) error { _, err := d.postWaitOnce(b); return err }},
		{"patchMessageOnce", func(d *DiscordWebhook, b []byte) error { return d.patchMessageOnce("999", b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
				rw.WriteHeader(http.StatusBadGateway)
			})
			d := &DiscordWebhook{URL: f.URL()}
			body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
			if err := tc.call(d, body); err == nil {
				t.Fatal("a 502 was reported as success")
			}
			if n := len(f.calls()); n != 1 {
				t.Errorf("requests = %d, want exactly 1", n)
			}
		})
	}
}

// TestPatchMessageOnceStillRecognisesUnknownMessage: the shutdown path must
// classify a 404/10008 the same way the retrying one does, or a re-post
// decision would depend on when the process happened to be stopping.
func TestPatchMessageOnceStillRecognisesUnknownMessage(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusNotFound)
		io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
	})
	d := &DiscordWebhook{URL: f.URL()}
	body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
	if err := d.patchMessageOnce("999", body); !errors.Is(err, ErrUnknownMessage) {
		t.Errorf("err = %v, want ErrUnknownMessage", err)
	}
}

// TestUnknownMessageStillCarriesTheHTTPError: asEditRefusal wraps with TWO %w,
// so the one path that re-labels a refusal is not also the one path where the
// status and snippet stop being reachable. Every other refusal patchMessage
// passes through keeps them.
//
// MUTANT: use %v for the underlying error — errors.Is still finds
// ErrUnknownMessage and errors.As silently stops finding *discordHTTPError.
func TestUnknownMessageStillCarriesTheHTTPError(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusNotFound)
		io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
	})
	d := &DiscordWebhook{URL: f.URL()}
	body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
	err := d.patchMessage("999", body)
	if !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("err = %v, want ErrUnknownMessage", err)
	}
	var he *discordHTTPError
	if !errors.As(err, &he) {
		t.Fatalf("errors.As to *discordHTTPError = false through the ErrUnknownMessage wrap (err = %v)", err)
	}
	if he.Status != http.StatusNotFound {
		t.Errorf("recovered status = %d, want 404", he.Status)
	}
	if !strings.Contains(he.Snippet, "Unknown Message") {
		t.Errorf("recovered snippet = %q, want the refusal body", he.Snippet)
	}
}

// TestEditURLsSurviveAThreadIDQuery: discordWebhookRe (manager.go) admits an
// optional query, so ValidateURL accepts a webhook URL carrying ?thread_id= —
// Discord's documented way to post into a forum thread, and a shape that
// works today in separate mode.
//
// MUTANT: build either URL by concatenation. The POST query becomes
// "thread_id=456?wait=true" with no wait parameter at all, so Discord answers
// its non-wait 204 AFTER creating a message that can never be edited; the
// PATCH lands on the execute route with "/messages/999" buried in the query.
func TestEditURLsSurviveAThreadIDQuery(t *testing.T) {
	f := newFakeDiscord(t, okCreated("77"))
	d := &DiscordWebhook{URL: f.URL() + "/api/webhooks/123/TOK?thread_id=456"}

	body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
	if _, err := d.postWait(body); err != nil {
		t.Fatalf("postWait: %v", err)
	}
	if err := d.patchMessage("999", body); err != nil {
		t.Fatalf("patchMessage: %v", err)
	}
	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("want 2 requests, got %d", len(calls))
	}

	if calls[0].Path != "/api/webhooks/123/TOK" {
		t.Errorf("POST path = %q, want /api/webhooks/123/TOK", calls[0].Path)
	}
	post, err := url.ParseQuery(calls[0].Query)
	if err != nil {
		t.Fatalf("POST query %q: %v", calls[0].Query, err)
	}
	if post.Get("wait") != "true" {
		t.Errorf("POST query = %q, want a wait=true parameter of its own", calls[0].Query)
	}
	if post.Get("thread_id") != "456" {
		t.Errorf("POST query = %q, want thread_id=456 kept", calls[0].Query)
	}

	if calls[1].Path != "/api/webhooks/123/TOK/messages/999" {
		t.Errorf("PATCH path = %q, want /api/webhooks/123/TOK/messages/999", calls[1].Path)
	}
	patch, err := url.ParseQuery(calls[1].Query)
	if err != nil {
		t.Fatalf("PATCH query %q: %v", calls[1].Query, err)
	}
	if patch.Get("thread_id") != "456" {
		t.Errorf("PATCH query = %q, want thread_id=456 kept — it names the forum thread on both routes", calls[1].Query)
	}
	if patch.Has("wait") {
		t.Errorf("PATCH query = %q — the edit route takes no wait parameter", calls[1].Query)
	}
}

// TestMessageURLNormalisesTheConfiguredURL pins the shapes the builder must
// normalise anyway — a trailing slash, which ValidateURL admits, and a
// fragment, which it no longer does but which a hand-written config or a
// future caller could still hand over — plus the escaping of an id that came
// straight out of the jobs column: decodeNotificationMsgs does no shape
// validation, so whatever is stored reaches this builder verbatim.
func TestMessageURLNormalisesTheConfiguredURL(t *testing.T) {
	const base = "https://discord.com/api/webhooks/123/TOK"
	for _, tc := range []struct{ name, webhookURL, id, want string }{
		{"plain", base, "999", base + "/messages/999"},
		{"trailing slash", base + "/", "999", base + "/messages/999"},
		{"fragment dropped", base + "#frag", "999", base + "/messages/999"},
		{"wait stripped", base + "?wait=true", "999", base + "/messages/999"},
		{"id with a query separator", base, "9?x=1", base + "/messages/9%3Fx=1"},
		{"id with a space", base, "9 9", base + "/messages/9%209"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &DiscordWebhook{URL: tc.webhookURL}
			if got := d.messageURL(tc.id); got != tc.want {
				t.Errorf("messageURL(%q) on %q = %q, want %q", tc.id, tc.webhookURL, got, tc.want)
			}
		})
	}
}

// TestEditOnceVariantsReportA429LikeSendOnce: the twins' doc comment claims
// they mirror SendOnce, and SendOnce has a dedicated 429 arm. Without it an
// operator reads "discord webhook returned 429" and has to decode the status
// and dig the Retry-After out of the quoted body himself.
func TestEditOnceVariantsReportA429LikeSendOnce(t *testing.T) {
	handler := func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.Header().Set("Retry-After", "3")
		rw.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(rw, `{"message":"You are being rate limited.","retry_after":3}`)
	}
	const want = "discord rate limited (retry-after: 3)"
	for _, tc := range []struct {
		name string
		call func(*DiscordWebhook, []byte) error
	}{
		{"SendOnce", func(d *DiscordWebhook, _ []byte) error {
			return d.SendOnce(One("t", "d", 0, nil, SendOptions{}))
		}},
		{"postWaitOnce", func(d *DiscordWebhook, b []byte) error { _, err := d.postWaitOnce(b); return err }},
		{"patchMessageOnce", func(d *DiscordWebhook, b []byte) error { return d.patchMessageOnce("999", b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, handler)
			d := &DiscordWebhook{URL: f.URL()}
			body, _ := buildPayload(One("t", "d", 0, nil, SendOptions{}))
			err := tc.call(d, body)
			if err == nil {
				t.Fatal("a 429 was reported as success")
			}
			if err.Error() != want {
				t.Errorf("err = %q, want %q", err, want)
			}
		})
	}
}

// TestSeparateSendStillPostsPlain: the generalised loop must not change what an
// ordinary send looks like on the wire — no ?wait=true, no body read.
func TestSeparateSendStillPostsPlain(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusNoContent)
	})
	d := &DiscordWebhook{URL: f.URL()}
	if err := d.Send(One("t", "d", 0x3498db, nil, SendOptions{Event: "finished"})); err != nil {
		t.Fatalf("Send: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Query != "" {
		t.Fatalf("ordinary Send changed shape: %+v", calls)
	}
}
