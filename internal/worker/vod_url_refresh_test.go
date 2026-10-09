package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// stubRefreshVideoInfo swaps the 403 refresh's player-response re-fetch for
// one returning fresh, and counts the calls.
func stubRefreshVideoInfo(t *testing.T, fresh func() *youtube.VideoInfo) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := refreshVideoInfo
	refreshVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
		calls.Add(1)
		return fresh(), nil
	}
	t.Cleanup(func() { refreshVideoInfo = orig })
	return &calls
}

// TestRefreshVodURLServesTheSameFile pins what the whole-file refresh hands
// the engine: a fresh URL for the file the partial is a prefix of — the same
// itag and byte length, from a client of the same GVS token class — and
// otherwise nothing, so the engine's 403 stands rather than a different
// rendition being appended mid-file. The token half follows the SERVED
// stream's client: a stream riding a missing_pot shadow stays bare.
//
// A token-free stream the fresh pool does not serve is asked of the
// cookieless chain again, as DownloadVod's missing_pot re-extract asked it:
// that stream came from there when the extraction's web_creator pool was
// adequate and carried no shadow, and a fresh extraction carries none either
// (the 2026-09-29 incident's shape). A stream that needs a token is never in
// that chain and does not ask it.
//
// Mutant: dropping `c.ContentLength == served.ContentLength` from
// sameVodFile — the re-encode row installs a URL for another file. Mutant:
// dropping `formats[i].TokenFreeAlternate` from its candidates — the shadow
// row finds nothing. Mutant: dropping `!tokenClassChanged(...)` — the shadow
// row takes the web_creator winner's URL. Mutant: dropping
// `youtube.GvsTokenRequired(served.Source) &&` from refreshVodURL — the
// shadow row mints a token for a bare URL. Mutant: bypassCache true → false
// — the tokenised row's mint reads the cache. Mutant: dropping the cookieless
// re-extract from refreshVodURL — the incident row keeps the expired URL.
// Mutant: dropping its `!youtube.GvsTokenRequired(served.Source)` gate — the
// tokenised re-encode row asks the cookieless chain. Mutant: handing the
// re-extract ctx instead of refreshCtx — the incident row's fetch carries no
// deadline.
func TestRefreshVodURLServesTheSameFile(t *testing.T) {
	const old, freshURL, shadowURL, cookielessURL = "http://cdn.invalid/videoplayback?expire=1&id=o-A&itag=137&clen=1000",
		"http://cdn.invalid/videoplayback?expire=2&id=o-B&itag=137&clen=1000",
		"http://cdn.invalid/videoplayback?expire=2&id=o-C&itag=137&clen=1000&c=ANDROID_VR",
		"http://cdn.invalid/visionos/videoplayback?expire=2&id=o-D&itag=137&clen=1000"
	// What the cookieless chain serves of itag 137: the served file, and a
	// re-encode of it.
	cookielessFile := []youtube.Format{{Itag: 137, URL: cookielessURL, ContentLength: "1000", Source: "visionos"}}
	cookielessReencode := []youtube.Format{{Itag: 137, URL: cookielessURL, ContentLength: "2000", Source: "visionos"}}
	for _, tc := range []struct {
		name           string
		served         string // the served stream's client
		fresh          []youtube.Format
		cookieless     []youtube.Format // what the cookieless chain answers
		wantURL        string
		wantToken      string
		wantCookieless int32 // cookieless re-extracts
	}{
		{"the same file re-resolves", "android_vr",
			[]youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: "android_vr"}}, cookielessFile, freshURL, "", 0},
		{"a re-encode under the same itag keeps the current URL", "android_vr",
			[]youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "2000", Source: "android_vr"}}, cookielessReencode, "", "", 1},
		{"a missing_pot stream rides the fresh shadow, bare", "android_vr",
			[]youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: "web_creator",
				TokenFreeAlternate: &youtube.Format{Itag: 137, URL: shadowURL, ContentLength: "1000", Source: "android_vr"}}}, cookielessFile, shadowURL, "", 0},
		{"a missing_pot stream the cookieless chain served is asked of it again, bare", "visionos",
			[]youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: "web_creator"}}, cookielessFile, cookielessURL, "", 1},
		{"a tokenised stream is re-minted past the cache", "web_creator",
			[]youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: "web_creator"}}, cookielessFile, freshURL, "fresh-token", 0},
		{"a tokenised re-encode asks no cookieless client", "web_creator",
			[]youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "2000", Source: "web_creator"}}, cookielessFile, "", "fresh-token", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
				return &youtube.VideoInfo{PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js", Formats: tc.fresh}
			})
			rx, sawDeadline := fakeCookielessCtx(t, tc.cookieless, nil)
			var mints int
			minter := &fakePotProvider{generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
				mints++
				if !bypassCache {
					return "cached-token", nil
				}
				return "fresh-token", nil
			}}
			job, _ := vodPotJob(t)
			job.YT = youtube.NewService(nil, &discardLogger{})
			served := youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: tc.served}

			gotURL, gotToken := refreshVodURL(context.Background(), job, "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
				served, stubCipherSolver{}, nil, minter, "vodpot", "VOD video")
			if gotURL != tc.wantURL || gotToken != tc.wantToken {
				t.Errorf("refreshVodURL = (%q, %q), want (%q, %q)", gotURL, gotToken, tc.wantURL, tc.wantToken)
			}
			if tc.wantToken == "" && mints != 0 {
				t.Errorf("minted %d tokens for a stream whose client needs none", mints)
			}
			if n := rx.Load(); n != tc.wantCookieless {
				t.Errorf("cookieless re-extracts = %d, want %d", n, tc.wantCookieless)
			} else if n > 0 && !sawDeadline.Load() {
				t.Errorf("the cookieless re-extract's ctx carries no deadline — it must run under the refresh's bound")
			}
		})
	}
}

// vodExpiryServer serves whole-file renditions by itag (2 chunks + 100 bytes
// each) from two generations of URL: expire=1 answers 403 past the first
// chunk, the way googlevideo answers once a URL's expire= has passed, and
// expire=2 serves everything. While drop is set, every request past the
// first chunk breaks off mid-body instead — a link going down mid-transfer.
type vodExpiryServer struct {
	*httptest.Server
	size  int64
	mu    sync.Mutex
	drop  bool
	drops atomic.Int32
}

func newVodExpiryServer(t *testing.T) *vodExpiryServer {
	t.Helper()
	s := &vodExpiryServer{size: 2*engine.DownloadChunkSize + 100}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end <= 0 || end >= s.size {
			end = s.size - 1
		}
		s.mu.Lock()
		drop := s.drop && start >= engine.DownloadChunkSize
		s.mu.Unlock()
		if r.URL.Query().Get("expire") == "1" && start >= engine.DownloadChunkSize {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body := make([]byte, end-start+1)
		if start == 0 {
			copy(body, "\x00\x00\x00\x18ftypdash")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, s.size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if drop {
			s.drops.Add(1)
			w.Write(body[:1000]) // short: the server drops the connection
			return
		}
		w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

// formats is the server's video 136 and audio 140 under one URL generation.
func (s *vodExpiryServer) formats(expire int) []youtube.Format {
	w, h, fps := 1280, 720, 30
	clen := strconv.FormatInt(s.size, 10)
	url := func(itag, id string) string {
		return fmt.Sprintf("%s/videoplayback?expire=%d&id=o-%s%d&itag=%s&clen=%s", s.URL, expire, id, expire, itag, clen)
	}
	return []youtube.Format{
		{Itag: 136, URL: url("136", "V"), MimeType: `video/mp4; codecs="avc1.4d401f"`, Bitrate: 1_500_000, Width: &w, Height: &h, Fps: &fps, ContentLength: clen, Source: "android_vr"},
		{Itag: 140, URL: url("140", "A"), MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 128_000, ContentLength: clen, Source: "android_vr"},
	}
}

// TestVodDownloadersWireRefreshAndConnectivity pins that the VOD strategy
// hands both whole-file downloaders the engine's two recoveries:
// OnCredentialRefresh (refreshVodURL), so a URL that expires mid-transfer is
// re-extracted for the same file instead of ending the job on a 403, and the
// orchestrator's IsOnline, so an outage is waited out instead of exhausting
// the three-attempt chunk ladder.
//
// Mutant: dropping `OnCredentialRefresh: vodURLRefresh(...)` from the video
// downloader (or the audio one) — the expiry row fails on HTTP 403. Mutant:
// dropping `IsOnline: isOnline` from the video downloader (or the audio one),
// or passing nil for `deps.IsOnline` in vodStrategyT.Download — the outage
// row fails after three attempts.
func TestVodDownloadersWireRefreshAndConnectivity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expire int // the URL generation the extraction hands DownloadVod
		outage bool
	}{
		{"a URL that expires mid-transfer is refreshed", 1, false},
		{"an outage is waited out", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newVodExpiryServer(t)
			refreshes := stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
				return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, Formats: srv.formats(2)}
			})
			var offline atomic.Bool
			if tc.outage {
				srv.mu.Lock()
				srv.drop = true
				srv.mu.Unlock()
				offline.Store(true)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("outage goroutine panicked: %v", r)
						}
					}()
					time.Sleep(600 * time.Millisecond)
					srv.mu.Lock()
					srv.drop = false
					srv.mu.Unlock()
					offline.Store(false)
				}()
			}
			job, _ := vodPotJob(t)
			job.Logger = discardLogger{} // two downloaders log at once
			job.YT = youtube.NewService(nil, &discardLogger{})
			info := &youtube.VideoInfo{PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js", StreamStatus: youtube.StreamVOD, Formats: srv.formats(tc.expire)}
			res, err := VodStrategy.Download(context.Background(), job, info, &StrategyDeps{
				RoutedCipherSolver: stubCipherSolver{},
				IsOnline:           func() bool { return !offline.Load() },
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range []*engine.SegmentDownloader{res.VideoDownloader, res.AudioDownloader} {
				d.SetFastDelaysForTests(20)
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
			if !tc.outage && refreshes.Load() != 2 {
				t.Errorf("player-response re-fetches = %d, want one per stream", refreshes.Load())
			}
		})
	}
}

// TestVodMissingPotStreamRefreshesItsURL pins the refresh end to end for the
// 2026-09-29 incident's shape: a cookied extraction whose web_creator pool is
// adequate carries no token-free shadow, the GVS mint fails, and DownloadVod
// serves the cookieless chain's visionos copies. Their URLs expire
// mid-transfer. The player-response re-fetch hands back the same web_creator
// pool, with nothing of the served token class in it, so the refresh asks the
// cookieless chain again for the same file — where it used to return nothing,
// and the job ended on the 403 the refresh exists to answer.
//
// Mutant: dropping the cookieless re-extract from refreshVodURL — the
// download fails on "HTTP 403; the URL refresh returned nothing".
func TestVodMissingPotStreamRefreshesItsURL(t *testing.T) {
	srv := newVodExpiryServer(t)
	relabel := func(formats []youtube.Format, source string, level int) []youtube.Format {
		for i := range formats {
			formats[i].Source = source
			if source == "visionos" {
				formats[i].URL = strings.Replace(formats[i].URL, "videoplayback", "visionos/videoplayback", 1)
			}
			formats[i] = withLevel(formats[i], level)
		}
		return formats
	}
	fakeVodMint(t, "", errors.New("sidecar down"))
	var rx atomic.Int32
	orig := fetchCookielessFormats
	fetchCookielessFormats = func(*youtube.Service, context.Context, string) ([]youtube.Format, error) {
		// Each call hands out the next URL generation; the setup's, expire=1,
		// expires past the first chunk.
		return relabel(srv.formats(int(rx.Add(1))), "visionos", youtube.AuthLevelVisionOS), nil
	}
	t.Cleanup(func() { fetchCookielessFormats = orig })
	refreshes := stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, Formats: relabel(srv.formats(2), "web_creator", youtube.AuthLevelWebCreator)}
	})

	job, _ := reextractJob(t)
	job.Logger = discardLogger{} // two downloaders log at once
	info := &youtube.VideoInfo{PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js", StreamStatus: youtube.StreamVOD,
		Formats: relabel(srv.formats(1), "web_creator", youtube.AuthLevelWebCreator)}
	res, err := DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, &bgutils.PotProvider{}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if res.VideoFormat.Source != "visionos" || res.AudioFormat.Source != "visionos" {
		t.Fatalf("setup served %s / %s, want the cookieless visionos copies", res.VideoFormat.Source, res.AudioFormat.Source)
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
	if r, n := refreshes.Load(), rx.Load(); r != 2 || n != 3 {
		t.Errorf("player-response re-fetches = %d, cookieless re-extracts = %d — want one of each per stream, plus the setup's re-extract", r, n)
	}
}
