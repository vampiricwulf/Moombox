package youtube

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// chapterPage wraps an initial-data literal in the page shape the extractor
// anchors on.
func chapterPage(initialData string) []byte {
	return []byte(`<html><script>var ytInitialData = ` + initialData + `;</script></html>`)
}

const playerBarChapters = `{"playerOverlays":{"playerOverlayRenderer":{"decoratedPlayerBarRenderer":{"decoratedPlayerBarRenderer":{"playerBar":{"chapteredPlayerBarRenderer":{"chapters":[
	{"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Intro"}}},
	{"chapterRenderer":{"timeRangeStartMillis":95000,"title":{"simpleText":"Main"}}},
	{"chapterRenderer":{"timeRangeStartMillis":999999999,"title":{"simpleText":"Past the end"}}}]}}}}}}}`

const panelChapters = `"engagementPanels":[
	{"engagementPanelSectionListRenderer":{"content":{"structuredDescriptionContentRenderer":{}}}},
	{"engagementPanelSectionListRenderer":{"content":{"macroMarkersListRenderer":{"contents":[
		{"macroMarkersListItemRenderer":{"timeDescription":{"simpleText":"0:00"},"title":{"simpleText":"Start"}}},
		{"macroMarkersListItemRenderer":{"timeDescription":{"runs":[{"text":"1:02:03"}]},"title":{"runs":[{"text":"Late "},{"text":"part"}]}}}]}}}}]`

// TestChaptersFromWatchPage is yt-dlp's chapter order over ytInitialData
// (owner decision D23 follow-up 2): the player bar's chapters
// (_extract_chapters_from_json), a chapter past the duration dropped; and only
// when those give nothing, the first engagement panel with macro markers
// (_extract_chapters_from_engagement_panel), its text read from simpleText or
// runs.
//
// Mutants: the player bar path not read (the panel's chapters come back for
// the first page); the panel path not read; runs not joined.
func TestChaptersFromWatchPage(t *testing.T) {
	got := ChaptersFromWatchPage(chapterPage(`{`+panelChapters+`,`+playerBarChapters[1:]), 600_000)
	want := []utils.Chapter{{StartMs: 0, Title: "Intro"}, {StartMs: 95_000, Title: "Main"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("player-bar chapters = %+v, want %+v", got, want)
	}

	got = ChaptersFromWatchPage(chapterPage(`{`+panelChapters+`}`), 4*3600_000)
	want = []utils.Chapter{{StartMs: 0, Title: "Start"}, {StartMs: 3723_000, Title: "Late part"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("engagement-panel chapters = %+v, want %+v", got, want)
	}

	if got := ChaptersFromWatchPage(chapterPage(`{"contents":{}}`), 600_000); got != nil {
		t.Errorf("a page without chapters = %+v, want none", got)
	}
}

// TestFetchChaptersFallsBackToTheDescription: a page with no chapter markers,
// and a page that cannot be fetched at all, leave the description's
// timestamps (yt-dlp's last source); a page with markers wins over them.
//
// Mutants: the description fallback removed; the page's markers ignored.
func TestFetchChaptersFallsBackToTheDescription(t *testing.T) {
	desc := "0:00 Hello\n1:00 World\n"
	descWant := []utils.Chapter{{StartMs: 0, Title: "Hello"}, {StartMs: 60_000, Title: "World"}}
	for name, page := range map[string]func(http.ResponseWriter){
		"no markers":  func(w http.ResponseWriter) { w.Write(chapterPage(`{"contents":{}}`)) },
		"page refuse": func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
		"markers win": func(w http.ResponseWriter) { w.Write(chapterPage(playerBarChapters)) },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { page(w) }), 0)
				s := NewService(cookies.NewCookieJar(), noopLogger{})
				got := s.FetchChapters(context.Background(), "test1234567", desc, 600_000)
				want := descWant
				if name == "markers win" {
					want = []utils.Chapter{{StartMs: 0, Title: "Intro"}, {StartMs: 95_000, Title: "Main"}}
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("FetchChapters = %s, want %s", fmt.Sprint(got), fmt.Sprint(want))
				}
			})
		})
	}
}

// TestFetchChaptersPassesTheConsentWall: the finalize-time chapter read is a
// YouTube page read like the others, so it sends the consent and language
// cookies they send (Auth.PageCookieHeader). An EU, EEA or UK address without
// a consented SOCS is sent to consent.youtube.com, whose page holds no
// chapter markers, and the archive then got the description's chapters or
// none. A jar that is not signed in sends SOCS=CAI; a signed-in jar sends its
// own session and consent.
//
// Mutants: the header built from the raw jar (the empty jar meets the wall
// and the description's chapters come back); the consent cookies alone sent
// for a signed-in jar (its session cookies are missing).
func TestFetchChaptersPassesTheConsentWall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		jar        []string // cookie name, value pairs
		wantCookie string
	}{
		{name: "no cookies at all", wantCookie: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "signed in, with its own consent",
			jar:        []string{"SAPISID", "sapisid-a", "LOGIN_INFO", "logininfo-a", "SOCS", "CAESEwgDEgk2NjI4NDQ2OTIaAmVuIAEaBgiA_LyuBg"},
			wantCookie: "LOGIN_INFO=logininfo-a; PREF=hl=en&tz=UTC; SAPISID=sapisid-a; SOCS=CAESEwgDEgk2NjI4NDQ2OTIaAmVuIAEaBgiA_LyuBg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &pageRecorder{}
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Host == "consent.youtube.com" {
						_, _ = w.Write([]byte(`<html>Before you continue to YouTube</html>`))
						return
					}
					rec.note(r.URL.Path, r.Header.Get("Cookie"))
					if !consented(r.Header.Get("Cookie")) {
						http.Redirect(w, r, "https://consent.youtube.com/m?continue=x&gl=NL", http.StatusFound)
						return
					}
					_, _ = w.Write(chapterPage(playerBarChapters))
				}), 0)

				jar := cookies.NewCookieJar()
				if len(tc.jar) > 0 {
					jar = loadCookieRows(t, tc.jar...)
				}
				s := NewService(jar, noopLogger{})
				got := s.FetchChapters(context.Background(), "test1234567", "0:00 Hello\n1:00 World\n", 600_000)
				want := []utils.Chapter{{StartMs: 0, Title: "Intro"}, {StartMs: 95_000, Title: "Main"}}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("FetchChapters = %s, want the page's own markers %s", fmt.Sprint(got), fmt.Sprint(want))
				}
				if sent := rec.of("/watch"); len(sent) == 0 || sent[0] != tc.wantCookie {
					t.Errorf("watch page Cookie = %q, want %q", sent, tc.wantCookie)
				}
			})
		})
	}
}
