package worker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The pre-roll ad wait driven for real: each strategy and each refresh runs
// inside a synctest bubble, so a 15 s wait passes in no time and every check
// lands on an exact instant of the bubble's clock. The bubble's clock starts
// on a whole second, so upstream's integer-clock floor (playbackWaitLeft)
// takes nothing off the waits below.

// preRollWaitMsg is the Info line a strategy's wait logs.
const preRollWaitMsg = "waiting out the player response's pre-roll ad before the first media request, as YouTube requires"

// strategyRun is a strategy (or a refresh) running in its own goroutine.
type strategyRun struct {
	done chan struct{}
	res  *DownloadResult
	err  error
}

func startStrategy(t *testing.T, download func() (*DownloadResult, error)) *strategyRun {
	t.Helper()
	r := &strategyRun{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("strategy panicked: %v", p)
			}
		}()
		r.res, r.err = download()
	}()
	return r
}

func (r *strategyRun) returned() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// rowProgress is the row's progress line.
func rowProgress(t *testing.T, job *JobContext) string {
	t.Helper()
	row, err := job.DB.GetJob(job.Job.ID)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	return row.Progress
}

// assertWaitsExactly checks that a started run is still waiting a moment
// before want has passed and has returned once it has, and that the wait's
// progress line is gone by then.
func assertWaitsExactly(t *testing.T, job *JobContext, r *strategyRun, want time.Duration) {
	t.Helper()
	time.Sleep(want - time.Millisecond)
	synctest.Wait()
	if r.returned() {
		t.Fatalf("returned before its %v wait was over (err %v)", want, r.err)
	}
	time.Sleep(time.Millisecond)
	synctest.Wait()
	if !r.returned() {
		t.Fatalf("still waiting once its %v wait was over", want)
	}
	if r.err != nil {
		t.Fatalf("returned %v after the wait, want nil", r.err)
	}
	if job.DB != nil {
		if got := rowProgress(t, job); got != "" {
			t.Errorf("progress line after the wait = %q, want it cleared", got)
		}
	}
}

// assertNoWait checks that a started run returned without the clock moving,
// logged no wait and wrote no wait line.
func assertNoWait(t *testing.T, job *JobContext, logs *captureLogger, r *strategyRun) {
	t.Helper()
	synctest.Wait()
	if !r.returned() {
		t.Fatal("still waiting, want it to start at once")
	}
	if r.err != nil {
		t.Fatalf("returned %v, want nil", r.err)
	}
	if l := logLine(logs, preRollWaitMsg); l != nil {
		t.Errorf("logged a pre-roll wait it did not make: %v", l)
	}
	if got := rowProgress(t, job); strings.Contains(got, playbackWaitProgress) {
		t.Errorf("progress line = %q, want no wait line", got)
	}
}

// adStamped returns info's formats with AvailableAt set to at.
func adStamped(info *youtube.VideoInfo, at time.Time) *youtube.VideoInfo {
	for i := range info.Formats {
		info.Formats[i].AvailableAt = at
	}
	return info
}

// TestPlaybackWaitLineCarriesTheWaitMarker pins the TUI's contract for the
// wait's progress line (internal/tui isWaitProgress): a Downloading row
// waiting out a pre-roll ad reads as a wait, not as its frozen percent.
//
// Mutant: playbackWaitLine returning playbackWaitProgress alone.
func TestPlaybackWaitLineCarriesTheWaitMarker(t *testing.T) {
	if got, want := playbackWaitLine(12*time.Second), "Waiting out YouTube's pre-roll ad... (12s left)"; got != want {
		t.Errorf("playbackWaitLine(12s) = %q, want %q", got, want)
	}
	if msg := playbackWaitLine(0); !strings.Contains(msg, "... (") {
		t.Errorf("playbackWaitLine(0) = %q lacks the %q wait marker", msg, "... (")
	}
}

// TestPlaybackWaitLeftFollowsUpstreamsClock pins the downloader half's
// arithmetic: upstream sleeps available_at - int(time.time()), the second it
// is in counted whole; nothing waits for a stream upstream calls live or
// post-live, whose formats it never stamps — its own live_status
// (PlaybackWaitExempt), not the extraction's status, which calls a finished
// broadcast post_live that upstream waits for; a format with no ad (the zero
// time) or an instant already past waits nothing. latestPlayback is
// upstream's max() over the formats a download fetches.
//
// Mutants: now not truncated to its second (the fractional row waits 5.6 s);
// the PlaybackWaitApplies gate dropped (the exempt rows wait); the gate
// asked of the status instead (the finished broadcast's row waits nothing,
// the post-live DVR row 15 s); the max(…, 0) dropped (the no-ad and past
// rows go negative); latestPlayback taking the first format, or naming the
// wrong one's client.
func TestPlaybackWaitLeftFollowsUpstreamsClock(t *testing.T) {
	base := time.Unix(1_700_000_100, 0)
	waits := &youtube.VideoInfo{StreamStatus: youtube.StreamVOD}
	for _, tc := range []struct {
		name string
		info *youtube.VideoInfo
		at   time.Time
		now  time.Time
		want time.Duration
	}{
		{"a processed VOD waits", waits, base.Add(15 * time.Second), base, 15 * time.Second},
		{"a plain upload waits", &youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream}, base.Add(15 * time.Second), base, 15 * time.Second},
		{"a finished broadcast the extraction calls post_live waits", &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive}, base.Add(15 * time.Second), base, 15 * time.Second},
		{"the second now is in counts whole", waits, base.Add(6 * time.Second), base.Add(400 * time.Millisecond), 6 * time.Second},
		{"a live stream never waits", &youtube.VideoInfo{StreamStatus: youtube.StreamLive, PlaybackWaitExempt: true}, base.Add(15 * time.Second), base, 0},
		{"a post-live DVR the extraction calls a VOD never waits", &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlaybackWaitExempt: true}, base.Add(15 * time.Second), base, 0},
		{"no ad", waits, time.Time{}, base, 0},
		{"an instant already past", waits, base.Add(-time.Second), base, 0},
	} {
		if got := playbackWaitLeft(tc.info, tc.at, tc.now); got != tc.want {
			t.Errorf("%s: playbackWaitLeft = %v, want %v", tc.name, got, tc.want)
		}
	}

	at, source := latestPlayback(nil,
		&youtube.Format{AvailableAt: base.Add(5 * time.Second), Source: "visionos"},
		&youtube.Format{AvailableAt: base.Add(15 * time.Second), Source: "tv_auth"},
		&youtube.Format{Source: "web_creator"})
	if !at.Equal(base.Add(15*time.Second)) || source != "tv_auth" {
		t.Errorf("latestPlayback = (%v, %q), want (%v, tv_auth)", at, source, base.Add(15*time.Second))
	}
	if at, source := latestPlayback(nil, &youtube.Format{Source: "tv_auth"}); !at.IsZero() || source != "" {
		t.Errorf("latestPlayback of formats with no ad = (%v, %q), want the zero time and no client", at, source)
	}
}

// TestDownloadVodWaitsOutThePreRollAd is the field case: a non-Premium
// account's VOD whose every URL was refused at its first request, the size
// probe, because the player response carried a pre-roll ad and Moombox asked
// at once. DownloadVod now returns its downloaders — whose first request is
// that probe — only once the latest instant of the formats it chose has
// passed, the row's progress line counting down meanwhile and one Info line
// naming the seconds and the client. A missing_pot degrade waits for the
// response of the shadow it swaps in, not the winner's.
//
// Mutants (each run): the wait dropped from DownloadVod (returns at once);
// latestPlayback over the video alone (the audio-led row waits 10 s, not
// 15 s); the wait taken before the missing_pot degrade, on the setup
// selection (the shadow row waits 5 s, not 20 s); the progress line not
// written (the first checkpoint), not rewritten (the 6 s checkpoint), or not
// cleared (the end); the Info line dropped or naming another client; the
// wait gated on the extraction's status (the post_live broadcast returns at
// once).
func TestDownloadVodWaitsOutThePreRollAd(t *testing.T) {
	t.Run("the latest of the chosen formats", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			job, logs := vodPotJob(t)
			now := time.Now()
			info := vodPotInfo("tv_auth", "tv_auth")
			info.StreamStatus = youtube.StreamVOD
			info.Formats[0].AvailableAt = now.Add(10 * time.Second)
			info.Formats[1].AvailableAt = now.Add(15 * time.Second)

			r := startStrategy(t, func() (*DownloadResult, error) {
				return DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, nil, nil)
			})
			synctest.Wait()
			if r.returned() {
				t.Fatalf("DownloadVod returned at once (err %v), want it to wait out the 15 s ad", r.err)
			}
			if got, want := rowProgress(t, job), "Waiting out YouTube's pre-roll ad... (15s left)"; got != want {
				t.Errorf("progress line as the wait starts = %q, want %q", got, want)
			}
			line := logLine(logs, preRollWaitMsg)
			if line == nil {
				t.Fatalf("no pre-roll wait line logged: %v", logs.lines())
			}
			if line["seconds"] != 15.0 || line["client"] != "tv_auth" || line["tag"] != "VOD" || line["jobID"] != job.Job.ID {
				t.Errorf("pre-roll wait line = %v, want seconds=15 client=tv_auth tag=VOD jobID=%s", line, job.Job.ID)
			}
			time.Sleep(6 * time.Second)
			synctest.Wait()
			if got, want := rowProgress(t, job), "Waiting out YouTube's pre-roll ad... (10s left)"; got != want {
				t.Errorf("progress line 6 s in = %q, want %q", got, want)
			}
			assertWaitsExactly(t, job, r, 9*time.Second)
			if r.res.VideoDownloader == nil || r.res.AudioDownloader == nil {
				t.Errorf("want both downloaders after the wait, got video=%v audio=%v", r.res.VideoDownloader, r.res.AudioDownloader)
			}
			if n := len(logLines(logs, func(m string) bool { return m == preRollWaitMsg })); n != 1 {
				t.Errorf("pre-roll wait lines = %d, want exactly 1", n)
			}
		})
	})

	t.Run("a missing_pot shadow waits for its own response", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fakeVodMint(t, "", errors.New("sidecar down"))
			job, logs := vodPotJob(t)
			now := time.Now()
			info := vodPotInfo("web_creator", "web_creator")
			info.StreamStatus = youtube.StreamVOD
			for i := range info.Formats {
				f := &info.Formats[i]
				f.AvailableAt = now.Add(5 * time.Second)
				shadow := *f
				shadow.Source, shadow.URL = "visionos", f.URL+"&c=VISIONOS"
				shadow.AvailableAt = now.Add(20 * time.Second)
				f.TokenFreeAlternate = &shadow
			}

			r := startStrategy(t, func() (*DownloadResult, error) {
				return DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
			})
			synctest.Wait()
			if r.returned() {
				t.Fatalf("DownloadVod returned at once (err %v), want it to wait out the shadow's ad", r.err)
			}
			assertWaitsExactly(t, job, r, 20*time.Second)
			if line := logLine(logs, preRollWaitMsg); line == nil || line["client"] != "visionos" {
				t.Errorf("pre-roll wait line = %v, want one naming client=visionos", line)
			}
			if got := r.res.VideoFormat.Source; got != "visionos" {
				t.Errorf("video served from %q, want the visionos shadow", got)
			}
		})
	})

	// The reviewer's case: an archived broadcast whose status came from a
	// WEB-family answer (web_creator after a failed TV, the watch page) reads
	// post_live off the microformat's endTimestamp, but upstream calls it
	// was_live and waits — and so does this.
	t.Run("a finished broadcast the extraction calls post_live", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			job, _ := vodPotJob(t)
			info := adStamped(vodPotInfo("web_creator", "web_creator"), time.Now().Add(15*time.Second))
			info.StreamStatus = youtube.StreamPostLive
			r := startStrategy(t, func() (*DownloadResult, error) {
				return DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, nil, nil)
			})
			synctest.Wait()
			if r.returned() {
				t.Fatalf("DownloadVod returned at once (err %v), want it to wait out the 15 s ad upstream waits for", r.err)
			}
			assertWaitsExactly(t, job, r, 15*time.Second)
		})
	})
}

// TestDownloadVodWithoutAnAdStartsAtOnce: no ad, an instant already past, or
// the stamped formats of a stream upstream calls live (the strategy's
// fallback for a live pool with no manifest) or post-live — a TV answer in
// the post-live DVR window states isPostLiveDvr and no microformat, so the
// extraction calls it a VOD — none waits, logs or writes a wait line. A
// Premium account's responses never carry an ad, so its downloads are
// exactly as they were.
//
// Mutants: the PlaybackWaitApplies gate dropped from playbackWaitLeft (the
// live and post-live rows wait); the gate asked of the status instead (the
// post-live DVR row waits).
func TestDownloadVodWithoutAnAdStartsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status youtube.StreamStatus
		exempt bool
		in     time.Duration // the ad's instant from now; 0: no ad
	}{
		{"no ad", youtube.StreamVOD, false, 0},
		{"an instant already past", youtube.StreamVOD, false, -time.Second},
		{"a live stream", youtube.StreamLive, true, 15 * time.Second},
		{"a post-live DVR the extraction calls a VOD", youtube.StreamVOD, true, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				job, logs := vodPotJob(t)
				info := vodPotInfo("tv_auth", "tv_auth")
				info.StreamStatus, info.PlaybackWaitExempt = tc.status, tc.exempt
				if tc.in != 0 {
					adStamped(info, time.Now().Add(tc.in))
				}
				r := startStrategy(t, func() (*DownloadResult, error) {
					return DownloadVod(context.Background(), job, info, stubCipherSolver{}, nil, nil, nil)
				})
				assertNoWait(t, job, logs, r)
			})
		})
	}
}

// TestPreRollWaitEndsOnCancel: a job cancelled (or a shutdown) during the
// wait ends it at once, the strategy returning the cancel as its setup error
// — which processJob routes to handleCancellation by ctx — with the wait line
// cleared.
//
// Mutant: the ctx.Done arm dropped from waitForPlayback (the run returns
// only when the ad is over, with no error).
func TestPreRollWaitEndsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job, _ := vodPotJob(t)
		info := adStamped(vodPotInfo("tv_auth", "tv_auth"), time.Now().Add(30*time.Second))
		info.StreamStatus = youtube.StreamVOD
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := startStrategy(t, func() (*DownloadResult, error) {
			return DownloadVod(ctx, job, info, stubCipherSolver{}, nil, nil, nil)
		})
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if r.returned() {
			t.Fatalf("returned before the cancel (err %v)", r.err)
		}
		cancel()
		synctest.Wait()
		if !r.returned() {
			t.Fatal("still waiting after the cancel")
		}
		if !errors.Is(r.err, context.Canceled) || r.res != nil {
			t.Errorf("DownloadVod = (%v, %v), want (nil, an error wrapping context.Canceled)", r.res, r.err)
		}
		if got := rowProgress(t, job); got != "" {
			t.Errorf("progress line after the cancel = %q, want it cleared", got)
		}
	})
}

// manifestHandler serves body and records the bubble instant of each request.
type manifestHandler struct {
	body string
	mu   sync.Mutex
	at   []time.Time
}

func (h *manifestHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.at = append(h.at, time.Now())
	h.mu.Unlock()
	_, _ = w.Write([]byte(h.body))
}

func (h *manifestHandler) requests() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Time(nil), h.at...)
}

// serveInBubble points the strategies' manifest fetch (workerHTTPClient) at
// h, in-process, so the fetch runs on the bubble's clock.
func serveInBubble(t *testing.T, h http.Handler) {
	t.Helper()
	orig := workerHTTPClient
	workerHTTPClient = httpxtest.NewClient(h)
	t.Cleanup(func() { workerHTTPClient = orig })
}

// TestManifestStrategiesWaitAfterTheManifest: the DASH and HLS strategies
// fetch the manifest first — upstream fetches it during extraction, before
// its sleep — and then hold their downloaders until the manifest's own
// response allows media requests: a finished stream's only — a broadcast
// the extraction calls post_live included, when upstream calls it was_live.
// A live stream's manifest, stamped or not, starts at once.
//
// Mutants (each run): the wait dropped from DownloadDash or DownloadHls (the
// VOD row returns at once); the wait placed before the manifest fetch (the
// fetch lands at the ad's end, not at 0); the manifest's own instant swapped
// for the formats' (no wait — the formats here carry none); the wait gated on
// the status (the post_live row returns at once).
func TestManifestStrategiesWaitAfterTheManifest(t *testing.T) {
	for _, kind := range []string{"DASH", "HLS"} {
		for _, tc := range []struct {
			status youtube.StreamStatus
			exempt bool
			wait   bool
		}{
			{youtube.StreamVOD, false, true},
			{youtube.StreamPostLive, false, true},
			{youtube.StreamLive, true, false},
		} {
			t.Run(kind+"/"+string(tc.status), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					job, logs := vodPotJob(t)
					start := time.Now()
					info := &youtube.VideoInfo{StreamStatus: tc.status, PlaybackWaitExempt: tc.exempt}
					var download func() (*DownloadResult, error)
					h := &manifestHandler{}
					if kind == "DASH" {
						h.body = potTestMPD
						info.DashManifestURL, info.DashManifestSource = "http://m.invalid/api/manifest/dash/id/abc", "web_safari"
						info.DashManifestAvailableAt = start.Add(8 * time.Second)
						download = func() (*DownloadResult, error) {
							return DownloadDash(context.Background(), job, info, nil, nil, nil, nil)
						}
					} else {
						h.body = potTestMaster
						info.HlsManifestURL, info.HlsManifestSource = "http://m.invalid/api/manifest/hls_variant/id/abc/file/index.m3u8", "tv_auth"
						info.HlsManifestAvailableAt = start.Add(8 * time.Second)
						download = func() (*DownloadResult, error) {
							return DownloadHls(context.Background(), job, info, nil, nil, nil, nil)
						}
					}
					serveInBubble(t, h)

					r := startStrategy(t, download)
					if !tc.wait {
						assertNoWait(t, job, logs, r)
						return
					}
					synctest.Wait()
					if r.returned() {
						t.Fatalf("returned at once (err %v), want it to wait out the manifest's 8 s ad", r.err)
					}
					if got := h.requests(); len(got) != 1 || !got[0].Equal(start) {
						t.Errorf("manifest requested at %v, want once, at the start (%v) — before the wait", got, start)
					}
					assertWaitsExactly(t, job, r, 8*time.Second)
					if line := logLine(logs, preRollWaitMsg); line == nil || line["client"] != info.DashManifestSource+info.HlsManifestSource || line["tag"] != kind {
						t.Errorf("pre-roll wait line = %v, want one naming the manifest's client and tag=%s", line, kind)
					}
				})
			})
		}
	}
}

// TestManifestlessWaitsOnlyOffLive: the manifest-free strategy serves live
// and post-live streams, which upstream never stamps, so their formats — ad
// or not — start at once; a stream upstream no longer calls either while its
// formats are still segmented (a refresh can hand it one: processed into a
// VOD, or a finished broadcast the extraction still calls post_live off its
// endTimestamp) waits for the chosen formats' latest instant.
//
// Mutants: the wait dropped from DownloadManifestlessDash (the waiting rows
// return at once); the gate dropped (the exempt rows wait); the gate asked of
// the status (the was_live row returns at once).
func TestManifestlessWaitsOnlyOffLive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status youtube.StreamStatus
		exempt bool
		wait   bool
	}{
		{"processed into a VOD", youtube.StreamVOD, false, true},
		{"was_live, the extraction says post_live", youtube.StreamPostLive, false, true},
		{"post_live", youtube.StreamPostLive, true, false},
		{"live", youtube.StreamLive, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				job, logs := vodPotJob(t)
				now := time.Now()
				info := manifestlessPotInfo("tv_auth", "tv_auth")
				info.StreamStatus, info.PlaybackWaitExempt = tc.status, tc.exempt
				info.Formats[0].AvailableAt = now.Add(7 * time.Second)
				info.Formats[1].AvailableAt = now.Add(4 * time.Second)
				r := startStrategy(t, func() (*DownloadResult, error) {
					return DownloadManifestlessDash(context.Background(), job, info, stubCipherSolver{}, nil, nil, nil)
				})
				if !tc.wait {
					assertNoWait(t, job, logs, r)
					return
				}
				synctest.Wait()
				if r.returned() {
					t.Fatalf("returned at once (err %v), want it to wait out the 7 s ad", r.err)
				}
				assertWaitsExactly(t, job, r, 7*time.Second)
			})
		})
	}
}

// TestRefreshVodURLWaitsOutTheFreshAd: a whole-file refresh's URL comes from
// a FRESH player response, which carries a pre-roll ad of its own — the
// field log's two fresh tv_auth URLs, each refused at once — so the refresh
// hands the engine that URL only once the ad is over. The wait is on the
// job's context, not the refresh budget: at the 30 s MaximumTimeout floor
// that budget is 10 s, shorter than this 12 s ad, and the whole-file path
// has no stall clock running while it waits. A cancel during the wait keeps
// the current URL. A fresh response upstream calls post-live (or live) is
// never waited for, whatever the extraction's status says — a TV answer in
// the post-live DVR window reads as a VOD here.
//
// Mutants (each run): the wait dropped from refreshVodURL (the URL comes
// back at once); the wait put on refreshCtx (the 12 s ad outlasts the 10 s
// budget and the URL never comes back); the wait not ending on the cancel;
// the wait's gate read off anything but the fresh extraction's live_status —
// the status, or the setup's (the post-live row waits 12 s).
func TestRefreshVodURLWaitsOutTheFreshAd(t *testing.T) {
	const old, freshURL = "http://cdn.invalid/videoplayback?expire=1&id=o-A&itag=137&clen=1000",
		"http://cdn.invalid/videoplayback?expire=2&id=o-B&itag=137&clen=1000"
	type refreshed struct{ url, token string }
	runAs := func(t *testing.T, ctx context.Context, exempt bool) (*JobContext, *captureLogger, chan refreshed) {
		t.Helper()
		at := time.Now().Add(12 * time.Second)
		stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
			return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlaybackWaitExempt: exempt, PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
				Formats: []youtube.Format{{Itag: 137, URL: freshURL, ContentLength: "1000", Source: "tv_auth", AvailableAt: at}}}
		})
		job, logs := vodPotJob(t)
		job.Config.MaximumTimeout = 30
		job.YT = youtube.NewService(nil, &discardLogger{})
		served := youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "tv_auth"}
		out := make(chan refreshed, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("refreshVodURL panicked: %v", p)
				}
			}()
			u, tok := refreshVodURL(ctx, job, "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
				served, stubCipherSolver{}, nil, nil, "vodpot", "VOD video")
			out <- refreshed{u, tok}
		}()
		return job, logs, out
	}
	run := func(t *testing.T, ctx context.Context) (*JobContext, *captureLogger, chan refreshed) {
		t.Helper()
		return runAs(t, ctx, false)
	}

	t.Run("the URL waits for its response's ad", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			if got := credentialRefreshTimeoutFor(&JobConfig{MaximumTimeout: 30}); got >= 12*time.Second {
				t.Fatalf("refresh budget at the floor = %v; the test needs it shorter than the 12 s ad", got)
			}
			_, logs, out := run(t, context.Background())
			time.Sleep(12*time.Second - time.Millisecond)
			synctest.Wait()
			select {
			case got := <-out:
				t.Fatalf("refreshVodURL returned %q before its response's 12 s ad was over", got.url)
			default:
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			select {
			case got := <-out:
				if got.url != freshURL {
					t.Errorf("refreshVodURL = %q after the ad, want the fresh URL", got.url)
				}
			default:
				t.Fatal("refreshVodURL still waiting once the ad was over")
			}
			line := logLine(logs, "credential refresh: waiting out the fresh player response's pre-roll ad")
			if line == nil || line["seconds"] != 12.0 || line["client"] != "tv_auth" || line["tag"] != "VOD video" {
				t.Errorf("refresh wait line = %v, want seconds=12 client=tv_auth tag=VOD video", line)
			}
		})
	})

	t.Run("a cancel during the wait keeps the current URL", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, _, out := run(t, ctx)
			time.Sleep(2 * time.Second)
			synctest.Wait()
			cancel()
			synctest.Wait()
			select {
			case got := <-out:
				if got.url != "" {
					t.Errorf("refreshVodURL = %q after a cancel, want the current URL kept", got.url)
				}
			default:
				t.Fatal("refreshVodURL still waiting after the cancel")
			}
		})
	})

	t.Run("a post-live response's URL comes back at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, logs, out := runAs(t, context.Background(), true)
			synctest.Wait()
			select {
			case got := <-out:
				if got.url != freshURL {
					t.Errorf("refreshVodURL = %q, want the fresh URL", got.url)
				}
			default:
				t.Fatal("refreshVodURL waiting on a post-live response's ad, which upstream never stamps")
			}
			if line := logLine(logs, "credential refresh: waiting out"); line != nil {
				t.Errorf("logged a refresh wait it did not make: %v", line)
			}
		})
	})
}

// TestRefreshGvsCredentialsWaitsWithinItsBudget: the segmented refresh
// installs a fresh URL only once its response's ad is over, inside its own
// bound — that bound protects a live head's stall budget, and a live or
// post-live response (all it normally sees) never waits, as upstream stamps
// neither. An ad that would outlast the bound is not waited for at all: the
// current URL stays, the fresh token is handed back, and a Warn says why.
//
// Mutants (each run): the wait dropped (the VOD row's URL comes back at 0);
// the deadline check dropped (the long-ad row sleeps to the deadline and
// returns at 10 s); the gate read off the setup info instead of the fresh
// one (the setup is post-live, so the VOD rows never wait).
func TestRefreshGvsCredentialsWaitsWithinItsBudget(t *testing.T) {
	const freshURL = "https://fresh.invalid/videoplayback?id=2&n=abc123"
	for _, tc := range []struct {
		name    string
		status  youtube.StreamStatus
		exempt  bool
		ad      time.Duration
		wantAt  time.Duration // when it returns
		wantURL bool
	}{
		{"a processed VOD's fresh URL waits for its ad", youtube.StreamVOD, false, 6 * time.Second, 6 * time.Second, true},
		{"an ad longer than the budget keeps the current URL", youtube.StreamVOD, false, 20 * time.Second, 0, false},
		{"a live response never waits", youtube.StreamLive, true, 6 * time.Second, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				orig := refreshVideoInfo
				refreshVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
					return &youtube.VideoInfo{
						StreamStatus:       tc.status,
						PlaybackWaitExempt: tc.exempt,
						PlayerURL:          "https://www.youtube.com/s/player/deadbeef/player.js",
						Formats:            []youtube.Format{{Itag: 140, URL: freshURL, Source: "web_creator", AvailableAt: start.Add(tc.ad)}},
					}, nil
				}
				t.Cleanup(func() { refreshVideoInfo = orig })
				fake := &fakePotProvider{generate: func(context.Context, string, bool) (string, error) { return "fresh-token", nil }}
				solver := &orderedSolver{onN: func(context.Context, string, string) (string, error) { return "decrypted-n", nil }}
				logs := &captureLogger{}
				job := &JobContext{
					Job:    &database.Job{ID: "test-job", VideoID: "vid1"},
					YT:     youtube.NewService(nil, &discardLogger{}),
					Config: &JobConfig{MaximumTimeout: 30}, // a 10 s budget
					Logger: logs,
				}
				videoInfo := &youtube.VideoInfo{
					StreamStatus:       youtube.StreamPostLive,
					PlaybackWaitExempt: true,
					PlayerURL:          "https://www.youtube.com/s/player/deadbeef/player.js",
					Formats:            []youtube.Format{{Itag: 140, URL: "https://setup.invalid/videoplayback?id=1&n=abc123", Source: "web_creator"}},
				}

				var gotURL, gotToken string
				var returnedAt time.Duration
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer func() {
						if p := recover(); p != nil {
							t.Errorf("refreshGvsCredentials panicked: %v", p)
						}
					}()
					gotURL, gotToken = refreshGvsCredentials(context.Background(), job, videoInfo, 140, solver, nil, fake, "vd-123", "manifestless DASH audio")
					returnedAt = time.Since(start)
				}()
				<-done

				if returnedAt != tc.wantAt {
					t.Errorf("returned after %v, want %v", returnedAt, tc.wantAt)
				}
				if gotToken != "fresh-token" {
					t.Errorf("token = %q, want the fresh token whatever the ad", gotToken)
				}
				if installed := strings.Contains(gotURL, "fresh.invalid"); installed != tc.wantURL {
					t.Errorf("baseURL = %q, want installed=%v", gotURL, tc.wantURL)
				}
				warn := logLine(logs, "credential refresh: the fresh player response's pre-roll ad outlasts the refresh budget")
				if (warn != nil) != (!tc.wantURL) {
					t.Errorf("budget Warn = %v, want one only when the URL is kept", warn)
				}
			})
		})
	}
}
