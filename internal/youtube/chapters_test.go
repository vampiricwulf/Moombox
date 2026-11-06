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
