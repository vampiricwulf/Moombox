package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// cookieFileRows is a cookies.txt holding name=value rows on .youtube.com.
func cookieFileRows(pairs ...string) string {
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(".youtube.com\tTRUE\t/\tTRUE\t2000000000\t" + pairs[i] + "\t" + pairs[i+1] + "\n")
	}
	return b.String()
}

// loadCookieRows writes cookieFileRows(pairs...) to the test's temp dir and
// loads it, so SyncCookies has a file to re-read.
func loadCookieRows(t *testing.T, pairs ...string) *cookies.CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(cookieFileRows(pairs...)), 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	return jar
}

// cookieValue is the value of name in a Cookie header, or "".
func cookieValue(header, name string) string {
	for pair := range strings.SplitSeq(header, "; ") {
		if k, v, ok := strings.Cut(pair, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// pageRecorder serves YouTube pages in a test and records the Cookie header
// of every request, by path.
type pageRecorder struct {
	mu      sync.Mutex
	cookies map[string][]string
}

func (r *pageRecorder) note(path, cookie string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cookies == nil {
		r.cookies = map[string][]string{}
	}
	r.cookies[path] = append(r.cookies[path], cookie)
}

func (r *pageRecorder) of(path string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cookies[path]...)
}

// syncWarnLogger records every Warn line; the cascades may log from more
// than one goroutine.
type syncWarnLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *syncWarnLogger) Debug(string, ...any) {}
func (l *syncWarnLogger) Info(string, ...any)  {}
func (l *syncWarnLogger) Error(string, ...any) {}
func (l *syncWarnLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprintf("%s %v", msg, args))
}

func (l *syncWarnLogger) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

// consented reports whether a Cookie header carries a SOCS that records a
// consent — the wall consentWall stands in front of a page without one.
func consented(cookie string) bool {
	socs := cookieValue(cookie, "SOCS")
	return socs != "" && !strings.HasPrefix(socs, "CAA")
}

// TestWatchPagesPassTheConsentWall is G1. An EU, EEA or UK address is sent
// from the watch page to consent.youtube.com unless the request carries a
// consented SOCS, and the interstitial's 200 holds no ytcfg: no player URL,
// STS or visitor data. yt-dlp sets SOCS=CAI for every session that is not
// signed in (_initialize_consent, _base.py:675-681) and passes it. Here the
// public cascade's page — fetched without the jar's session, which it still
// is — carries CAI, and so does a half-signed-in jar's; a signed-in jar
// carries the SOCS its own browser consented with, which Load used to drop,
// and none is invented for it (upstream leaves a signed-in session's consent
// alone), so the wall it meets names SOCS as the remedy, not CONSENT.
//
// Mutants: the public page fetched with "" (the public rows meet the wall);
// with the jar's own header (the half-signed-in row sends SAPISID); SOCS
// dropped from essentialYouTubeCookies (the own-consent row meets the wall);
// SOCS=CAI added for a signed-in jar too (the last row's page carries it);
// the CONSENT advice restored (the last row's warning).
func TestWatchPagesPassTheConsentWall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		jar        []string // cookie name, value pairs
		authed     bool
		wantCookie string
		wantWall   bool
	}{
		{name: "public, no cookies at all", wantCookie: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "public, half signed in: no credential rides on the page",
			jar: []string{"SAPISID", "sapisid-a", "VISITOR_INFO1_LIVE", "vis"}, wantCookie: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "authenticated, with its own consent", authed: true,
			jar:        []string{"SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a", "SOCS", "CAESEwgDEgk2NjI4NDQ2OTIaAmVuIAEaBgiA_LyuBg"},
			wantCookie: "LOGIN_INFO=logininfo-a; PREF=hl=en&tz=UTC; SAPISID=sapisid-a; SOCS=CAESEwgDEgk2NjI4NDQ2OTIaAmVuIAEaBgiA_LyuBg"},
		{name: "authenticated, without a consent of its own", authed: true, wantWall: true,
			jar:        []string{"SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a"},
			wantCookie: "LOGIN_INFO=logininfo-a; PREF=hl=en&tz=UTC; SAPISID=sapisid-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &pageRecorder{}
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Host == "consent.youtube.com" {
						rec.note("consent", r.Header.Get("Cookie"))
						_, _ = w.Write([]byte(`<html>Before you continue to YouTube</html>`))
						return
					}
					rec.note(r.URL.Path, r.Header.Get("Cookie"))
					if !consented(r.Header.Get("Cookie")) {
						http.Redirect(w, r, "https://consent.youtube.com/m?continue=x&gl=NL", http.StatusFound)
						return
					}
					_, _ = w.Write([]byte(watchPageFixture))
				}), 0)
				serveBodies(t, nil)

				jar := cookies.NewCookieJar()
				if len(tc.jar) > 0 {
					jar = loadCookieRows(t, tc.jar...)
				}
				log := &syncWarnLogger{}
				p := NewPlayerAPI(NewAuth(jar, noopLogger{}), log)
				var visitor []string
				p.OnVisitorData = func(vd string) { visitor = append(visitor, vd) }
				get := p.GetVideoInfoPublic
				if tc.authed {
					get = p.GetVideoInfoAuthenticated
				}
				if _, err := get(context.Background(), "test1234567"); err != nil {
					t.Fatalf("extraction: %v", err)
				}

				pages := rec.of("/watch")
				if len(pages) != 1 || pages[0] != tc.wantCookie {
					t.Fatalf("watch page Cookie = %q, want one request with %q", pages, tc.wantCookie)
				}
				walled := len(rec.of("consent")) > 0
				if walled != tc.wantWall {
					t.Fatalf("consent wall reached = %v, want %v", walled, tc.wantWall)
				}
				if !tc.wantWall {
					if len(visitor) != 1 || visitor[0] != "vd-page" {
						t.Errorf("visitor data captured %q, want the page's vd-page — the page's ytcfg was not read", visitor)
					}
					for _, w := range log.lines() {
						if strings.Contains(w, "Could not fetch watch page") {
							t.Errorf("the watch page failed: %s", w)
						}
					}
					return
				}
				var warned bool
				for _, w := range log.lines() {
					if strings.Contains(w, "redirected to consent wall (consent.youtube.com)") {
						warned = true
						if !strings.Contains(w, "SOCS") || strings.Contains(w, "CONSENT") {
							t.Errorf("the wall's advice = %s, want it to name SOCS, not CONSENT", w)
						}
					}
				}
				if !warned {
					t.Errorf("no consent-wall warning among %q", log.lines())
				}
			})
		})
	}
}

// membersOnlyPage is a watch page whose embedded player response refuses
// test1234567 as members-only, its reason in reasonLang: YouTube's English
// text, or the Japanese a PREF hl=ja gets.
func membersOnlyPage(reasonLang string) string {
	reason := "Join this channel to get access to members-only content like this video, and other exclusive perks."
	if reasonLang == "ja" {
		reason = "このチャンネルのメンバーになると、この動画などの限定コンテンツにアクセスできます。"
	}
	return `<html><script>ytcfg.set({"STS":20381,"visitorData":"vd-page",` +
		`"PLAYER_JS_URL":"\/s\/player\/0a1b2c3d\/player_es6.vflset\/en_US\/base.js"});</script>` +
		`<script>var ytInitialPlayerResponse = {"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"` + reason + `"},` +
		`"videoDetails":{"videoId":"test1234567","title":"t","author":"a"}};</script></html>`
}

// TestSignedInWatchPageIsReadInEnglish is G20. The authenticated cascade's
// members-only verdict is the watch page's own parse (TV and WEB_CREATOR
// agree, and the cookieless chain is skipped), and that parse matches
// English keywords. A user whose YouTube language is Japanese exports a PREF
// with hl=ja, which outranks Accept-Language, so the page's refusal came back
// in Japanese, matched neither "member" nor "join", and the job parked in
// COOKIES? "Login required" instead of telling the operator the account is
// not a member. yt-dlp rewrites PREF to hl=en&tz=UTC, keeping its other keys
// (_initialize_pref, _base.py:683-692); so does the page header now.
//
// The page here answers in the language its request's PREF names, as
// YouTube's does.
//
// Mutants: the authenticated page fetched with GetCookieHeader (the verdict
// is login_required); englishPref keeping the jar's hl (same); englishPref
// dropping the other keys (the PREF assertion).
func TestSignedInWatchPageIsReadInEnglish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &pageRecorder{}
		servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie := r.Header.Get("Cookie")
			rec.note(r.URL.Path, cookie)
			lang := "en"
			if pref, err := url.ParseQuery(cookieValue(cookie, "PREF")); err == nil && pref.Get("hl") != "" {
				lang = pref.Get("hl")
			}
			_, _ = w.Write([]byte(membersOnlyPage(lang)))
		}), 0)
		const refused = `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Join this channel to get access to members-only content like this video, and other exclusive perks."},` +
			`"videoDetails":{"videoId":"test1234567","title":"t","author":"a"}}`
		serveBodies(t, map[string]string{
			constants.TVSamsungClient.ClientID:   refused,
			constants.WebEmbeddedClient.ClientID: refused,
			constants.WebCreatorClient.ClientID:  refused,
		})

		jar := loadCookieRows(t, "SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a",
			"PREF", "f6=40000000&tz=Asia.Tokyo&hl=ja")
		p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
		info, err := p.GetVideoInfoAuthenticated(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("GetVideoInfoAuthenticated: %v", err)
		}

		pages := rec.of("/watch")
		if len(pages) != 1 {
			t.Fatalf("watch page asked %d times, want once", len(pages))
		}
		if got, want := cookieValue(pages[0], "PREF"), "f6=40000000&tz=UTC&hl=en"; got != want {
			t.Errorf("watch page PREF = %q, want %q (hl=en and tz=UTC, f6 kept)", got, want)
		}
		if info.PlayabilityError != PlayabilityMembersOnly {
			t.Errorf("verdict = %q (%q), want members_only — the page was read in a language its keywords do not match",
				info.PlayabilityError, info.PlayabilityReason)
		}
	})
}

// TestHomepageIsFetchedWithThePageCookies: the startup homepage is where the
// visitor data, API key and first player URL come from, and the consent wall
// stands in front of it as in front of the watch page. A jar that is not
// signed in sends SOCS=CAI; a signed-in one its cookies and an English PREF,
// no SOCS invented.
//
// Mutant: fetchHomepage sending GetCookieHeader (both rows).
func TestHomepageIsFetchedWithThePageCookies(t *testing.T) {
	for _, tc := range []struct {
		name string
		jar  []string
		want string
	}{
		{name: "no cookies", want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "signed in, a German PREF", jar: []string{"SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a", "PREF", "hl=de&f6=40000000"},
			want: "LOGIN_INFO=logininfo-a; PREF=hl=en&f6=40000000&tz=UTC; SAPISID=sapisid-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := stubHomepage(t, nil)
			jar := cookies.NewCookieJar()
			if len(tc.jar) > 0 {
				jar = loadCookieRows(t, tc.jar...)
			}
			NewService(jar, noopLogger{}).Init(context.Background())
			if got := stub.fetches(); len(got) != 1 || got[0] != tc.want {
				t.Errorf("homepage Cookie = %q, want one fetch with %q", got, tc.want)
			}
		})
	}
}

// TestEmbedPageCarriesTheConsentCookies: the embed page web_embedded reads
// encryptedHostFlags and a player URL off is fetched without the jar's
// session, as it always was, but behind the same SOCS the watch page passes
// the wall with — none of the signed-in jar's credentials.
//
// Mutants: FetchEmbedPage sending no Cookie (the request carries none); the
// call site passing PageCookieHeader (SAPISID rides along, and no SOCS).
func TestEmbedPageCarriesTheConsentCookies(t *testing.T) {
	rec := &pageRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.note("embed", r.Header.Get("Cookie"))
		_, _ = w.Write([]byte(`<html><script>ytcfg.set({"encryptedHostFlags":"ehf"});</script></html>`))
	}))
	t.Cleanup(srv.Close)
	orig := constants.YouTubeURLs.Embed
	constants.YouTubeURLs.Embed = srv.URL + "/embed"
	t.Cleanup(func() { constants.YouTubeURLs.Embed = orig })
	serveBodies(t, nil)

	jar := loadCookieRows(t, "SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a", "PREF", "hl=fr")
	p := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{})
	if _, err := p.fetchWithEmbedded(context.Background(), "test1234567", DefaultYtcfg(), 0, true); err != nil {
		t.Fatalf("fetchWithEmbedded: %v", err)
	}
	if got, want := rec.of("embed"), "PREF=hl=en&tz=UTC; SOCS=CAI"; len(got) != 1 || got[0] != want {
		t.Errorf("embed page Cookie = %q, want one fetch with %q", got, want)
	}
}

// TestLivenessPagesCarryThePageCookies: the account and membership probes
// read their markers and the membership video list off pages fetched with the
// session, in English now that PREF says hl=en, and a jar that is not fully
// signed in sends SOCS=CAI, so a consent wall no longer stands between a
// dead session and the LoggedOut its page states. The anonymous refusal is
// still asked of the jar's own cookies (TestFetchLivenessPageRefusesAnAnonymousFetch).
//
// Mutant: fetchLivenessPage sending GetCookieHeader (both rows).
func TestLivenessPagesCarryThePageCookies(t *testing.T) {
	for _, tc := range []struct {
		name string
		jar  []string
		want string
	}{
		{name: "signed in, a Japanese PREF", jar: []string{"SAPISID", "sap", "LOGIN_INFO", "li", "PREF", "f6=40000000&hl=ja"},
			want: "LOGIN_INFO=li; PREF=f6=40000000&hl=en&tz=UTC; SAPISID=sap"},
		{name: "LOGIN_INFO gone", jar: []string{"SAPISID", "sap"},
			want: "PREF=hl=en&tz=UTC; SAPISID=sap; SOCS=CAI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pageRecorder{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.note("page", r.Header.Get("Cookie"))
				_, _ = w.Write([]byte(`ytcfg.set({"LOGGED_IN":true});`))
			}))
			t.Cleanup(srv.Close)
			s := jarServiceFromCookieFile(t, cookieFileRows(tc.jar...))
			if _, err := s.fetchLivenessPage(t.Context(), srv.URL); err != nil {
				t.Fatalf("fetchLivenessPage: %v", err)
			}
			if got := rec.of("page"); len(got) != 1 || got[0] != tc.want {
				t.Errorf("liveness page Cookie = %q, want one fetch with %q", got, tc.want)
			}
		})
	}
}

// innertubeClientContext decodes a player or browse request body's
// context.client.
func innertubeClientContext(t *testing.T, body string) map[string]any {
	t.Helper()
	var req struct {
		Context struct {
			Client map[string]any `json:"client"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request body %q: %v", body, err)
	}
	return req.Context.Client
}

// TestInnertubeRequestsStateEnglishAndUTC is G20's Innertube half: every
// player call either cascade makes, and the /browse body, carries hl=en,
// timeZone=UTC and utcOffsetMinutes=0 — yt-dlp's _extract_context
// (_base.py:753-759) — whatever the call adds to the client's Context.
//
// Mutants: the zone fields dropped from a client's Context (that client's
// calls); a request builder that stops copying the Context (every call).
func TestInnertubeRequestsStateEnglishAndUTC(t *testing.T) {
	check := func(t *testing.T, what, body string) {
		t.Helper()
		c := innertubeClientContext(t, body)
		if c["hl"] != "en" || c["timeZone"] != "UTC" || c["utcOffsetMinutes"] != float64(0) {
			t.Errorf("%s: hl=%v timeZone=%v utcOffsetMinutes=%v, want en, UTC, 0", what, c["hl"], c["timeZone"], c["utcOffsetMinutes"])
		}
	}
	for _, authed := range []bool{false, true} {
		t.Run(fmt.Sprintf("authenticated=%v", authed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(watchPageFixture))
				}), 0)
				// An inadequate TV answer, so the cascades go on to the
				// clients behind it.
				tr := serveBodies(t, map[string]string{constants.TVSamsungClient.ClientID: `{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"test1234567"}}`})
				jar := cookies.NewCookieJar()
				get := NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{}).GetVideoInfoPublic
				if authed {
					jar = loadCookieRows(t, "SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a")
					get = NewPlayerAPI(NewAuth(jar, noopLogger{}), noopLogger{}).GetVideoInfoAuthenticated
				}
				if _, err := get(context.Background(), "test1234567"); err != nil {
					t.Fatalf("extraction: %v", err)
				}
				calls := 0
				for _, c := range []constants.YouTubeClientConfig{
					constants.TVSamsungClient, constants.WebEmbeddedClient, constants.WebCreatorClient,
					constants.WebSafariClient, constants.WebClient, constants.VisionOSClient, constants.AndroidVRClient,
				} {
					for _, body := range tr.of(c.ClientID) {
						calls++
						check(t, c.ClientName, body)
					}
				}
				if calls < 2 {
					t.Fatalf("%d player calls recorded, want the cascade's", calls)
				}
			})
		})
	}
	t.Run("browse", func(t *testing.T) {
		svc := NewService(cookies.NewCookieJar(), noopLogger{})
		b, err := json.Marshal(svc.browseRequestBody("UCx", "params", "", "vd"))
		if err != nil {
			t.Fatal(err)
		}
		check(t, "browse", string(b))
	})
}
