package youtube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The players these tests name, each on the main variant extraction builds.
const (
	iframePlayerURL   = "https://www.youtube.com/s/player/9f3c2b1a/player_ias.vflset/en_US/base.js"
	homepagePlayerURL = "https://www.youtube.com/s/player/0c0ffee0/player_ias.vflset/en_US/base.js"
	pagePlayer        = "https://www.youtube.com/s/player/0a1b2c3d/player_ias.vflset/en_US/base.js"
	embedPlayer       = "https://www.youtube.com/s/player/e0e0e0e0/player_ias.vflset/en_US/base.js"
)

// servedFromBase is u as a page served from constants.YouTubeURLs.Base names
// it: a relative player path is joined to the base the page came from, which
// the homepage tests point at their own server.
func servedFromBase(u string) string {
	return strings.Replace(u, "https://www.youtube.com", constants.YouTubeURLs.Base, 1)
}

// stubIframeAPI makes the player-URL fallback's iframe API read answer with
// url (or fail with err) for the test, counting the reads.
func stubIframeAPI(t *testing.T, url string, err error) *atomic.Int32 {
	t.Helper()
	var reads atomic.Int32
	orig := fetchIframePlayerURL
	fetchIframePlayerURL = func(context.Context) (string, error) {
		reads.Add(1)
		return url, err
	}
	t.Cleanup(func() { fetchIframePlayerURL = orig })
	return &reads
}

// stubWatchPageYtcfg serves the cascades a watch page whose ytcfg is ytcfg
// (copied per call) and which carries no player response.
func stubWatchPageYtcfg(t *testing.T, ytcfg YtcfgData) {
	t.Helper()
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		c := ytcfg
		return &WatchPageResult{Ytcfg: &c}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })
}

// clientBodyTransport answers every player call with the body its client's entry
// names (adequateOKBody when none does) and records each request's body per
// X-YouTube-Client-Name.
type clientBodyTransport struct {
	mu        sync.Mutex
	responses map[string]string
	bodies    map[string][]string
}

func (tr *clientBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	client := req.Header.Get("X-YouTube-Client-Name")
	b, _ := io.ReadAll(req.Body)
	tr.mu.Lock()
	if tr.bodies == nil {
		tr.bodies = map[string][]string{}
	}
	tr.bodies[client] = append(tr.bodies[client], string(b))
	body, ok := tr.responses[client]
	tr.mu.Unlock()
	if !ok {
		body = adequateOKBody
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
}

func (tr *clientBodyTransport) of(client string) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.bodies[client]...)
}

func serveBodies(t *testing.T, responses map[string]string) *clientBodyTransport {
	t.Helper()
	tr := &clientBodyTransport{responses: responses}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })
	return tr
}

// TestIframeAPIGivesTheCanonicalPlayerURL is the iframe API half of G2,
// yt-dlp's _download_player_url (_video.py:2157-2171): the player id the
// script names — slashes escaped, as served, or not — becomes that player's
// main-variant URL, and a script naming none is an error, not a URL.
//
// Mutants: the regex's escaped-slash allowance dropped (the escaped row
// fails); the URL built from the matched path rather than the id (no main
// variant); the no-player case returning "" with no error.
func TestIframeAPIGivesTheCanonicalPlayerURL(t *testing.T) {
	var mu sync.Mutex
	body := ""
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		b := body
		mu.Unlock()
		_, _ = io.WriteString(w, b)
	}))
	defer srv.Close()
	orig := constants.YouTubeURLs.Base
	constants.YouTubeURLs.Base = srv.URL
	t.Cleanup(func() { constants.YouTubeURLs.Base = orig })

	for _, tc := range []struct {
		name, script, want string
	}{
		{"as served", `var scriptUrl = 'https:\/\/www.youtube.com\/s\/player\/9f3c2b1a\/www-widgetapi.vflset\/www-widgetapi.js';try{var ttPolicy=window.trustedTypes}catch(e){}`, iframePlayerURL},
		{"unescaped", `var scriptUrl = 'https://www.youtube.com/s/player/9f3c2b1a/www-widgetapi.vflset/www-widgetapi.js';`, iframePlayerURL},
		{"names no player", `var scriptUrl = 'https:\/\/www.youtube.com\/iframe_api_is_moving';`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			body, paths = tc.script, nil
			mu.Unlock()
			got, err := fetchPlayerURLFromIframeAPI(context.Background())
			if tc.want == "" {
				if err == nil {
					t.Fatalf("got %q, nil — a script naming no player must be an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("fetchPlayerURLFromIframeAPI = %q, %v; want %q", got, err, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(paths, []string{"/iframe_api"}) {
				t.Errorf("requested %v, want /iframe_api once", paths)
			}
		})
	}
}

// TestIframePlayerURLSeamIsTheIframeAPIFetch pins what TestMain stubs: in
// production the fallback's iframe read is fetchPlayerURLFromIframeAPI.
//
// Mutant: fetchIframePlayerURL initialised to anything else.
func TestIframePlayerURLSeamIsTheIframeAPIFetch(t *testing.T) {
	prod := productionIframePlayerURL
	if prod == nil { // the live suite never stubbed it
		prod = fetchIframePlayerURL
	}
	if reflect.ValueOf(prod).Pointer() != reflect.ValueOf(fetchPlayerURLFromIframeAPI).Pointer() {
		t.Error("fetchIframePlayerURL is not fetchPlayerURLFromIframeAPI in production")
	}
}

// TestResolvePlayerURLFallbackOrder pins G2's source order for an
// extraction whose watch page names no player: a URL a page named within
// playerURLFreshFor, then the iframe API, then the last good URL whatever its
// age — and the page's own URL, when it names one, ahead of all of them and
// remembered for the next.
//
// Mutants: the freshness window ignored (the stale rows skip the iframe API,
// or the fresh row asks it); the iframe API's answer not remembered (the
// "remembers" check); the last good URL dropped behind a failed iframe read
// (that row gets ""); the page's URL not remembered.
func TestResolvePlayerURLFallbackOrder(t *testing.T) {
	iframeDown := errors.New("iframe API down")
	for _, tc := range []struct {
		name       string
		page       string
		known      string
		knownAge   time.Duration
		iframe     string
		iframeErr  error
		want       string
		wantReads  int32
		wantMemory string
	}{
		{name: "the page names one", page: pagePlayer, known: homepagePlayerURL, iframe: iframePlayerURL, want: pagePlayer, wantMemory: pagePlayer},
		{name: "a fresh homepage read", known: homepagePlayerURL, knownAge: time.Hour, iframe: iframePlayerURL, want: homepagePlayerURL, wantMemory: homepagePlayerURL},
		{name: "a stale one: the iframe API", known: homepagePlayerURL, knownAge: 7 * time.Hour, iframe: iframePlayerURL, want: iframePlayerURL, wantReads: 1, wantMemory: iframePlayerURL},
		{name: "a stale one and the iframe API down: the last good", known: homepagePlayerURL, knownAge: 7 * time.Hour, iframeErr: iframeDown, want: homepagePlayerURL, wantReads: 1, wantMemory: homepagePlayerURL},
		{name: "nothing known: the iframe API", iframe: iframePlayerURL, want: iframePlayerURL, wantReads: 1, wantMemory: iframePlayerURL},
		{name: "nothing known and the iframe API down", iframeErr: iframeDown, want: "", wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := stubIframeAPI(t, tc.iframe, tc.iframeErr)
			p := newRetryTestAPI()
			if tc.known != "" {
				p.rememberPlayerURL(tc.known, "homepage")
				p.playerURLAt = time.Now().Add(-tc.knownAge)
			}
			if got := p.resolvePlayerURL(context.Background(), "test1234567", tc.page); got != tc.want {
				t.Errorf("resolvePlayerURL = %q, want %q", got, tc.want)
			}
			if n := reads.Load(); n != tc.wantReads {
				t.Errorf("iframe API read %d times, want %d", n, tc.wantReads)
			}
			if p.playerURL != tc.wantMemory {
				t.Errorf("remembered %q, want %q", p.playerURL, tc.wantMemory)
			}
		})
	}
}

// TestCascadesCarryTheResolvedPlayerURL: a watch page that names no player
// no longer leaves the extraction without one. Both cascades resolve it
// before STS, so the page's ytcfg STS goes out on every player call and the
// winner carries the iframe API's URL — where both used to be empty, every
// signature and n solve failing behind them.
//
// Mutants: either cascade's resolvePlayerURL call dropped, or moved after
// extractSTS (no STS on the call; PlayerURL empty).
func TestCascadesCarryTheResolvedPlayerURL(t *testing.T) {
	for _, path := range []string{"public", "authenticated"} {
		t.Run(path, func(t *testing.T) {
			stubWatchPageYtcfg(t, YtcfgData{STS: 20381, VisitorData: "vd-page"})
			stubIframeAPI(t, iframePlayerURL, nil)
			tr := serveBodies(t, nil)
			p := newRetryTestAPI()
			get := p.GetVideoInfoPublic
			if path == "authenticated" {
				get = p.GetVideoInfoAuthenticated
			}
			info, err := get(context.Background(), "test1234567")
			if err != nil {
				t.Fatalf("extraction: %v", err)
			}
			if info.PlayerURL != iframePlayerURL {
				t.Errorf("PlayerURL = %q, want the iframe API's %q", info.PlayerURL, iframePlayerURL)
			}
			tv := tr.of("7")
			if len(tv) == 0 || !strings.Contains(tv[0], `"signatureTimestamp":20381`) {
				t.Errorf("TV request bodies %q, want the ytcfg STS on the call", tv)
			}
		})
	}
}

// TestPagesNamingAPlayerFeedTheFallback: the homepage Service.Init and the
// account-session read fetch anyway, and the embed page the age-gate bypass
// fetches, each name the current player; each is remembered, and an
// extraction whose watch page names none takes it without asking the iframe
// API. The embed page's is adopted by the very call it was fetched for,
// when nothing earlier in the extraction had one.
//
// Mutants: either homepage read not remembering its player; the embed
// page's player not extracted, not remembered, or not adopted for its own
// call (the age-gated winner carries no PlayerURL); pagePlayerURL accepting
// a URL that names no player (the homepage's leading scriptSrc wins).
func TestPagesNamingAPlayerFeedTheFallback(t *testing.T) {
	// The homepage leads with a script that is not a player, as a page may.
	const homepage = `<script>ytcfg.set({"scriptSrc":"https:\/\/www.youtube.com\/s\/desktop\/abc\/jsbin\/desktop_polymer.vflset\/desktop_polymer.js",` +
		`"INNERTUBE_API_KEY":"k","visitorData":"vd-home","PLAYER_JS_URL":"\/s\/player\/0c0ffee0\/player_ias.vflset\/en_US\/base.js"});</script>`
	serveHomepage := func(t *testing.T) *atomic.Int32 {
		t.Helper()
		var fetches atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fetches.Add(1)
			_, _ = io.WriteString(w, homepage)
		}))
		t.Cleanup(srv.Close)
		orig := constants.YouTubeURLs.Base
		constants.YouTubeURLs.Base = srv.URL
		t.Cleanup(func() { constants.YouTubeURLs.Base = orig })
		return &fetches
	}

	t.Run("Service.Init's homepage", func(t *testing.T) {
		serveHomepage(t)
		svc := NewService(cookies.NewCookieJar(), noopLogger{})
		svc.Init(context.Background())
		stubWatchPage(t)
		reads := stubIframeAPI(t, iframePlayerURL, nil)
		serveBodies(t, nil)
		info, err := svc.PlayerAPI.GetVideoInfoPublic(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("GetVideoInfoPublic: %v", err)
		}
		if want := servedFromBase(homepagePlayerURL); info.PlayerURL != want {
			t.Errorf("PlayerURL = %q, want the homepage's %q", info.PlayerURL, want)
		}
		if n := reads.Load(); n != 0 {
			t.Errorf("the iframe API was read %d times, want none — the homepage's player is fresh", n)
		}
	})

	t.Run("the account-session read's homepage", func(t *testing.T) {
		fetches := serveHomepage(t)
		jar, _ := loadAccountJar(t, accountCookies("a"))
		svc := NewService(jar, noopLogger{})
		recordPlayerCalls(t)
		if _, err := svc.ProbeVideoStatusAuthenticated(context.Background(), "test1234567"); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if fetches.Load() != 1 {
			t.Fatalf("homepage fetched %d times, want the one session read", fetches.Load())
		}
		if want := servedFromBase(homepagePlayerURL); svc.PlayerAPI.playerURL != want {
			t.Errorf("remembered %q, want the homepage's %q", svc.PlayerAPI.playerURL, want)
		}
	})

	t.Run("the age-gate bypass's embed page", func(t *testing.T) {
		embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `<script>ytcfg.set({"WEB_PLAYER_CONTEXT_CONFIGS":{"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER":`+
				`{"jsUrl":"\/s\/player\/e0e0e0e0\/player_ias.vflset\/en_US\/base.js","encryptedHostFlags":"ehf"}}});</script>`)
		}))
		t.Cleanup(embed.Close)
		orig := constants.YouTubeURLs.Embed
		constants.YouTubeURLs.Embed = embed.URL
		t.Cleanup(func() { constants.YouTubeURLs.Embed = orig })
		stubWatchPage(t)
		reads := stubIframeAPI(t, "", errors.New("iframe API down"))
		tr := serveBodies(t, map[string]string{
			"7": `{"playabilityStatus":{"status":"AGE_VERIFICATION_REQUIRED","reason":"Sign in to confirm your age"},"videoDetails":{"videoId":"test1234567"}}`,
		})
		p := newRetryTestAPI()
		info, err := p.GetVideoInfoPublic(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("GetVideoInfoPublic: %v", err)
		}
		if reads.Load() != 1 {
			t.Fatalf("iframe API read %d times, want the one failed read before STS", reads.Load())
		}
		if info.PlayerURL != embedPlayer {
			t.Errorf("PlayerURL = %q, want the embed page's %q", info.PlayerURL, embedPlayer)
		}
		if emb := tr.of("56"); len(emb) != 1 || !strings.Contains(emb[0], `"encryptedHostFlags":"ehf"`) {
			t.Errorf("web_embedded bodies %q, want the one age-gate call", emb)
		}
		if p.playerURL != embedPlayer || p.playerURLSource != "embed page" {
			t.Errorf("remembered %q from %q, want the embed page's", p.playerURL, p.playerURLSource)
		}
	})
}

// TestWatchPageYtcfgNamesTheMainVariantAndSTS: the watch page's player URL
// is rewritten to its main variant (G50, yt-dlp's _construct_player_url),
// and the page's ytcfg STS is read (yt-dlp's _extract_signature_timestamp
// reads ytcfg first). A description quoting `"STS":1` is JSON-escaped on
// the page, so it cannot pass for the real one. The URL is the first that
// names a player, as on every other page (pagePlayerURL): a leading script
// that is not one is passed over, and a page naming none has none — so the
// fallbacks supply one and the script is never remembered.
//
// Mutants: normalizePlayerJSURL not canonicalising (the es6 and locale rows
// keep their variant); the STS regex dropped or unanchored to a bare quote;
// the first-match read restored, or watchPagePlayerURL not asking
// cipher.PlayerIDOf (the two non-player rows read desktop_polymer.js).
func TestWatchPageYtcfgNamesTheMainVariantAndSTS(t *testing.T) {
	for _, tc := range []struct {
		name, page, wantURL string
		wantSTS             int
	}{
		{"es6, escaped, relative", `ytcfg.set({"STS":20381,"PLAYER_JS_URL":"\/s\/player\/0a1b2c3d\/player_es6.vflset\/en_US\/base.js"})`, pagePlayer, 20381},
		{"another locale, absolute", `{"jsUrl":"https://www.youtube.com/s/player/0a1b2c3d/player_ias.vflset/fr_FR/base.js"}`, pagePlayer, 0},
		{"a quoted STS in a description", `{"shortDescription":"set \"STS\":12345 here","jsUrl":"/s/player/0a1b2c3d/player_ias.vflset/en_US/base.js"}`, pagePlayer, 0},
		{"a leading script that is not a player", `{"scriptSrc":"https:\/\/www.youtube.com\/s\/desktop\/abc\/jsbin\/desktop_polymer.vflset\/desktop_polymer.js",` +
			`"PLAYER_JS_URL":"\/s\/player\/0a1b2c3d\/player_es6.vflset\/en_US\/base.js"}`, pagePlayer, 0},
		{"no script that is a player", `{"scriptSrc":"https:\/\/www.youtube.com\/s\/desktop\/abc\/jsbin\/desktop_polymer.vflset\/desktop_polymer.js"}`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ytcfg, _ := extractYtcfgAndPlayerResponse([]byte(tc.page))
			if ytcfg.PlayerURL != tc.wantURL {
				t.Errorf("PlayerURL = %q, want %q", ytcfg.PlayerURL, tc.wantURL)
			}
			if ytcfg.STS != tc.wantSTS {
				t.Errorf("STS = %d, want %d", ytcfg.STS, tc.wantSTS)
			}
		})
	}
}

// TestExtractSTSPrefersYtcfg: ytcfg's STS is taken without opening the
// player — no cipher solver is wired here at all — and no player URL means
// no STS, as upstream sends none without one.
//
// Mutants: extractSTS ignoring ytcfg.STS (the first row reads 0 with no
// solver); the player-URL gate dropped (the second row sends the STS).
func TestExtractSTSPrefersYtcfg(t *testing.T) {
	p := newRetryTestAPI()
	if got := p.extractSTS(context.Background(), &YtcfgData{PlayerURL: pagePlayer, STS: 20381}); got != 20381 {
		t.Errorf("extractSTS = %d, want ytcfg's 20381", got)
	}
	if got := p.extractSTS(context.Background(), &YtcfgData{STS: 20381}); got != 0 {
		t.Errorf("extractSTS with no player URL = %d, want 0", got)
	}
	if got := p.extractSTS(context.Background(), nil); got != 0 {
		t.Errorf("extractSTS(nil) = %d, want 0", got)
	}
}
