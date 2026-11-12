package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// testClip is a ShareClipRenderStatus reading (twitch.TwitchClipInfo) modelled
// on yt-dlp's TwitchClipsIE test case FaintLightGullWholeWheat, its
// renditions served from base: a 720p60 and a 1080p30 landscape one and a
// taller portrait crop, each signed as GetClip signs them. It was cut from
// VOD 1234567890, 1h2m5s in.
func testClip(base string) *twitch.TwitchClipInfo {
	signed := func(name string) string { return base + "/" + name + "?sig=s1&token=t1" }
	offset := 3725
	return &twitch.TwitchClipInfo{
		VideoID: "1234567890", VideoOffsetSeconds: &offset,
		ID: "396245304", Slug: "FaintLightGullWholeWheat",
		Title:            "EA Play 2016 Live from the Novo Theatre",
		BroadcasterLogin: "ea", BroadcasterDisplayName: "EA",
		CuratorDisplayName: "stereotype_", CuratorLogin: "stereotype_",
		GameCategory: "Just Chatting", CreatedAt: "2016-06-12T21:36:33Z",
		DurationSeconds: 32, ThumbnailURL: base + "/preview.jpg",
		Qualities: []twitch.TwitchClipQuality{
			{URL: signed("clip-720.mp4"), Quality: "720", Height: 720, FrameRate: 60},
			{URL: signed("clip-1080.mp4"), Quality: "1080", Height: 1080, FrameRate: 30},
			{URL: signed("clip-portrait.mp4"), Quality: "1920", Height: 1920, FrameRate: 60, Portrait: true},
		},
	}
}

// addClipJob inserts the row a pasted clip URL creates: the clip kind's ID,
// Upcoming, manually added.
func addClipJob(t *testing.T, db *database.Database, outDir string) *database.Job {
	t.Helper()
	id := twitch.ClipJobID("FaintLightGullWholeWheat")
	job := &database.Job{ID: id, VideoID: id, URL: "https://clips.twitch.tv/FaintLightGullWholeWheat",
		Title: "Manual Add", ChannelName: "Manual", Platform: "twitch", Status: database.StatusUpcoming,
		ManuallyAdded: true, OutputDirectory: outDir}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	return job
}

// TestTwitchClipJobDownloadsTheBestRenditionAndFinalizes is owner decision
// D26 follow-up 1 end to end, through processJob: a "tw_clip_<slug>" row is
// read through ShareClipRenderStatus (never as a channel's live wait — the
// pasted clips.twitch.tv URL names no channel), gets the clip's title
// ("<channel> — <title>"), channel, thumbnail, length, category and published
// time, downloads the best rendition (1080p30 landscape, not the 720p60 nor
// the taller portrait crop) as a whole signed file, and finalizes into the
// output directory under the ordinary template, "[tw_clip_<slug>]" naming it.
//
// Mutants: the clip routing in processTwitch removed (the row is taken for a
// live channel "faintlightgullwholewheat" and never reaches the server); the
// worker's TwitchClip arm removed (no download: "stream processing" result
// with no variant goes to the YouTube path and fails); SelectClipQuality
// replaced by Qualities[0] (720 fetched); the metadata write dropped (the
// title stays "Manual Add").
func TestTwitchClipJobDownloadsTheBestRenditionAndFinalizes(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	fixture := filepath.Join(t.TempDir(), "clip.mp4")
	if out, err := exec.Command(ffmpegPath, "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=30",
		"-f", "lavfi", "-i", "sine=duration=1",
		"-c:v", "libx264", "-c:a", "aac", "-shortest", fixture).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, out)
	}
	media, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/preview.jpg" {
			http.Error(rw, "no thumbnail here", http.StatusNotFound)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Query().Get("sig") != "s1" || r.URL.Query().Get("token") != "t1" {
			http.Error(rw, "unsigned", http.StatusForbidden)
			return
		}
		http.ServeContent(rw, r, "clip.mp4", time.Time{}, strings.NewReader(string(media)))
	}))
	defer srv.Close()

	w, db := testWorkerSetup(t)
	w.streamProc.clipFn = func(_ context.Context, slug string) (*twitch.TwitchClipInfo, error) {
		if slug != "FaintLightGullWholeWheat" {
			return nil, fmt.Errorf("asked for slug %q", slug)
		}
		return testClip(srv.URL), nil
	}
	outDir := t.TempDir()
	job := addClipJob(t, db, outDir)

	w.processJob(context.Background(), job.ID)

	got, err := db.GetJob(job.ID)
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != database.StatusFinished {
		t.Fatalf("status = %s (error %q), want Finished", got.Status, got.Error)
	}
	if got.Title != "EA — EA Play 2016 Live from the Novo Theatre" || got.ChannelName != "EA" ||
		got.ThumbnailURL != srv.URL+"/preview.jpg" ||
		got.StreamStartTime != "2016-06-12T21:36:33Z" || got.TwitchCategory != "Just Chatting" ||
		!got.IsVod || got.TwitchQuality != "1080p30" {
		t.Errorf("row = title %q channel %q thumb %q start %q category %q isVod %v quality %q",
			got.Title, got.ChannelName, got.ThumbnailURL, got.StreamStartTime, got.TwitchCategory, got.IsVod, got.TwitchQuality)
	}
	if !strings.Contains(got.OutputFile, "[tw_clip_FaintLightGullWholeWheat].mp4") || !strings.HasPrefix(got.OutputFile, filepath.Clean(outDir)) {
		t.Errorf("output_file = %q, want the template's name under %s", got.OutputFile, outDir)
	}
	if _, err := os.Stat(got.OutputFile); err != nil {
		t.Errorf("archive: %v", err)
	}
	// The curator line, the title and the source VOD link are the row's
	// description (owner decisions FE-32 and FQ-3), and the finalize writes
	// it beside the archive as a VOD's is.
	const wantDesc = "Clipped by stereotype_\n\nEA Play 2016 Live from the Novo Theatre\n\nhttps://www.twitch.tv/videos/1234567890?t=1h2m5s"
	if got.Description != wantDesc {
		t.Errorf("description = %q, want %q", got.Description, wantDesc)
	}
	if b, err := os.ReadFile(got.DescriptionFile); err != nil || string(b) != wantDesc {
		t.Errorf(".description %q = %q, %v; want %q", got.DescriptionFile, b, err, wantDesc)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("the clip was never fetched")
	}
	for _, p := range paths {
		if p != "/clip-1080.mp4" {
			t.Errorf("fetched %s, want only the best rendition /clip-1080.mp4", p)
		}
	}
}

// TestTwitchClipPersistedQueryNotFoundFailsClearly is owner decision D26's
// rule at the job: a re-hashed ShareClipRenderStatus ends the clip job in
// Error with "Twitch changed its clip API (ShareClipRenderStatus); update
// Moombox" as its error, verbatim.
//
// Mutant: twitchClipErrorText formatting every error alike ("twitch clip
// error: Twitch changed…").
func TestTwitchClipPersistedQueryNotFoundFailsClearly(t *testing.T) {
	w, db := testWorkerSetup(t)
	w.streamProc.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) {
		return nil, &twitch.ClipAPIChangedError{Op: "ShareClipRenderStatus"}
	}
	job := addClipJob(t, db, t.TempDir())

	w.processJob(context.Background(), job.ID)

	got, _ := db.GetJob(job.ID)
	if got.Status != database.StatusError || got.Error != "Twitch changed its clip API (ShareClipRenderStatus); update Moombox" {
		t.Errorf("status %s error %q, want Error with the clear message", got.Status, got.Error)
	}
}

// TestTwitchClipGoneIsTheClipsVerdict: a clip Twitch no longer has is the
// job's verdict, in yt-dlp's words; a fetch that never reached Twitch is
// returned as an error (processJob waits an outage out and reads it again),
// as a VOD's is, never a verdict.
//
// Mutant: the twitchVodFetchError call dropped (the transport failure becomes
// the clip's verdict).
func TestTwitchClipGoneIsTheClipsVerdict(t *testing.T) {
	w, db := testWorkerSetup(t)
	job := addClipJob(t, db, t.TempDir())
	sp := w.streamProc

	sp.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) { return nil, twitch.ErrClipUnavailable }
	res, err := sp.processTwitch(context.Background(), job)
	if err != nil || res.ShouldDownload || res.Error != "twitch clip error: this clip is no longer available" {
		t.Errorf("gone clip = %+v, %v", res, err)
	}

	sp.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) {
		return nil, &netTimeoutErr{}
	}
	if res, err := sp.processTwitch(context.Background(), job); err == nil {
		t.Errorf("transport failure became a verdict: %+v", res)
	}
}

// netTimeoutErr is a transport failure isTransportFailure recognises (a
// net.Error timing out).
type netTimeoutErr struct{}

func (*netTimeoutErr) Error() string   { return "dial tcp: i/o timeout" }
func (*netTimeoutErr) Timeout() bool   { return true }
func (*netTimeoutErr) Temporary() bool { return true }

// TestTwitchClipDownloadIsOneWholeFile pins the clip's downloader: the
// engine's direct (whole-file) path on the chosen rendition's signed URL, into
// staging/video.mp4, with a resume identity naming the rendition — and its URL
// refresh answering a refused chunk with the SAME rendition's fresh URL from a
// second ShareClipRenderStatus read, and nothing once the clip no longer lists
// it.
//
// Mutants: IsDirectURL false (segmented); the refresh matching any rendition
// (the 720 URL returned); the refresh returning the stale URL; the refresh
// matching the quality label alone (the portrait crop's URL returned).
func TestTwitchClipDownloadIsOneWholeFile(t *testing.T) {
	var built []engine.DownloaderOptions
	prev := newSegmentDownloader
	newSegmentDownloader = func(opts engine.DownloaderOptions) *engine.SegmentDownloader {
		built = append(built, opts)
		return engine.NewSegmentDownloader(opts)
	}
	t.Cleanup(func() { newSegmentDownloader = prev })

	w, db := testWorkerSetup(t)
	job := addClipJob(t, db, t.TempDir())
	jobCtx := w.buildJobContext(job)
	first := testClip("https://cdn.example/old")
	reads := 0
	clip := &TwitchClipDownload{Slug: "FaintLightGullWholeWheat", Quality: first.Qualities[1],
		Reread: func(context.Context, string) (*twitch.TwitchClipInfo, error) {
			reads++
			if reads > 1 {
				gone := testClip("https://cdn.example/new")
				gone.Qualities = gone.Qualities[:1]
				return gone, nil
			}
			return testClip("https://cdn.example/new"), nil
		}}

	result := w.orchestrator.twitchClipDownloadResult(context.Background(), jobCtx, clip)
	if len(built) != 1 {
		t.Fatalf("downloaders built = %d, want 1", len(built))
	}
	opts := built[0]
	if !opts.IsDirectURL || opts.BaseURL != first.Qualities[1].URL ||
		opts.OutputFile != filepath.Join(jobCtx.StagingDir, "video.mp4") || result.VideoPath != opts.OutputFile ||
		opts.StreamID != "tw_clip_FaintLightGullWholeWheat/quality=1080/portrait=false" || result.AudioPath != "" {
		t.Errorf("downloader = %+v, result = %+v", opts, result)
	}
	if u, _ := opts.OnCredentialRefresh(); u != "https://cdn.example/new/clip-1080.mp4?sig=s1&token=t1" {
		t.Errorf("refresh = %q, want the same rendition's fresh URL", u)
	}
	if u, _ := opts.OnCredentialRefresh(); u != "" {
		t.Errorf("refresh with the rendition gone = %q, want nothing", u)
	}
	// A portrait rendition under the same label is another file: appending
	// it to the landscape partial would splice two renditions into one.
	clip.Reread = func(context.Context, string) (*twitch.TwitchClipInfo, error) {
		crop := testClip("https://cdn.example/crop")
		crop.Qualities = []twitch.TwitchClipQuality{{URL: "https://cdn.example/crop/portrait-1080.mp4?sig=s1&token=t1",
			Quality: "1080", Height: 1080, FrameRate: 30, Portrait: true}}
		return crop, nil
	}
	if u, _ := opts.OnCredentialRefresh(); u != "" {
		t.Errorf("refresh answered by a portrait rendition of the same label = %q, want nothing", u)
	}
}

// TestTwitchClipIsNoLiveRecovery: a clip's transport failure is not marked for
// the Twitch monitor's live recovery, which re-initialises a broadcast's job
// when it finds the broadcast live again — a clip is no broadcast.
//
// Mutant: the jobfilter.IsClipJob guard dropped from markTwitchUnreachable.
func TestTwitchClipIsNoLiveRecovery(t *testing.T) {
	w, _ := testWorkerSetup(t)
	job := &database.Job{ID: "tw_clip_X", VideoID: "tw_clip_X", Platform: "twitch"}
	err := w.markTwitchUnreachable(job, &netTimeoutErr{})
	var marked *twitchUnreachableError
	if errors.As(err, &marked) {
		t.Error("a clip's transport failure was marked for the live recovery")
	}
}

// TestClipsHostURLNamesNoChannel: a clip row's clips.twitch.tv/<slug> URL
// names no channel — its first path segment is the slug — so the login a
// Twitch row is tied to (the removal confirmation's count, TwitchJobLogin) is
// read from the row's channel name instead; a /<login>/clip/<slug> URL still
// names its channel.
//
// Mutant: the clips.twitch.tv exclusion dropped (the slug is read as the
// login).
func TestClipsHostURLNamesNoChannel(t *testing.T) {
	for _, tc := range []struct{ url, channel, want string }{
		{"https://clips.twitch.tv/FaintLightGullWholeWheat", "EA", "ea"},
		{"https://www.twitch.tv/ea/clip/FaintLightGullWholeWheat", "EA", "ea"},
	} {
		job := &database.Job{ID: "tw_clip_FaintLightGullWholeWheat", VideoID: "tw_clip_FaintLightGullWholeWheat",
			URL: tc.url, ChannelName: tc.channel, Platform: "twitch"}
		if got := TwitchJobLogin(job); got != tc.want {
			t.Errorf("TwitchJobLogin(%s) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// TestTwitchClipKeepsAnOutcomeThatLandedFirst: ExecuteTwitchClip's two status
// writes never overwrite an outcome. A Cancel that landed between the
// admission and the Downloading write stops the clip before it is fetched,
// the row still Cancelled; an outcome that landed while the file came down
// (here an Error, which the Cancel listener does not act on) stops it before
// Muxing, the row still in its outcome.
//
// Mutants: the Downloading write unconditional (UpdateJobFields: the
// Cancelled row turns Downloading and the clip is fetched); the Muxing guard
// dropped together with muxAndFinalize's own (FFmpeg runs past the Error).
// Either Muxing guard alone holds the row: muxAndFinalize writes Muxing
// unless an outcome stands too, so dropping ExecuteTwitchClip's alone is
// caught by the finalize's and changes nothing this test can see.
func TestTwitchClipKeepsAnOutcomeThatLandedFirst(t *testing.T) {
	for _, tc := range []struct {
		name        string
		before      bool // the outcome lands before ExecuteTwitchClip, else mid-download
		outcome     database.JobStatus
		wantFetched bool
	}{
		{"cancelled before the download", true, database.StatusCancelled, false},
		{"errored during the download", false, database.StatusError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := addClipJob(t, db, t.TempDir())
			var mu sync.Mutex
			fetched := 0
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				mu.Lock()
				fetched++
				mu.Unlock()
				if !tc.before {
					db.UpdateJobFields(job.ID, map[string]any{"status": tc.outcome})
				}
				http.ServeContent(rw, r, "clip.mp4", time.Time{}, strings.NewReader("not really an mp4"))
			}))
			defer srv.Close()
			if tc.before {
				db.UpdateJobFields(job.ID, map[string]any{"status": tc.outcome})
			}
			jobCtx := w.buildJobContext(job)
			clip := &TwitchClipDownload{Slug: "FaintLightGullWholeWheat", Quality: testClip(srv.URL).Qualities[1]}

			err := w.orchestrator.ExecuteTwitchClip(context.Background(), jobCtx, clip)
			if !errors.Is(err, ErrCancelled) {
				t.Errorf("ExecuteTwitchClip = %v, want ErrCancelled", err)
			}
			got, _ := db.GetJob(job.ID)
			if got.Status != tc.outcome {
				t.Errorf("status = %s, want %s kept", got.Status, tc.outcome)
			}
			mu.Lock()
			defer mu.Unlock()
			if (fetched > 0) != tc.wantFetched {
				t.Errorf("CDN requests = %d, want fetched %v", fetched, tc.wantFetched)
			}
		})
	}
}

// TestRecordPastedClipEntersOnlyClips: a pasted clip is entered in the
// processing history (the twitch_archive_clips sweep's dedupe), and every other
// manual add is left out of it, as it always was.
//
// Mutants: the jobfilter.IsClipJob check dropped (the live channel's row is
// entered); the AddToHistory call dropped (the clip is not).
func TestRecordPastedClipEntersOnlyClips(t *testing.T) {
	_, db := testWorkerSetup(t)
	clip := &database.Job{ID: "tw_clip_X", VideoID: "tw_clip_X", Platform: "twitch"}
	live := &database.Job{ID: "tw_manual_ea_1", VideoID: "tw_manual_ea_1", Platform: "twitch"}
	for _, j := range []*database.Job{clip, live} {
		if err := RecordPastedClip(db, j); err != nil {
			t.Fatal(err)
		}
	}
	if seen, _ := db.HasProcessed(clip.ID); !seen {
		t.Error("the pasted clip is not in the processing history")
	}
	if seen, _ := db.HasProcessed(live.ID); seen {
		t.Error("a live channel's manual add was entered in the processing history")
	}
}

// TestTwitchClipHonoursQualityPreferenceAndCap is owner decision FE-29 on the
// worker: processTwitchClip selects through twitch.SelectClipQuality with the
// row's quality_preference and the downloader's max_video_resolution read
// from the config, and records the pick as twitch_quality.
//
// Mutant: processTwitchClip back on BestClipQuality (the 1080p30 rendition in
// both rows); the cap not read from the config (the 1080p30 rendition under
// the 720 cap).
func TestTwitchClipHonoursQualityPreferenceAndCap(t *testing.T) {
	for _, c := range []struct {
		pref, want string
		cap        int
	}{
		{"720p60", "720p60", 0},
		{"best", "720p60", 720},
	} {
		w, db := testWorkerSetup(t)
		cfg := config.Defaults()
		cfg.Paths.StagingDirectory = t.TempDir()
		cfg.Downloader.MaxVideoResolution = c.cap
		w.SetConfigStore(config.NewStore(cfg, ""))
		job := addClipJob(t, db, t.TempDir())
		db.UpdateJobFields(job.ID, map[string]any{"quality_preference": c.pref})
		job.QualityPreference = c.pref
		w.streamProc.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) {
			return testClip("https://clips.example"), nil
		}
		res, err := w.streamProc.processTwitchClip(context.Background(), job, "FaintLightGullWholeWheat")
		if err != nil || res.TwitchClip == nil {
			t.Fatalf("pref %q cap %d: %+v, %v", c.pref, c.cap, res, err)
		}
		if got := twitchClipQualityLabel(&res.TwitchClip.Quality); got != c.want {
			t.Errorf("pref %q cap %d took %s, want %s", c.pref, c.cap, got, c.want)
		}
		if row, _ := db.GetJob(job.ID); row.TwitchQuality != c.want {
			t.Errorf("pref %q cap %d recorded twitch_quality %q, want %s", c.pref, c.cap, row.TwitchQuality, c.want)
		}
	}
}

// TestTwitchClipDescriptionNamesTheCurator is owner decisions FE-32 and
// FQ-3: a clip job's description reads, a blank line between each, "Clipped
// by <curator>" — the display name, else the login — then the clip's title,
// then a link to the source VOD at the clip's start when the answer names a
// VOD and an offset, then the clip's own description when there is one. A
// part the clip has nothing for is left out.
//
// Mutants: the login fallback dropped (the login-only row is empty); the
// clip's description dropped (its rows lack it); the curator line dropped
// (every row with a curator fails); the title dropped (the titled rows lack
// it); the link dropped, or written without the VOD check (the no-VOD and
// no-offset rows gain one); the offset's units miscomputed — minutes as
// offset/60 (the 1h2m5s link reads 1h62m5s), or a zero unit left out (the
// 0h0m5s link reads 5s); the order of the parts changed.
func TestTwitchClipDescriptionNamesTheCurator(t *testing.T) {
	at := func(s int) *int { return &s }
	for _, c := range []struct {
		clip twitch.TwitchClipInfo
		want string
	}{
		{twitch.TwitchClipInfo{CuratorDisplayName: "Stereotype_", CuratorLogin: "stereotype_"}, "Clipped by Stereotype_"},
		{twitch.TwitchClipInfo{CuratorLogin: "stereotype_"}, "Clipped by stereotype_"},
		{twitch.TwitchClipInfo{CuratorDisplayName: "Stereotype_", Description: "The play of the year."}, "Clipped by Stereotype_\n\nThe play of the year."},
		{twitch.TwitchClipInfo{Description: "  Only this.  "}, "Only this."},
		{twitch.TwitchClipInfo{}, ""},
		// With a source VOD and an offset into it.
		{twitch.TwitchClipInfo{CuratorDisplayName: "okSTFUdude", Title: " MISSILE ", VideoID: "2919684247", VideoOffsetSeconds: at(8973), Description: "What a shot."},
			"Clipped by okSTFUdude\n\nMISSILE\n\nhttps://www.twitch.tv/videos/2919684247?t=2h29m33s\n\nWhat a shot."},
		{twitch.TwitchClipInfo{CuratorDisplayName: "a", Title: "t", VideoID: "1", VideoOffsetSeconds: at(3725)},
			"Clipped by a\n\nt\n\nhttps://www.twitch.tv/videos/1?t=1h2m5s"},
		{twitch.TwitchClipInfo{Title: "t", VideoID: "1", VideoOffsetSeconds: at(5)}, "t\n\nhttps://www.twitch.tv/videos/1?t=0h0m5s"},
		{twitch.TwitchClipInfo{Title: "t", VideoID: "1", VideoOffsetSeconds: at(0)}, "t\n\nhttps://www.twitch.tv/videos/1?t=0h0m0s"},
		// Without one: no VOD (deleted, never saved), no offset, an offset
		// no VOD has, an ID no VOD has.
		{twitch.TwitchClipInfo{CuratorDisplayName: "okSTFUdude", Title: "A day in the life of xQc", VideoOffsetSeconds: at(10)},
			"Clipped by okSTFUdude\n\nA day in the life of xQc"},
		{twitch.TwitchClipInfo{Title: "t", VideoID: "1"}, "t"},
		{twitch.TwitchClipInfo{Title: "t", VideoID: "1", VideoOffsetSeconds: at(-1)}, "t"},
		{twitch.TwitchClipInfo{Title: "t", VideoID: "v1/../x", VideoOffsetSeconds: at(1)}, "t"},
	} {
		if got := twitchClipDescription(&c.clip); got != c.want {
			t.Errorf("twitchClipDescription(%+v) = %q, want %q", c.clip, got, c.want)
		}
	}
}
