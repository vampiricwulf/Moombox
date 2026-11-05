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

// flagged counts the web_embedded calls that carried the embed page's flags:
// the age-gate retry's.
func (tr *ageGateTransport) flagged() int {
	n := 0
	for _, c := range tr.embeddedCalls() {
		if strings.Contains(c.body, embedPageFlags) {
			n++
		}
	}
	return n
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

// TestAgeGateRetryWhenTVFailsAtTheHTTPLevel is G29 (owner decision D24):
// the web_embedded age-gate retry, encryptedHostFlags and all, fires on TV's
// verdict and, when TV failed at the HTTP level, on the watch page's — or,
// on the authenticated cascade, the web client's. It used to be keyed on TV
// alone, so a TV refused with a 403 left the age-gated video to the
// cookieless chain, which cannot serve one, and the extraction returned the
// watch page's age-restricted parse. And TV's verdict stays final when TV
// answered: a TV that serves the formats is not second-guessed by the watch
// page's gate.
//
// Here every client but the ones a row names is refused 403, and
// web_embedded passes the gate only with the embed page's flags.
//
// Mutants: the retry keyed on TV alone again (the three "TV failed" rows
// return the age-restricted parse, no flagged call); the tvFailed guard
// dropped from ageGateRetryWitness (the "TV answered" row makes a flagged
// call); the web witness left out of the authenticated cascade (the "web
// says" row); the public cascade passing tvFailed false (the public "TV
// failed" row).
func TestAgeGateRetryWhenTVFailsAtTheHTTPLevel(t *testing.T) {
	tvOK := map[string]ageGateAnswer{constants.TVSamsungClient.ClientID: {status: http.StatusOK, body: adequateOKBody}}
	webGated := map[string]ageGateAnswer{constants.WebClient.ClientID: {status: http.StatusOK, body: ageRestrictedBody}}
	for _, tc := range []struct {
		name        string
		public      bool
		watchPage   string // the page's player response; "" for none
		answers     map[string]ageGateAnswer
		wantFlagged int
		wantPlay    PlayabilityError
	}{
		{name: "public: TV failed, the watch page says age-gated", public: true,
			watchPage: ageRestrictedBody, wantFlagged: 1, wantPlay: PlayabilityOK},
		{name: "authenticated: TV failed, the watch page says age-gated",
			watchPage: ageRestrictedBody, wantFlagged: 1, wantPlay: PlayabilityOK},
		{name: "authenticated: TV failed, the web client says age-gated",
			answers: webGated, wantFlagged: 1, wantPlay: PlayabilityOK},
		{name: "public: TV answered with formats, the watch page's gate does not overrule it", public: true,
			watchPage: ageRestrictedBody, answers: tvOK, wantFlagged: 0, wantPlay: PlayabilityOK},
		{name: "public: TV failed and nothing says age-gated", public: true,
			wantFlagged: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveEmbedPage(t)
			tr := serveAgeGate(t, tc.answers)
			if tc.watchPage != "" {
				stubWatchPageWithPlayerResponse(t, tc.watchPage)
			} else {
				stubWatchPage(t)
			}
			p := newRetryTestAPI()
			var info *VideoInfo
			var err error
			if tc.public {
				info, err = p.GetVideoInfoPublic(context.Background(), "test1234567")
			} else {
				info, err = p.GetVideoInfoAuthenticated(context.Background(), "test1234567")
			}
			if got := tr.flagged(); got != tc.wantFlagged {
				t.Errorf("web_embedded calls with the embed page's flags = %d, want %d", got, tc.wantFlagged)
			}
			if tc.wantPlay == "" {
				return // nothing answered: the error is finishExtraction's, pinned elsewhere
			}
			if err != nil {
				t.Fatalf("extraction: %v", err)
			}
			if info.PlayabilityError != tc.wantPlay {
				t.Errorf("playability = %q, want %q", info.PlayabilityError, tc.wantPlay)
			}
			if len(info.Formats) == 0 {
				t.Error("no formats: the gate was not passed")
			}
		})
	}
}

// TestAgeGateRetryWitness pins the witness order and its log labels: TV's
// verdict first; the others only when TV failed, the first age-gated one
// named; nil answers skipped.
//
// Mutants: the others consulted while TV answered; a nil info dereferenced
// (panics); the last witness named instead of the first.
func TestAgeGateRetryWitness(t *testing.T) {
	gated := &VideoInfo{PlayabilityError: PlayabilityAgeRestricted}
	ok := &VideoInfo{PlayabilityError: PlayabilityOK}
	empty := &VideoInfo{}
	for _, tc := range []struct {
		name     string
		tv       *VideoInfo
		tvFailed bool
		others   []ageGateWitness
		want     string
	}{
		{name: "TV says it", tv: gated, others: []ageGateWitness{{"watch_page", gated}}, want: "tv"},
		{name: "TV answered otherwise", tv: ok, others: []ageGateWitness{{"watch_page", gated}}, want: ""},
		{name: "TV failed, the page says it", tv: empty, tvFailed: true,
			others: []ageGateWitness{{"watch_page", nil}, {"web_safari", gated}, {"web", gated}}, want: "web_safari"},
		{name: "TV failed, nobody says it", tv: empty, tvFailed: true,
			others: []ageGateWitness{{"watch_page", ok}, {"web", nil}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ageGateRetryWitness(tc.tv, tc.tvFailed, tc.others...); got != tc.want {
				t.Errorf("ageGateRetryWitness = %q, want %q", got, tc.want)
			}
		})
	}
}
