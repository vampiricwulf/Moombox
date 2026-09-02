package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// Fixtures. Obviously fake values, far-future expiries (mergeCookieFiles prunes
// on expiry), and every one of them is scanned for in the leak test below.
const (
	importHeader     = "# Netscape HTTP Cookie File\n"
	importYouTube    = ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfake-sapisid-aaaa\n"
	importYouTubeTwo = ".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfake-logininfo-aaaa\n"
	importTwitch     = ".twitch.tv\tTRUE\t/\tTRUE\t2000000000\tauth-token\tfake-authtoken-aaaa\n"
	importSignedOut  = ".youtube.com\tTRUE\t/\tFALSE\t2000000000\tYSC\tfake-ysc-aaaa\n"
)

func importPaste() string { return importHeader + importYouTube + importYouTubeTwo }

// recordingLogger captures every line the service logs, so the leak scan can
// read them. The anonymous four-method interface, repeated — never extracted.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) record(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg+" "+fmt.Sprint(args...))
}
func (l *recordingLogger) Debug(msg string, args ...any) { l.record(msg, args...) }
func (l *recordingLogger) Info(msg string, args ...any)  { l.record(msg, args...) }
func (l *recordingLogger) Warn(msg string, args ...any)  { l.record(msg, args...) }
func (l *recordingLogger) Error(msg string, args ...any) { l.record(msg, args...) }
func (l *recordingLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// importRouter wires the real CookieRoutes over a service pointed at a real
// cookies.txt. The RefreshService is real too and its jar is EMPTY, so the
// handler's deferred CheckNow short-circuits before any network call.
func importRouter(t *testing.T, seed string, rl *web.RateLimiter) (chi.Router, string, *recordingLogger, *cookies.AutoCookieService) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if seed != "" {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed cookies.txt: %v", err)
		}
	}
	log := &recordingLogger{}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	svc := cookies.NewAutoCookieService(dir, path, jar, log)
	// Verified without a network call: the callbacks are the seam
	// checkPlatformAuth goes through, and a nil callback would report success
	// on presence alone, which is a different state.
	svc.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	svc.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

	r := chi.NewRouter()
	CookieRoutes(r, cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log), svc, nil, rl)
	return r, path, log, svc
}

func postImport(t *testing.T, r chi.Router, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestCookieImportRouteWritesAndAnswersWithTheVerdict — the happy path, asserted on
// the FILE (the sibling platform survived) and on the wire (the verdict).
//
// The mutation: answering before calling ImportCookies, or rendering
// refreshSvc.GetStatus() instead of the result — the response would then carry
// the PRE-import snapshot, and a bad export would be reported as fine.
func TestCookieImportRouteWritesAndAnswersWithTheVerdict(t *testing.T) {
	r, path, _, _ := importRouter(t, importHeader+importTwitch, nil)

	rec := postImport(t, r, "text/plain", importPaste())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body.String())
	}
	if body["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", body["authenticated"])
	}
	if body["youtubeVerification"] != "ok" {
		t.Errorf("youtubeVerification = %v, want \"ok\"", body["youtubeVerification"])
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(onDisk), "fake-authtoken-aaaa") {
		t.Error("the Twitch row is gone — the endpoint replaced rather than merged")
	}
	if !strings.Contains(string(onDisk), "fake-sapisid-aaaa") {
		t.Error("the pasted YouTube row never reached the file")
	}
}

// TestCookieImportOutcomeSpeaksTheSetupOutcomeVocabulary. The two payloads answer the
// same question about the same three states, and the dashboard renders both
// through cookieSetupAcceptedToast / cookieSetupRejectedMessage. A key added to
// one and not the other is the junction defect this file keeps finding: the
// import's UI silently reads `undefined` and hedges about a working session.
//
// The mutation: renaming or adding a key in either renderer.
func TestCookieImportOutcomeSpeaksTheSetupOutcomeVocabulary(t *testing.T) {
	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	got := keys(cookieImportOutcome(cookies.ImportResult{}))
	want := keys(cookieSetupOutcome(cookies.SetupResult{}))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cookieImportOutcome keys = %v, cookieSetupOutcome keys = %v — the two must stay "+
			"identical so the dashboard's copy helpers read both", got, want)
	}
}

// TestCookieImportRouteRefusesTheThreeShapesWithTheirOwnMessage.
//
// The mutation: collapsing the three sentinels into the default arm. Every bad
// paste then answers "cookie import failed" with a 500, which names nothing the
// operator can act on and reads as a server fault rather than a bad export.
func TestCookieImportRouteRefusesTheThreeShapesWithTheirOwnMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{"json export", `[{"name":"SAPISID"}]`, cookies.ErrImportNotNetscape},
		{"comments only", importHeader + "# nothing here\n", cookies.ErrImportNoRows},
		{"signed-out export", importHeader + importSignedOut, cookies.ErrImportNoCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, path, _, _ := importRouter(t, importHeader+importTwitch, nil)
			before, _ := os.ReadFile(path)

			rec := postImport(t, r, "text/plain", tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %q", rec.Body.String())
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.want.Error()) {
				t.Errorf("error = %q, want it to carry %q", msg, tc.want.Error())
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Error("a refused paste still rewrote cookies.txt")
			}
		})
	}
}

// TestCookieImportRouteRefusesTheWrongRequestShapes. Each of these is a distinct
// operator mistake with a distinct answer, and each has its own mutant: drop
// the cap and a 50 MB paste is merged into memory and onto disk; drop the
// content-type switch and a JSON body posted by a well-meaning client is read
// as cookie text and rejected with the wrong sentence; drop the empty-body
// check and a mis-click reports "that cookie file has no cookie rows" about a
// file the operator never sent.
func TestCookieImportRouteRefusesTheWrongRequestShapes(t *testing.T) {
	t.Run("over the cap, pasted", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		oversize := importPaste() + strings.Repeat("# padding\n", 60000)
		rec := postImport(t, r, "text/plain", oversize)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", rec.Code, rec.Body.String())
		}
	})

	// The two shapes reach the cap through DIFFERENT code, so one subtest does
	// not cover both: the paste gets the *http.MaxBytesError straight back from
	// io.ReadAll, while the upload gets whatever ParseMultipartForm returns for a
	// body whose reader refused. errors.As has to find the sentinel through
	// that, or an oversize upload answers 400 "no `cookies` file part" — a
	// sentence about the wrong mistake, on the shape a phone uses.
	t.Run("over the cap, uploaded", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreateFormFile("cookies", "cookies.txt")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write([]byte(importPaste() + strings.Repeat("# padding\n", 60000))); err != nil {
			t.Fatalf("write part: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close multipart: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413 — a 400 here means ParseMultipartForm wrapped the cap error "+
				"somewhere errors.As cannot reach, and the reader needs its own check: %s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("an unsupported content type", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		rec := postImport(t, r, "application/json", `{"cookies":"..."}`)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status %d, want 415: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an empty body", func(t *testing.T) {
		r, _, _, _ := importRouter(t, "", nil)
		rec := postImport(t, r, "text/plain", "   \n\n")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestCookieImportRouteAcceptsAMultipartUpload — the file picker's shape. The panel
// has two controls and this is the one a phone uses; without it the file input
// posts a body the handler answers 415 to.
//
// The mutation: deleting the multipart arm, or reading the wrong part name.
func TestCookieImportRouteAcceptsAMultipartUpload(t *testing.T) {
	r, path, _, _ := importRouter(t, importHeader+importTwitch, nil)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("cookies", "cookies.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte(importPaste())); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/cookies/import", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	onDisk, _ := os.ReadFile(path)
	if !strings.Contains(string(onDisk), "fake-sapisid-aaaa") {
		t.Error("the uploaded rows never reached the file")
	}
}

// TestCookieImportRouteIsOnTheHeavyLimiter. The endpoint validates, merges, writes and
// then makes up to four live auth round-trips; unlimited it is a free
// amplifier against two upstreams and a rewrite of the credential file per
// request.
//
// The mutation: registering the route on `r` instead of `heavy`.
func TestCookieImportRouteIsOnTheHeavyLimiter(t *testing.T) {
	rl := web.NewRateLimiterCtx(context.Background(), 1, time.Minute)
	t.Cleanup(rl.Close)
	r, _, _, _ := importRouter(t, "", rl)

	if rec := postImport(t, r, "text/plain", importPaste()); rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec := postImport(t, r, "text/plain", importPaste())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429 — the route is not on the heavy limiter", rec.Code)
	}
}

// TestCookieImportRouteHasNoGET is the third controller ruling, at the wire. The
// endpoint accepts credential bytes and never serves them; that asymmetry is
// the whole of what keeps it from being an exfiltration path.
//
// The mutation: adding a GET handler for this path — "so the panel can show
// what is currently loaded" is exactly how it would arrive.
func TestCookieImportRouteHasNoGET(t *testing.T) {
	r, _, _, _ := importRouter(t, importHeader+importYouTube+importYouTubeTwo, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cookies/import", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/cookies/import answered %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "fake-sapisid-aaaa") {
		t.Fatal("GET /api/cookies/import served a cookie value")
	}
}

// importRouterOverADirectory points the service's cookie path at a DIRECTORY,
// so the pre-merge read fails with something other than "does not exist" — the
// S9 abort — on both platforms (EISDIR on Linux, a read error on Windows) and
// without the unexported seam, which this package cannot reach.
//
// It exists for the leak scan below: that abort's message is the one route
// answer that interpolates an OS error, and an OS error carries a path.
func importRouterOverADirectory(t *testing.T) (chi.Router, *recordingLogger) {
	t.Helper()
	dir := t.TempDir()
	blocked := filepath.Join(dir, "cookies.txt")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("mkdir the blocking directory: %v", err)
	}
	log := &recordingLogger{}
	svc := cookies.NewAutoCookieService(dir, blocked, cookies.NewCookieJar(), log)
	r := chi.NewRouter()
	CookieRoutes(r, cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log), svc, nil, nil)
	return r, log
}

// TestCookieImportRouteLeaksNoValueAnywhere is the security rule, executed: drive
// every answer this endpoint can give with a body full of distinctive fake
// values, and scan the response AND every log line for each of them.
//
// THE ANSWERS THAT WRAP AN OS ERROR are included, and they are the interesting
// ones. The three sentinels are fixed strings and cannot carry anything; the
// unreadable-file abort interpolates `readErr` and logs the path, so a path DOES
// appear — which is intended, the message is useless without it, and a path is
// not a secret. What this proves about that answer is the narrower claim: no
// cookie VALUE rides along with it.
//
// The unwritable-file and reload-failure answers cannot be reached from this
// package (they need the write seam), and they are scanned in
// TestImportFailurePathsCarryNoValue over in internal/cookies. The reload
// failure's route arm is additionally value-free by construction: it interpolates
// nothing at all.
//
// The mutation: any handler or sentinel that grows a %s of the submitted text —
// the shape a "helpful" diagnostic naming the offending row would take.
func TestCookieImportRouteLeaksNoValueAnywhere(t *testing.T) {
	secrets := []string{"fake-sapisid-aaaa", "fake-logininfo-aaaa", "fake-authtoken-aaaa", "fake-ysc-aaaa"}
	scan := func(t *testing.T, rec *httptest.ResponseRecorder, log *recordingLogger) {
		t.Helper()
		haystack := rec.Body.String() + "\n" + fmt.Sprint(rec.Header()) + "\n" + log.all()
		for _, secret := range secrets {
			if strings.Contains(haystack, secret) {
				t.Fatalf("a cookie value reached the response or the log: %q", secret)
			}
		}
	}

	for _, tc := range []struct{ name, body string }{
		{"accepted", importPaste()},
		{"signed-out", importHeader + importSignedOut},
		{"not netscape", `[{"name":"SAPISID","value":"fake-sapisid-aaaa"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, log, _ := importRouter(t, importHeader+importTwitch, nil)
			rec := postImport(t, r, "text/plain", tc.body)
			scan(t, rec, log)
		})
	}

	t.Run("the unreadable-file abort", func(t *testing.T) {
		r, log := importRouterOverADirectory(t)
		rec := postImport(t, r, "text/plain", importPaste())
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422 — an unreadable cookies.txt must reach the operator as the "+
				"S9 abort, not as a bare 500: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "refusing to merge or overwrite") {
			t.Errorf("the 422 does not carry the abort's own sentence, so nothing tells the operator "+
				"the file was left alone: %s", rec.Body.String())
		}
		scan(t, rec, log)
	})
}
