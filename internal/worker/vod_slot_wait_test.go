package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// vodInfoAt is a finished VOD whose one video-only format is served at base
// under the given expire= stamp.
func vodInfoAt(base string, expire int64, size int) *youtube.VideoInfo {
	w, h, fps := 1280, 720, 30
	return &youtube.VideoInfo{
		PlayerURL:    "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
		StreamStatus: youtube.StreamVOD,
		Formats: []youtube.Format{
			{Itag: 136, URL: base + "/videoplayback?expire=" + strconv.FormatInt(expire, 10) + "&id=o-AKvod&itag=136", MimeType: `video/mp4; codecs="avc1.4d401f"`, Bitrate: 2_000_000, Width: &w, Height: &h, Fps: &fps, ContentLength: strconv.Itoa(size), Source: "android_vr"},
		},
	}
}

// TestStaleVodExtractionIsRefreshedAfterSlotWait is V3 through processJob: a
// VOD's format URLs were extracted before the slot waits and had expired by
// the time the download started (googlevideo answers 403). The download must
// re-extract and use the fresh URLs, and the job must finish.
//
// Mutant: drop the refreshStaleVodInfo call from processJob — the download
// runs on the expired URL, gets 403, and the job ends in Error.
func TestStaleVodExtractionIsRefreshedAfterSlotWait(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	w.orchestrator.SetFfmpegPath(ffmpegPath)
	w.orchestrator.routedCipher = stubCipherSolver{}
	_, outputDir := muxFixtureJob(t, w, db, "j-stale")
	db.UpdateJobFields("j-stale", map[string]any{"status": database.StatusUpcoming})

	full := filepath.Join(t.TempDir(), "full.mp4")
	writeMuxFixture(t, ffmpegPath, full, 3)
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	fresh := serveWholeFile(t, body)
	expired := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusForbidden) // expire= has passed
	}))
	t.Cleanup(expired.Close)

	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt(expired.URL, 1, len(body))}, nil
	}
	var refreshed atomic.Int32
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		refreshed.Add(1)
		return vodInfoAt(fresh.URL, time.Now().Add(6*time.Hour).Unix(), len(body)), nil
	}

	w.processJob(context.Background(), "j-stale")

	row, _ := db.GetJob("j-stale")
	if row.Status != database.StatusFinished {
		t.Fatalf("status = %s (%q), want Finished on the re-extracted URLs", row.Status, row.Error)
	}
	if refreshed.Load() != 1 {
		t.Errorf("re-extractions = %d, want 1", refreshed.Load())
	}
	if len(mp4sIn(t, outputDir)) != 1 {
		t.Errorf("outputs = %v", mp4sIn(t, outputDir))
	}
}

// TestStaleVodRefreshFailureKeepsUnexpiredURLs is W20-23 through processJob: a
// VOD whose URLs lapse within the hour is re-extracted once its slots are
// held, and that re-extraction fails on a blip. The URLs it already holds
// still serve the file, so the download goes ahead on them and the job
// finishes — it used to end in Error over a fetch it did not need.
//
// Mutant: return the re-extraction's error unconditionally from
// refreshStaleVodInfo — the job ends in Error.
func TestStaleVodRefreshFailureKeepsUnexpiredURLs(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	w.orchestrator.SetFfmpegPath(ffmpegPath)
	w.orchestrator.routedCipher = stubCipherSolver{}
	_, outputDir := muxFixtureJob(t, w, db, "j-valid")
	db.UpdateJobFields("j-valid", map[string]any{"status": database.StatusUpcoming})

	full := filepath.Join(t.TempDir(), "full.mp4")
	writeMuxFixture(t, ffmpegPath, full, 3)
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	srv := serveWholeFile(t, body)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		// Good for 55 more minutes: inside vodInfoMaxAge, so stale.
		return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt(srv.URL, time.Now().Add(55*time.Minute).Unix(), len(body))}, nil
	}
	var refreshed atomic.Int32
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		refreshed.Add(1)
		return nil, dialRefused
	}

	w.processJob(context.Background(), "j-valid")

	row, _ := db.GetJob("j-valid")
	if row.Status != database.StatusFinished {
		t.Fatalf("status = %s (%q), want Finished on the extraction's own URLs", row.Status, row.Error)
	}
	if refreshed.Load() != 1 {
		t.Errorf("re-extractions = %d, want 1", refreshed.Load())
	}
	if len(mp4sIn(t, outputDir)) != 1 {
		t.Errorf("outputs = %v", mp4sIn(t, outputDir))
	}
}

// TestStaleVodRefreshFailureFallsBackOnlyWhileTheURLsLive pins when a failed
// re-extraction leaves the stale extraction in place: a transient failure,
// with every format URL still unexpired. Anything else returns the error as
// before — URLs already expired or with no expiry to read, a verdict on the
// video, a definitive refusal, a cancel.
//
// Mutants: drop the `!ok` in vodURLsUnexpired — the no-expiry row keeps its
// extraction; `!exp.After(now)` → `false` — the expired row does; drop
// `!isVodRefreshVerdict(err)` — the verdict row does; drop the classNetwork
// test — the 404 row does; drop `ctx.Err() == nil` — the cancelled row does;
// make vodURLsUnexpired answer `true` for no URL at all — the URL-less row
// does.
func TestStaleVodRefreshFailureFallsBackOnlyWhileTheURLsLive(t *testing.T) {
	now := time.Now()
	future := now.Add(4 * time.Hour).Unix()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name     string
		info     *youtube.VideoInfo
		err      error
		ctx      context.Context
		wantKept bool
	}{
		{"a blip, URLs good for hours", vodInfoAt("https://r1.googlevideo.com", future, 1), dialRefused, context.Background(), true},
		{"a 429, URLs good for hours", vodInfoAt("https://r1.googlevideo.com", future, 1), errors.New("full fetch failed: ANDROID_VR API error: HTTP 429"), context.Background(), true},
		{"a blip, URLs expired", vodInfoAt("https://r1.googlevideo.com", 1, 1), dialRefused, context.Background(), false},
		{"a blip, no expiry to read", &youtube.VideoInfo{Formats: []youtube.Format{{Itag: 136, URL: "https://example.invalid/v.mp4"}}}, dialRefused, context.Background(), false},
		{"a blip, no URL at all", &youtube.VideoInfo{Formats: []youtube.Format{{Itag: 136}}}, dialRefused, context.Background(), false},
		{"a verdict on the video", vodInfoAt("https://r1.googlevideo.com", future, 1), &vodRefreshVerdict{err: errors.New("This video is private")}, context.Background(), false},
		{"a definitive refusal", vodInfoAt("https://r1.googlevideo.com", future, 1), errors.New("full fetch failed: web API error: HTTP 404"), context.Background(), false},
		{"a cancel", vodInfoAt("https://r1.googlevideo.com", future, 1), dialRefused, cancelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := testWorkerSetup(t)
			w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
				return nil, tc.err
			}
			result := &StreamProcessResult{IsVod: true, VideoInfo: tc.info}
			// Two hours old: stale by age whatever the URLs say.
			err := w.refreshStaleVodInfo(tc.ctx, &database.Job{ID: "j", Platform: "youtube"}, result, now.Add(-2*time.Hour))
			if tc.wantKept {
				if err != nil || result.VideoInfo != tc.info {
					t.Errorf("refreshStaleVodInfo = %v; want nil with the stale extraction kept", err)
				}
			} else if err == nil {
				t.Errorf("refreshStaleVodInfo = nil; want the re-extraction's error")
			}
		})
	}
}

// TestStaleVodRefreshVerdictIsNotRetried: a backlog VOD whose stale
// re-extraction hears that the video went members-only or private while it
// queued — or is no longer a finished stream — ends where that answer sends
// it, as one refused up front does: COOKIES? or Error, not back in Queued
// for a retry. The verdict reaches processJob as an error whose text
// classifyProbeErr cannot place, and so used to read as transient.
//
// Mutants: drop `isVodRefreshVerdict(err) ||` from
// requeueBacklogAfterTransientFailure — every row goes back to Queued, held;
// return the playability verdict unwrapped from judgeRefreshedVodInfo — the
// members-only and private rows do; return the status change unwrapped —
// the live row does.
func TestStaleVodRefreshVerdictIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *youtube.VideoInfo
		want database.JobStatus
	}{
		{"members-only, signed out", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityMembersOnly, PlayabilityReason: "Join this channel to get access to members-only content", SessionAuth: youtube.SessionAuthLoggedOut}, database.StatusCookies},
		{"private", &youtube.VideoInfo{PlayabilityError: youtube.PlayabilityPrivate, PlayabilityReason: "Private video"}, database.StatusError},
		{"live again", &youtube.VideoInfo{StreamStatus: youtube.StreamLive}, database.StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			backlogRetryJob(t, db, "verdict_vod", 1, true)
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				// expire=1: stale once the slots are held
				return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
			}
			w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
				return judgeRefreshedVodInfo(tc.info)
			}
			w.processJob(context.Background(), "verdict_vod")
			row, _ := db.GetJob("verdict_vod")
			if row.Status != tc.want {
				t.Errorf("status = %s (%q, held %v), want %s", row.Status, row.Error, w.scheduler.held("verdict_vod", time.Now()), tc.want)
			}
		})
	}
}

// TestFreshVodExtractionIsNotRefreshed: a download that starts on a fresh
// extraction spends no request on a second one.
//
// Mutant: make refreshStaleVodInfo ignore vodInfoStale — the fresh job
// re-extracts.
func TestFreshVodExtractionIsNotRefreshed(t *testing.T) {
	w, _ := testWorkerSetup(t)
	var refreshed atomic.Int32
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		refreshed.Add(1)
		return &youtube.VideoInfo{}, nil
	}
	result := &StreamProcessResult{IsVod: true, VideoInfo: vodInfoAt("http://x", time.Now().Add(6*time.Hour).Unix(), 1)}
	if err := w.refreshStaleVodInfo(context.Background(), &database.Job{ID: "j", Platform: "youtube"}, result, time.Now()); err != nil {
		t.Fatal(err)
	}
	if refreshed.Load() != 0 {
		t.Errorf("a fresh extraction was re-extracted")
	}
}

// TestVodInfoStale pins the staleness rule: the extraction's age, or the
// URL's own expiry (query or path form), whichever says so first.
//
// Mutants: drop the age rule — the hour-old extraction reads fresh; drop the
// expiry loop — the near-expiry URLs read fresh; drop the path-form fallback —
// the path-form URL reads fresh.
func TestVodInfoStale(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	far := strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)
	near := strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10)
	info := func(u string) *youtube.VideoInfo { return &youtube.VideoInfo{Formats: []youtube.Format{{URL: u}}} }
	for _, tc := range []struct {
		name string
		info *youtube.VideoInfo
		age  time.Duration
		want bool
	}{
		{"fresh, far expiry", info("https://r1.googlevideo.com/videoplayback?expire=" + far + "&itag=136"), time.Minute, false},
		{"an hour old", info("https://r1.googlevideo.com/videoplayback?expire=" + far + "&itag=136"), time.Hour, true},
		{"expires within the hour", info("https://r1.googlevideo.com/videoplayback?expire=" + near + "&itag=136"), time.Minute, true},
		{"path-form expiry within the hour", info("https://r1.googlevideo.com/videoplayback/expire/" + near + "/itag/136"), time.Minute, true},
		{"no expiry at all", info("https://example.invalid/v.mp4"), time.Minute, false},
	} {
		if got := vodInfoStale(tc.info, now.Add(-tc.age), now); got != tc.want {
			t.Errorf("%s: vodInfoStale = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestVodSlotWaitSaysSoAndClearsAfter pins the row a queueing VOD shows: the
// slot-wait progress line while it waits — never Downloading — and nothing
// once the wait is over. A VOD that finds a slot free writes no line.
//
// Mutants: drop the progress write — the waiting row says nothing; drop the
// deferred clear — the line outlives the wait.
func TestVodSlotWaitSaysSoAndClearsAfter(t *testing.T) {
	w, db := testWorkerSetup(t)
	w.queue = NewJobQueue(1)
	for _, id := range []string{"j-busy", "j-wait"} {
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusUpcoming}); err != nil {
			t.Fatal(err)
		}
	}
	if !w.acquireDownloadSlot(context.Background(), "j-busy", true) {
		t.Fatal("the free slot was not taken")
	}
	if row, _ := db.GetJob("j-busy"); row.Progress != "" {
		t.Errorf("a VOD that found a slot free wrote %q", row.Progress)
	}

	got := make(chan bool, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("acquireDownloadSlot panicked: %v", r)
			}
		}()
		got <- w.acquireDownloadSlot(context.Background(), "j-wait", true)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		row, _ := db.GetJob("j-wait")
		if row.Progress == vodSlotWaitProgress {
			if row.Status == database.StatusDownloading {
				t.Errorf("the queueing VOD reads Downloading")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queueing VOD's progress is %q, want %q", row.Progress, vodSlotWaitProgress)
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.queue.ReleaseDownloadSlot("j-busy")
	if !<-got {
		t.Fatal("the slot was not acquired once it freed")
	}
	if row, _ := db.GetJob("j-wait"); row.Progress != "" {
		t.Errorf("progress after the wait = %q, want it cleared", row.Progress)
	}
}
