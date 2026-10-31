package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// These tests pin ONE claim: the Cookie header ChatAPI sends is produced by
// its getter AT REQUEST TIME, not captured once at construction. Live chat
// polls for hours while the in-process refresh rotates Google's cookies every
// ~30 minutes; a captured header goes stale, 401s, and ends chat capture for
// the rest of the archive.
//
// Every cookie value below is synthetic.

// seenRequest records what one request carried. cookiePresent is tracked
// separately from cookieValue so "no Cookie header at all" is distinguishable
// from "a Cookie header whose value is empty".
type seenRequest struct {
	cookiePresent bool
	cookieValue   string
}

// cookieRecorder is an httptest server that records the exact Cookie header of
// every request it receives and replies with a fixed body.
type cookieRecorder struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []seenRequest
}

func newCookieRecorder(t *testing.T, body string) *cookieRecorder {
	t.Helper()
	rec := &cookieRecorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, present := r.Header["Cookie"]
		entry := seenRequest{cookiePresent: present}
		if present {
			entry.cookieValue = strings.Join(values, "; ")
		}
		rec.mu.Lock()
		rec.seen = append(rec.seen, entry)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

// requests returns a snapshot of everything recorded so far.
func (rec *cookieRecorder) requests() []seenRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]seenRequest(nil), rec.seen...)
}

// redirectTransport reroutes every request to the test server while preserving
// method, path, body and — the point of the exercise — headers.
// FetchFreshContinuation hardcodes youtube.com, so this is how that use site
// gets exercised without a network request.
type redirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.Host = ""
	return rt.base.RoundTrip(clone)
}

// pointWatchPageAt makes api's watch-page fetch land on the recorder.
func pointWatchPageAt(t *testing.T, api *ChatAPI, rec *cookieRecorder) {
	t.Helper()
	target, err := url.Parse(rec.server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	api.client = &http.Client{Transport: redirectTransport{target: target, base: http.DefaultTransport}}
}

// watchPageHTML renders a minimal watch page carrying a chat continuation, so
// FetchFreshContinuation completes without an extraction error and the test is
// asserting on headers rather than on parse failure.
func watchPageHTML(token string) string {
	return `<html><body><script>var ytInitialData = ` +
		`{"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` +
		`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"` + token + `"}}]}}}}};` +
		`</script></body></html>`
}

// sequenceGetter returns a getter that yields values in order and repeats the
// last one once exhausted — a stand-in for a jar whose contents rotate.
func sequenceGetter(values ...string) func() string {
	var mu sync.Mutex
	i := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		v := values[min(i, len(values)-1)]
		i++
		return v
	}
}

// assertCookies compares what the server saw against the exact expected header
// values. A nil entry in want means "no Cookie header at all".
func assertCookies(t *testing.T, got []seenRequest, want []*string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("server saw %d requests, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if want[i] == nil {
			if got[i].cookiePresent {
				t.Errorf("request %d carried Cookie %q, want no Cookie header at all", i, got[i].cookieValue)
			}
			continue
		}
		if !got[i].cookiePresent {
			t.Errorf("request %d carried no Cookie header, want %q", i, *want[i])
			continue
		}
		if got[i].cookieValue != *want[i] {
			t.Errorf("request %d Cookie = %q, want %q", i, got[i].cookieValue, *want[i])
		}
	}
}

// TestFetchChatReReadsCookiePerRequest is the claim for the chat-poll use
// site. The getter returns a DIFFERENT value on its second call; if the header
// were captured at construction both polls would carry the first value, which
// is exactly the bug.
func TestFetchChatReReadsCookiePerRequest(t *testing.T) {
	rec := newCookieRecorder(t, `{}`)
	api := NewChatAPI("", "", sequenceGetter("SID=first-value", "SID=second-value"))

	for _, cont := range []string{"cont-1", "cont-2"} {
		if _, err := api.fetchChat(context.Background(), rec.server.URL, cont); err != nil {
			t.Fatalf("fetchChat(%s): %v", cont, err)
		}
	}

	assertCookies(t, rec.requests(), []*string{new("SID=first-value"), new("SID=second-value")})
}

// TestFetchFreshContinuationReReadsCookiePerRequest is the same claim for the
// watch-page use site. It is a SEPARATE code path from fetchChat and a fix
// applied to only one of them is the classic half-fix here.
func TestFetchFreshContinuationReReadsCookiePerRequest(t *testing.T) {
	rec := newCookieRecorder(t, watchPageHTML("chat-token"))
	api := NewChatAPI("", "", sequenceGetter("SID=first-value", "SID=second-value"))
	pointWatchPageAt(t, api, rec)

	for i := range 2 {
		token, isReplay, err := api.FetchFreshContinuation(context.Background(), "vid")
		if err != nil {
			t.Fatalf("FetchFreshContinuation call %d: %v", i, err)
		}
		if token != "chat-token" || isReplay {
			t.Fatalf("call %d returned (%q, %v), want (\"chat-token\", false)", i, token, isReplay)
		}
	}

	assertCookies(t, rec.requests(), []*string{new("SID=first-value"), new("SID=second-value")})
}

// TestFetchChatNilGetterSendsNoCookie: tests and cookieless installs build a
// ChatAPI without credentials. A nil getter must send no Cookie header and
// must not panic hours into a poll loop.
func TestFetchChatNilGetterSendsNoCookie(t *testing.T) {
	rec := newCookieRecorder(t, `{}`)
	api := NewChatAPI("", "", nil)

	if _, err := api.fetchChat(context.Background(), rec.server.URL, "cont"); err != nil {
		t.Fatalf("fetchChat: %v", err)
	}

	assertCookies(t, rec.requests(), []*string{nil})
}

// TestFetchFreshContinuationNilGetterSendsNoCookie — the other use site.
func TestFetchFreshContinuationNilGetterSendsNoCookie(t *testing.T) {
	rec := newCookieRecorder(t, watchPageHTML("chat-token"))
	api := NewChatAPI("", "", nil)
	pointWatchPageAt(t, api, rec)

	if _, _, err := api.FetchFreshContinuation(context.Background(), "vid"); err != nil {
		t.Fatalf("FetchFreshContinuation: %v", err)
	}

	assertCookies(t, rec.requests(), []*string{nil})
}

// TestFetchChatEmptyGetterSendsNoCookie preserves the pre-existing != ""
// guard: a getter that returns "" must send no Cookie header rather than an
// empty one.
func TestFetchChatEmptyGetterSendsNoCookie(t *testing.T) {
	rec := newCookieRecorder(t, `{}`)
	api := NewChatAPI("", "", func() string { return "" })

	if _, err := api.fetchChat(context.Background(), rec.server.URL, "cont"); err != nil {
		t.Fatalf("fetchChat: %v", err)
	}

	assertCookies(t, rec.requests(), []*string{nil})
}

// TestFetchFreshContinuationEmptyGetterSendsNoCookie — the other use site.
func TestFetchFreshContinuationEmptyGetterSendsNoCookie(t *testing.T) {
	rec := newCookieRecorder(t, watchPageHTML("chat-token"))
	api := NewChatAPI("", "", func() string { return "" })
	pointWatchPageAt(t, api, rec)

	if _, _, err := api.FetchFreshContinuation(context.Background(), "vid"); err != nil {
		t.Fatalf("FetchFreshContinuation: %v", err)
	}

	assertCookies(t, rec.requests(), []*string{nil})
}

// chatNopLogger satisfies youtube.NewAuth's logger interface silently.
type chatNopLogger struct{}

func (chatNopLogger) Debug(string, ...any) {}
func (chatNopLogger) Info(string, ...any)  {}
func (chatNopLogger) Warn(string, ...any)  {}
func (chatNopLogger) Error(string, ...any) {}

// writeSyntheticCookieFile writes a one-row Netscape cookie file. Synthetic
// throughout — no real cookie value is ever written here.
func writeSyntheticCookieFile(t *testing.T, path, name, value string) {
	t.Helper()
	row := strings.Join([]string{".youtube.com", "TRUE", "/", "TRUE", "0", name, value}, "\t")
	if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"+row+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCookieGetterFromYouTubeAuthTracksTheLiveJar is the end-to-end claim, and
// the one that matters: the getter the worker construction sites pass is
// youtube.Auth.GetCookieHeader as a METHOD VALUE. Rotate the jar underneath a
// constructed ChatAPI — the way the in-process refresh does every ~30 minutes
// — and the next request must carry the new value.
func TestCookieGetterFromYouTubeAuthTracksTheLiveJar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeSyntheticCookieFile(t, path, "SID", "before-rotation")

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load jar: %v", err)
	}
	auth := youtube.NewAuth(jar, chatNopLogger{})

	rec := newCookieRecorder(t, `{}`)
	// Exactly what internal/worker now assigns to opts.CookieHeader.
	api := NewChatAPI("", "", auth.GetCookieHeader)

	if _, err := api.fetchChat(context.Background(), rec.server.URL, "cont-1"); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	// The refresh rewrites cookies.txt and reloads the jar; the downloader is
	// not rebuilt.
	writeSyntheticCookieFile(t, path, "SID", "after-rotation")
	if err := jar.Reload(); err != nil {
		t.Fatalf("reload jar: %v", err)
	}

	if _, err := api.fetchChat(context.Background(), rec.server.URL, "cont-2"); err != nil {
		t.Fatalf("second poll: %v", err)
	}

	assertCookies(t, rec.requests(), []*string{new("SID=before-rotation"), new("SID=after-rotation")})
}

// TestFreshContinuationSendsThePageCookie: the watch page a stale
// continuation is re-read off is a YouTube page, and gets the page header the
// worker passes as PageCookieHeader (youtube.Auth.PageCookieHeader): the
// jar's cookies with SOCS=CAI for a jar that is not signed in — the consent
// an EU address needs to reach ytInitialData at all — and an English PREF.
// The chat API polls are Innertube and keep the jar's own header.
//
// Mutants: NewChatDownloader not wiring PageCookieHeader (the page carries
// the jar's header); FetchFreshContinuation reading cookieHeader first (the
// same); fetchChat taking the page header (the poll carries SOCS).
func TestFreshContinuationSendsThePageCookie(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeSyntheticCookieFile(t, path, "SID", "jar-value")
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load jar: %v", err)
	}
	auth := youtube.NewAuth(jar, chatNopLogger{})

	// Exactly what internal/worker assigns.
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:          "vid",
		OutputFile:       filepath.Join(t.TempDir(), "chat.json"),
		CookieHeader:     auth.GetCookieHeader,
		PageCookieHeader: auth.PageCookieHeader,
	})
	rec := newCookieRecorder(t, watchPageHTML("chat-token"))
	pointWatchPageAt(t, cd.api, rec)

	if _, _, err := cd.api.FetchFreshContinuation(context.Background(), "vid"); err != nil {
		t.Fatalf("FetchFreshContinuation: %v", err)
	}
	// The recorder answers the poll with the page too, which fetchChat
	// cannot parse; only the request it sent is under test.
	_, _ = cd.api.fetchChat(context.Background(), rec.server.URL, "cont")

	assertCookies(t, rec.requests(), []*string{new("PREF=hl=en&tz=UTC; SID=jar-value; SOCS=CAI"), new("SID=jar-value")})
}

// TestFreshContinuationConsentWallNamesSOCS: a wall that stands although the
// page cookie carries SOCS=CAI is one only a browser's own consent passes, so
// the error names SOCS — the cookie that replaced CONSENT (yt-dlp
// 378ae9f9f) — and no longer tells the operator to supply CONSENT.
//
// Mutant: the CONSENT advice restored.
func TestFreshContinuationConsentWallNamesSOCS(t *testing.T) {
	api := NewChatAPI("", "", nil)
	api.client = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "consent.youtube.com" {
			fmt.Fprint(w, "<html>Before you continue to YouTube</html>")
			return
		}
		http.Redirect(w, r, "https://consent.youtube.com/m?continue=x", http.StatusFound)
	}))

	_, _, err := api.FetchFreshContinuation(context.Background(), "vid")
	if err == nil || !strings.Contains(err.Error(), "consent wall (consent.youtube.com)") {
		t.Fatalf("err = %v, want the consent wall", err)
	}
	if !strings.Contains(err.Error(), "SOCS") || strings.Contains(err.Error(), "CONSENT") {
		t.Errorf("err = %v, want advice that names SOCS, not CONSENT", err)
	}
}

// TestChatContextIsEnglishAndUTC: the chat polls' Innertube context states
// hl=en, timeZone=UTC and utcOffsetMinutes=0, as yt-dlp's _extract_context
// forces into every context (_base.py:753-759).
//
// Mutant: either zone field dropped from NewChatAPI's client.
func TestChatContextIsEnglishAndUTC(t *testing.T) {
	var body []byte
	api := NewChatAPI("", "", nil)
	api.client = httpxtest.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{}`)
	}))
	if _, err := api.fetchChat(context.Background(), "https://www.youtube.com/youtubei/v1/live_chat/get_live_chat", "cont"); err != nil {
		t.Fatalf("fetchChat: %v", err)
	}
	var req struct {
		Context struct {
			Client map[string]any `json:"client"`
		} `json:"context"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	c := req.Context.Client
	if c["hl"] != "en" || c["timeZone"] != "UTC" || c["utcOffsetMinutes"] != float64(0) {
		t.Errorf("chat context hl=%v timeZone=%v utcOffsetMinutes=%v, want en, UTC, 0", c["hl"], c["timeZone"], c["utcOffsetMinutes"])
	}
}
