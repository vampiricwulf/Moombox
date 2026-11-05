package worker

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestNoJSVodStreamRefreshesOnANoJSCopy is the whole-file half of the
// owner's decision on W8's second follow-up: a stream riding a copy that
// needs no player JS (visionos's) whose URL expires mid-file refreshes onto
// the cookieless chain's copy of the same file when the fresh extraction's
// copy needs the JS — a tv winner's, its n to solve — rather than resolve
// that one and, while the challenges cannot be solved, fail and let the 403
// end the download. The download is driven for real: the setup's URLs are
// refused past the first chunk, the fresh pool holds only tv's copies with
// an n, the solver fails every challenge, and the cookieless chain serves
// visionos's copies — and both files arrive whole. Before, each stream's
// refresh resolved the tv copy, failed, kept the expired URL, and the
// download ended on HTTP 403.
//
// Mutant: drop the no-JS lookup from refreshVodURL — the download fails on
// the 403.
func TestNoJSVodStreamRefreshesOnANoJSCopy(t *testing.T) {
	srv := newVodExpiryServer(t)
	relabel := func(formats []youtube.Format, source string, level int, n bool) []youtube.Format {
		for i := range formats {
			formats[i].Source = source
			if n {
				formats[i].URL += "&n=" + nChal
			}
			formats[i] = withLevel(formats[i], level)
		}
		return formats
	}
	var rx atomic.Int32
	orig := fetchCookielessFormats
	fetchCookielessFormats = func(*youtube.Service, context.Context, string) ([]youtube.Format, error) {
		rx.Add(1)
		return relabel(srv.formats(3), "visionos", youtube.AuthLevelVisionOS, false), nil
	}
	t.Cleanup(func() { fetchCookielessFormats = orig })
	refreshes := stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: nPlayer,
			Formats: relabel(srv.formats(2), "tv_public", youtube.AuthLevelTVPublic, true)}
	})

	job, _ := reextractJob(t)
	job.Logger = discardLogger{} // two downloaders log at once
	info := &youtube.VideoInfo{PlayerURL: nPlayer, StreamStatus: youtube.StreamVOD,
		Formats: relabel(srv.formats(1), "visionos", youtube.AuthLevelVisionOS, false)}
	res, err := DownloadVod(context.Background(), job, info, failingSigSolver{}, nil, &bgutils.PotProvider{}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if res.VideoFormat.NeedsPlayerJS() || res.AudioFormat.NeedsPlayerJS() {
		t.Fatalf("setup served copies that need the player JS: %s / %s", res.VideoFormat.Source, res.AudioFormat.Source)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	o := &DownloadOrchestrator{logger: discardLogger{}}
	if err := o.runDownloaders(ctx, res); err != nil {
		t.Fatalf("download: %v", err)
	}
	for _, p := range []string{res.VideoPath, res.AudioPath} {
		if fi, err := os.Stat(p); err != nil || fi.Size() != srv.size {
			t.Errorf("%s = %v (err %v), want the whole %d-byte file", p, fi, err, srv.size)
		}
	}
	if r, n := refreshes.Load(), rx.Load(); r != 2 || n != 2 {
		t.Errorf("player-response re-fetches = %d, cookieless re-extracts = %d — want one of each per stream", r, n)
	}
}

// TestRefreshVodURLKeepsANoJSStreamNoJS pins the rule's edges on one
// refresh: a stream on a copy that needs no player JS takes the cookieless
// chain's copy of the same file when the fresh copy needs the JS; keeps the
// fresh copy — which then rides on solving — when the chain has no such
// copy, or only one that needs the JS too; asks the chain nothing when the
// fresh copy needs no JS either; and a stream that needs the JS itself is
// refreshed as it always was, the chain never asked.
//
// Mutants: drop the lookup — the chain row hands out the tv URL; drop its
// served gate (!served.NeedsPlayerJS) — the tv-stream row asks the chain;
// drop its f gate (f.NeedsPlayerJS) — the fresh-visionos row asks the
// chain; take the chain's copy whatever it needs — the chain-needs-JS row
// hands out its URL.
func TestRefreshVodURLKeepsANoJSStreamNoJS(t *testing.T) {
	const old = "http://cdn.invalid/videoplayback?expire=1&id=o-A&itag=137&clen=1000"
	const tvURL = "http://cdn.invalid/tv/videoplayback?expire=2&id=o-B&itag=137&clen=1000&n=" + nChal
	const visionosURL = "http://cdn.invalid/visionos/videoplayback?expire=2&id=o-C&itag=137&clen=1000"
	const freshVisionosURL = "http://cdn.invalid/visionos/videoplayback?expire=2&id=o-D&itag=137&clen=1000"
	tv := []youtube.Format{{Itag: 137, URL: tvURL, ContentLength: "1000", Source: "tv_public"}}
	vis := []youtube.Format{{Itag: 137, URL: visionosURL, ContentLength: "1000", Source: "visionos"}}
	for _, tc := range []struct {
		name      string
		served    youtube.Format
		fresh     []youtube.Format
		chain     []youtube.Format
		wantURL   string
		wantChain int32
	}{
		{"the fresh copy needs JS: the chain's", youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "visionos"}, tv, vis, visionosURL, 1},
		{"the chain has none: the fresh copy, solved", youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "visionos"}, tv, nil, tvURL, 1},
		{"the chain's copy needs JS too: the fresh copy", youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "visionos"}, tv,
			[]youtube.Format{{Itag: 137, URL: "http://cdn.invalid/avr/videoplayback?expire=2&id=o-E&itag=137&clen=1000&n=" + nChal, ContentLength: "1000", Source: "android_vr"}}, tvURL, 1},
		{"the fresh copy needs none: no chain", youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "visionos"},
			[]youtube.Format{{Itag: 137, URL: freshVisionosURL, ContentLength: "1000", Source: "visionos"}}, vis, freshVisionosURL, 0},
		{"a stream that needs JS: as always", youtube.Format{Itag: 137, URL: old + "&n=" + nChal, ContentLength: "1000", Source: "tv_public"}, tv, vis, tvURL, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
				return &youtube.VideoInfo{PlayerURL: nPlayer, Formats: tc.fresh}
			})
			rx := fakeCookieless(t, tc.chain, nil)
			job, logs := vodPotJob(t)
			job.YT = youtube.NewService(nil, &discardLogger{})
			// A solver that answers every n: solving works, so a fresh copy
			// that needs the JS resolves.
			solver := &orderedSolver{onN: func(_ context.Context, _, c string) (string, error) { return "solved" + c[:6], nil }}
			gotURL, _ := refreshVodURL(context.Background(), job, nPlayer, newVodRefreshState(nil, tc.served, tc.served.URL),
				solver, nil, nil, "", "VOD video")
			if strings.Split(gotURL, "&n=")[0] != strings.Split(tc.wantURL, "&n=")[0] {
				t.Errorf("refreshVodURL = %q, want %q; log: %v", gotURL, tc.wantURL, logs.lines())
			}
			if n := rx.Load(); n != tc.wantChain {
				t.Errorf("cookieless re-extracts = %d, want %d", n, tc.wantChain)
			}
		})
	}
}
