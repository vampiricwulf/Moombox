package youtube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// embedPageFlags is the encryptedHostFlags the stub embed page states, in the
// shape encryptedHostFlagsRegex reads.
const embedPageFlags = "ehf-reddit"

// embedPageRequest is what one embed page fetch carried.
type embedPageRequest struct {
	cookie, referer string
}

// serveEmbedPage points constants.YouTubeURLs.Embed at a stub embed page that
// states embedPageFlags, and returns the requests it was sent.
func serveEmbedPage(t *testing.T) func() []embedPageRequest {
	t.Helper()
	var mu sync.Mutex
	var got []embedPageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, embedPageRequest{cookie: r.Header.Get("Cookie"), referer: r.Header.Get("Referer")})
		mu.Unlock()
		_, _ = io.WriteString(w, `<html><script>ytcfg.set({"WEB_PLAYER_CONTEXT_CONFIGS":{"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER":{"encryptedHostFlags":"`+embedPageFlags+`"}}});</script></html>`)
	}))
	t.Cleanup(srv.Close)
	orig := constants.YouTubeURLs.Embed
	constants.YouTubeURLs.Embed = srv.URL + "/embed"
	t.Cleanup(func() { constants.YouTubeURLs.Embed = orig })
	return func() []embedPageRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]embedPageRequest(nil), got...)
	}
}

// ageGateTransport answers the player calls of an age-gated video. Each
// client's status and body come from its entry (by X-YouTube-Client-Name;
// a client with none is refused 403), except web_embedded ("56"), which
// passes the gate only when its call carries the embed page's
// encryptedHostFlags — the retry's shape — and is age-gated otherwise, as the
// authenticated cascade's lightweight call is. Every web_embedded request's
// Referer and body are recorded.
type ageGateTransport struct {
	answers map[string]ageGateAnswer

	mu       sync.Mutex
	embedded []embeddedCall
}

type ageGateAnswer struct {
	status int
	body   string
}

type embeddedCall struct {
	referer, body string
}

func (tr *ageGateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	client := req.Header.Get("X-YouTube-Client-Name")
	b, _ := io.ReadAll(req.Body)
	answer, ok := tr.answers[client]
	if !ok {
		answer = ageGateAnswer{status: http.StatusForbidden, body: `{"error":{"message":"refused","status":"PERMISSION_DENIED"}}`}
	}
	if client == constants.WebEmbeddedClient.ClientID {
		tr.mu.Lock()
		tr.embedded = append(tr.embedded, embeddedCall{referer: req.Header.Get("Referer"), body: string(b)})
		tr.mu.Unlock()
		answer = ageGateAnswer{status: http.StatusOK, body: ageRestrictedBody}
		if strings.Contains(string(b), `"encryptedHostFlags":"`+embedPageFlags+`"`) {
			answer.body = adequateOKBody
		}
	}
	return &http.Response{StatusCode: answer.status, Body: io.NopCloser(strings.NewReader(answer.body)), Header: make(http.Header), Request: req}, nil
}

func (tr *ageGateTransport) embeddedCalls() []embeddedCall {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]embeddedCall(nil), tr.embedded...)
}

func serveAgeGate(t *testing.T, answers map[string]ageGateAnswer) *ageGateTransport {
	t.Helper()
	tr := &ageGateTransport{answers: answers}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })
	return tr
}

// TestEmbedPageIsFetchedFromTheEmbeddingHost is G27 (owner decision D24):
// the embed page web_embedded reads encryptedHostFlags and a player URL off
// is fetched as yt-dlp's _download_ytcfg fetches it (f2bd3202c, #16177) —
// Referer https://www.reddit.com/, the embedUrl the player call then claims,
// and, signed in, the jar's session as the page cookies carry it; signed
// out (an empty jar, or a half-cleared one whose SAPISID is no session) the
// consent and language pair alone, the public watch page's. The player call
// itself claims reddit in its context and sends no Referer, as upstream's
// sends none.
//
// Mutants: FetchEmbedPage sending no Referer (every row); the player call
// keeping its youtube.com/embed Referer (every row); embedPageCookieHeader
// always returning the consent pair (the signed-in row: no SAPISID) or always
// the page cookies (the half-cleared row: SAPISID rides along); the embedUrl
// and the Referer drifting apart (embedThirdPartyURL edited in one place —
// the row asserts both against the literal).
func TestEmbedPageIsFetchedFromTheEmbeddingHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		jar  []string
		want string
	}{
		{name: "an empty jar", want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a half-cleared jar: SAPISID without LOGIN_INFO is no session",
			jar:  []string{"SAPISID", "sapisid-a", "PREF", "hl=fr"},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a signed-in jar: its session, in English",
			jar:  []string{"SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a", "PREF", "hl=fr"},
			want: "LOGIN_INFO=logininfo-a; PREF=hl=en&tz=UTC; SAPISID=sapisid-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := serveEmbedPage(t)
			tr := serveAgeGate(t, nil)
			jar := cookies.NewCookieJar()
			if len(tc.jar) > 0 {
				jar = loadCookieRows(t, tc.jar...)
			}
			p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
			if _, err := p.fetchWithEmbedded(context.Background(), "test1234567", DefaultYtcfg(), 0, true); err != nil {
				t.Fatalf("fetchWithEmbedded: %v", err)
			}

			got := pages()
			if len(got) != 1 {
				t.Fatalf("embed page fetched %d times, want once", len(got))
			}
			if got[0].referer != "https://www.reddit.com/" {
				t.Errorf("embed page Referer = %q, want https://www.reddit.com/ (yt-dlp's thirdParty.embedUrl)", got[0].referer)
			}
			if got[0].cookie != tc.want {
				t.Errorf("embed page Cookie = %q, want %q", got[0].cookie, tc.want)
			}

			calls := tr.embeddedCalls()
			if len(calls) != 1 {
				t.Fatalf("web_embedded player calls = %d, want 1", len(calls))
			}
			if calls[0].referer != "" {
				t.Errorf("player call Referer = %q, want none (upstream's generate_api_headers sends none)", calls[0].referer)
			}
			if !strings.Contains(calls[0].body, `"embedUrl":"https://www.reddit.com/"`) {
				t.Errorf("player call claims no reddit embedUrl: %s", calls[0].body)
			}
			if !strings.Contains(calls[0].body, `"encryptedHostFlags":"`+embedPageFlags+`"`) {
				t.Errorf("player call carries not the embed page's flags: %s", calls[0].body)
			}
		})
	}
}
