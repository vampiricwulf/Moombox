package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// Arc 10 R6, the HLS side, END TO END and offline.
//
// The Task 8 report claimed GetHLSMasterPlaylist had no injection seam and was
// therefore uncoverable. That was wrong: twitchHTTPClient (api.go) is a
// package-level var carrying BOTH halves of this method — the GQL POST that
// mints the playback token and the Usher fetch that follows — so an in-package
// test can swap its transport and drive the whole method.
//
// What that buys is the part the predicate's own tests cannot reach: that the
// predicate is CALLED at all, that its answer is what anonymousPlayback
// carries, and that the auth token is read ONCE. The last is the one whose
// failure mode is silent and backwards — a second read can turn a cookieless
// install into one whose credentials "failed".
//
// No fixture here is a credential. The token documents are two synthetic
// key/value pairs, the signature is empty, and the cookie values are the
// package's usual obviously-fake strings.

// hlsRoundTripper routes a request to a canned reply by URL prefix.
type hlsRoundTripper func(*http.Request) (*http.Response, error)

func (f hlsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// hlsWarnRecorder keeps the Warn MESSAGE and its ARGS, unlike the package's
// recordingLogger which deliberately drops args. Both halves are needed here:
// this test asserts the anonymous-playback Warn fires exactly once AND that
// neither half of it carries anything read from the token document.
type hlsWarnRecorder struct {
	mu    sync.Mutex
	warns []string
}

func (l *hlsWarnRecorder) Debug(string, ...any) {}
func (l *hlsWarnRecorder) Info(string, ...any)  {}
func (l *hlsWarnRecorder) Error(string, ...any) {}
func (l *hlsWarnRecorder) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg+" "+fmt.Sprint(args...))
}

func (l *hlsWarnRecorder) anonymousWarnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, w := range l.warns {
		if strings.Contains(w, "ANONYMOUS playback token") {
			out = append(out, w)
		}
	}
	return out
}

// hlsMasterFixture is the smallest master playlist ParseHLSMasterPlaylist
// yields a variant from. Its URL is a .invalid host so a leaked request cannot
// resolve anywhere.
const hlsMasterFixture = "#EXTM3U\n" +
	"#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO=\"chunked\"\n" +
	"https://video-weaver.test.invalid/chunked.m3u8\n"

// installHLSStub swaps the package-level HTTP client for one that serves a
// canned playback-token reply and a canned master playlist, and restores it in
// t.Cleanup. onGQL runs after the GQL reply is composed and before it is
// returned — the window in which a jar reload can happen underneath a method
// that reads its auth token twice.
//
// The swap is why no test in this file may call t.Parallel: the var is shared
// with every other test in the package. No test in this package uses it today.
func installHLSStub(t *testing.T, tokenValue string, onGQL func()) {
	t.Helper()
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })

	twitchHTTPClient = &http.Client{Transport: hlsRoundTripper(func(req *http.Request) (*http.Response, error) {
		reply := func(body []byte, ctype string) *http.Response {
			h := make(http.Header)
			h.Set("Content-Type", ctype)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       io.NopCloser(bytes.NewReader(body)),
				Request:    req,
			}
		}
		switch reqURL := req.URL.String(); {
		case strings.HasPrefix(reqURL, constants.TwitchURLs.GQL):
			// The real shape, with an EMPTY signature: nothing in this file is
			// a signed anything.
			body, err := json.Marshal(map[string]any{
				"data": map[string]any{
					"streamPlaybackAccessToken": map[string]any{
						"value":     tokenValue,
						"signature": "",
					},
				},
			})
			if err != nil {
				return nil, err
			}
			if onGQL != nil {
				onGQL()
			}
			return reply(body, "application/json"), nil
		case strings.HasPrefix(reqURL, constants.TwitchURLs.UsherLive):
			return reply([]byte(hlsMasterFixture), "application/vnd.apple.mpegurl"), nil
		default:
			// A request to anywhere else is a defect in this stub, not a pass.
			return nil, fmt.Errorf("stub received an unexpected request host")
		}
	})}
}

// hlsJarFile writes a Netscape cookie file. rows are written verbatim; an empty
// list writes a header and nothing else, which is the cookieless install.
func hlsJarFile(t *testing.T, path string, rows ...string) {
	t.Helper()
	content := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hlsJar writes the cookie file and loads a jar from it, so a later rewrite of
// the same path plus Reload is visible to the jar.
func hlsJar(t *testing.T, path string, rows ...string) *cookies.CookieJar {
	t.Helper()
	hlsJarFile(t, path, rows...)
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestGetHLSMasterPlaylistReportsTheAnonymousVerdict drives the whole method.
//
// The mutations it closes, neither of which any predicate test can see because
// neither is in the predicate:
//
//   - MM — delete the `if playbackTokenReportsAnonymous(...)` block from
//     GetHLSMasterPlaylist. Every predicate test still passes; the verdict is
//     simply never produced, the Warn never fires, and Arc 10's HLS half is
//     dead code that nothing reports.
//   - MN — keep the block and the Warn but drop `anonymousPlayback = true`.
//     The log line still appears, so a field check reads healthy, while the
//     platform is never marked.
//
// The last two cases are guards the predicate owns, re-asserted here through
// the real method: a cookieless install gets an anonymous token BY DESIGN and
// must not be reported, and an unreadable document is not a verdict.
func TestGetHLSMasterPlaylistReportsTheAnonymousVerdict(t *testing.T) {
	const fixtureToken = "test-token-aaaa"
	for _, tc := range []struct {
		name       string
		rows       []string
		tokenValue string
		wantReport bool
		wantWarns  int
	}{
		{
			name:       "credentials sent and ignored",
			rows:       []string{row(".twitch.tv", "auth-token", fixtureToken)},
			tokenValue: `{"user_id":null,"channel":"somechannel"}`,
			wantReport: true,
			wantWarns:  1,
		},
		{
			name:       "credentials sent and honoured",
			rows:       []string{row(".twitch.tv", "auth-token", fixtureToken)},
			tokenValue: `{"user_id":12345678,"channel":"somechannel"}`,
			wantReport: false,
			wantWarns:  0,
		},
		{
			name:       "cookieless install, anonymous by design",
			rows:       nil,
			tokenValue: `{"user_id":null,"channel":"somechannel"}`,
			wantReport: false,
			wantWarns:  0,
		},
		{
			name:       "credentials sent, document unreadable",
			rows:       []string{row(".twitch.tv", "auth-token", fixtureToken)},
			tokenValue: `{"userId":12345678}`,
			wantReport: false,
			wantWarns:  0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar := hlsJar(t, filepath.Join(t.TempDir(), "cookies.txt"), tc.rows...)
			log := &hlsWarnRecorder{}
			installHLSStub(t, tc.tokenValue, nil)

			svc := NewService(jar, log)
			variants, anonymousPlayback, err := svc.GetHLSMasterPlaylist(context.Background(), "somechannel")
			if err != nil {
				t.Fatalf("GetHLSMasterPlaylist: %v", err)
			}
			if len(variants) != 1 {
				t.Fatalf("variants = %d, want 1 — the stub's playlist did not reach the parser", len(variants))
			}
			if anonymousPlayback != tc.wantReport {
				t.Errorf("anonymousPlayback = %v, want %v", anonymousPlayback, tc.wantReport)
			}
			warns := log.anonymousWarnings()
			if len(warns) != tc.wantWarns {
				t.Errorf("anonymous-playback warnings = %d, want %d: %q", len(warns), tc.wantWarns, warns)
			}
			// The line names the channel and the consequence, and nothing else.
			// A future edit that interpolated the token document, or the
			// auth-token, would land here.
			for _, w := range warns {
				for _, secret := range []string{fixtureToken, "user_id", "12345678"} {
					if strings.Contains(w, secret) {
						t.Errorf("the anonymous-playback Warn carried %q: %q", secret, w)
					}
				}
			}
		})
	}
}

// TestGetHLSMasterPlaylistReadsTheAuthTokenOnce is the read-once discipline,
// and it is the only test that can see it.
//
// GetHLSMasterPlaylist reads s.Auth.GetAuthToken() into a local and hands the
// SAME value to the request and to the verdict. Reading it twice compiles,
// passes every other test in the tree, and is wrong in both directions — which
// is why both are driven here. A cookie import, a Reload, or a logout landing
// between the two reads is not a hypothetical: the jar is shared, and Reload is
// called from the refresh service, the web routes and the TUI.
//
//   - The jar GAINS a token during the call. Read once: the request was
//     cookieless, so the anonymous token it got back is by design and nothing
//     is reported. Read twice: the second read sees a token that was never
//     sent, and a cookieless install is told its credentials failed.
//   - The jar LOSES its token during the call. Read once: the credentials WERE
//     sent and ignored, so the mark is taken. Read twice: the second read sees
//     "", the guard swallows a genuine dead credential, and the one detector a
//     chat-off job has says nothing.
//
// MO closes on either half.
func TestGetHLSMasterPlaylistReadsTheAuthTokenOnce(t *testing.T) {
	const fixtureToken = "test-token-aaaa"
	for _, tc := range []struct {
		name       string
		before     []string
		after      []string
		wantReport bool
	}{
		{
			name:       "the jar gains a token mid-call",
			before:     nil,
			after:      []string{row(".twitch.tv", "auth-token", fixtureToken)},
			wantReport: false,
		},
		{
			name:       "the jar loses its token mid-call",
			before:     []string{row(".twitch.tv", "auth-token", fixtureToken)},
			after:      nil,
			wantReport: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cookies.txt")
			jar := hlsJar(t, path, tc.before...)

			// The reload happens strictly between the request and the verdict,
			// which is the whole window a second read would fall into.
			installHLSStub(t, `{"user_id":null}`, func() {
				hlsJarFile(t, path, tc.after...)
				if err := jar.Reload(); err != nil {
					t.Errorf("reload jar: %v", err)
				}
			})

			svc := NewService(jar, &hlsWarnRecorder{})
			_, anonymousPlayback, err := svc.GetHLSMasterPlaylist(context.Background(), "somechannel")
			if err != nil {
				t.Fatalf("GetHLSMasterPlaylist: %v", err)
			}
			if anonymousPlayback != tc.wantReport {
				t.Errorf("anonymousPlayback = %v, want %v — the auth token was read twice, "+
					"so the request and the verdict disagreed about what was sent",
					anonymousPlayback, tc.wantReport)
			}
		})
	}
}
