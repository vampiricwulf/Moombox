package engine

import (
	"slices"
	"testing"
)

// A DASH job runs a video and an audio downloader against the same media
// hosts. With one MediaCookieState between them the job pays for one refused
// request and logs one Warn: what the first downloader learned, the second
// sends up front.
//
// Mutant: doRequest keeping its own flag per downloader (the audio downloader
// is refused first too: four requests and two Warn lines).
func TestJobDownloadersShareTheMediaCookieState(t *testing.T) {
	host := &mediaHost{head: "5000", refuse: withoutCookies}
	serveEngineInMemory(t, host)
	logger := &secretLogger{}
	shared := &MediaCookieState{}
	video := newMediaDownloader(t, false, logger)
	audio := newMediaDownloader(t, false, logger)
	video.mediaCookies, audio.mediaCookies = shared, shared

	if _, _, err := video.fetchSegment(t.Context(), video.buildSegmentURL(1)); err != nil {
		t.Fatalf("video fetchSegment: %v", err)
	}
	if _, _, err := audio.fetchSegment(t.Context(), audio.buildSegmentURL(1)); err != nil {
		t.Fatalf("audio fetchSegment: %v", err)
	}

	want := []string{"", sessionCookieSentinel, sessionCookieSentinel}
	if got := host.cookiesSeen(); !slices.Equal(got, want) {
		t.Errorf("Cookie headers = %q, want %q: the audio downloader must not be refused a second time", got, want)
	}
	if n := warnings(logger); n != 1 {
		t.Errorf("%d Warn line(s) for one job, want one", n)
	}
	if hits := logger.containing("refusedWithoutCookies1"); len(hits) != 1 {
		t.Errorf("the Warn does not count the one request refused without cookies: %q", logger.containing("served when asked again"))
	}
}

// The option is what the worker's strategies set; a downloader given none has
// a state of its own.
func TestMediaCookiesOptionIsTheDownloadersState(t *testing.T) {
	shared := &MediaCookieState{}
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: mediaBase, OutputFile: "x", MediaCookies: shared})
	if d.mediaCookies != shared {
		t.Error("the downloader did not take the state its options named")
	}
	if own := NewSegmentDownloader(DownloaderOptions{BaseURL: mediaBase, OutputFile: "x"}); own.mediaCookies == nil || own.mediaCookies == shared {
		t.Error("a downloader given no state must have one of its own")
	}
}
