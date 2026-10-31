package youtube

import (
	"context"
	"net/http"
	"testing"
)

// tvPostLiveBody is what TV answers for a broadcast in its post-live DVR
// window, the shape this file is about: no microformat (TV sends none),
// isLiveContent and isPostLiveDvr in videoDetails, split adaptive formats
// carrying targetDurationSec and no contentLength — segment-addressed, &sq=N —
// and no dashManifestUrl anywhere.
const tvPostLiveBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLiveContent": true, "isPostLiveDvr": true, "lengthSeconds": "7260"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 137, "url": "https://tv/v", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080, "targetDurationSec": 2},
		{"itag": 140, "url": "https://tv/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "targetDurationSec": 2}
	]}
}`

// tvFinishedBody is the same broadcast once nobody states isPostLiveDvr: the
// VOD the extraction has always called it.
const tvFinishedBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLiveContent": true, "lengthSeconds": "7260"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 137, "url": "https://tv/v", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080, "contentLength": "1048576000"},
		{"itag": 140, "url": "https://tv/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "contentLength": "117000000"}
	]}
}`

// TestTVPostLiveAnswerIsPostLive is G6: upstream's first live_status rule is
// videoDetails.isPostLiveDvr (_video.py:3868-3874), read over every player
// response, and TV — the status authority of both cascades — sends no
// microformat, the only place classifyStream reads post-live from. The
// extraction came back StreamVOD, which the worker sent to the whole-file
// VodStrategy (no manifest) and lost as a "bare media fragment". It is
// StreamPostLive now, IsPostLiveDVR set, its segmented pool intact for the
// manifest-free path; one response's parse — the monitors' and probes' — is
// classifyStream's as before, and the flag stated false first stands, as
// upstream's get_first has it.
//
// Mutants (each run): finishExtraction not promoting (the post-live row of
// both cascades reads vod); a later statement of the flag overriding an
// earlier one (the watch page's false row reads post_live); classifyStream
// reading isPostLiveDvr itself (the single-parse check, and the false row,
// whose TV winner then calls itself post-live).
func TestTVPostLiveAnswerIsPostLive(t *testing.T) {
	cascades := map[string]func(*PlayerAPI) (*VideoInfo, error){
		"public": func(p *PlayerAPI) (*VideoInfo, error) {
			return p.GetVideoInfoPublic(context.Background(), "test1234567")
		},
		"authenticated": func(p *PlayerAPI) (*VideoInfo, error) {
			return p.GetVideoInfoAuthenticated(context.Background(), "test1234567")
		},
	}
	for _, tc := range []struct {
		name         string
		tv           string
		watchPage    string // "" for a page with no player response
		wantStatus   StreamStatus
		wantPostLive bool
	}{
		{"TV's post-live DVR answer", tvPostLiveBody, "", StreamPostLive, true},
		{"a finished broadcast nobody calls post-live", tvFinishedBody, "", StreamVOD, false},
		{"the watch page's earlier false stands", tvPostLiveBody,
			`{"playabilityStatus": {"status": "OK"}, "videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLiveContent": true, "isPostLiveDvr": false}}`,
			StreamVOD, false},
	} {
		for cascade, run := range cascades {
			t.Run(tc.name+"/"+cascade, func(t *testing.T) {
				if tc.watchPage == "" {
					stubWatchPage(t)
				} else {
					stubWatchPageWithPlayerResponse(t, tc.watchPage)
				}
				tr := &clientKeyedTransport{responses: map[string]struct {
					status int
					body   string
				}{"7": {http.StatusOK, tc.tv}}}
				orig := apiClient
				apiClient = &http.Client{Transport: tr}
				t.Cleanup(func() { apiClient = orig })

				info, err := run(newRetryTestAPI())
				if err != nil {
					t.Fatalf("extraction: %v", err)
				}
				if info.StreamStatus != tc.wantStatus || info.IsPostLiveDVR != tc.wantPostLive {
					t.Errorf("status = %q, IsPostLiveDVR = %v; want %q, %v", info.StreamStatus, info.IsPostLiveDVR, tc.wantStatus, tc.wantPostLive)
				}
				if tc.wantPostLive && (!HasSplitAdaptiveFormats(info.Formats) || info.DashManifestURL != "") {
					t.Errorf("the post-live pool lost its shape: split %v, manifest %q", HasSplitAdaptiveFormats(info.Formats), info.DashManifestURL)
				}
			})
		}
	}

	t.Run("one response's parse is classifyStream's", func(t *testing.T) {
		p := &PlayerAPI{logger: noopLogger{}}
		pr := liveStatusResponse(t, map[string]any{"isLiveContent": true, "isPostLiveDvr": true}, nil)
		info, err := p.parsePlayerResponse(context.Background(), pr, "", nil, "test1234567")
		if err != nil {
			t.Fatalf("parsePlayerResponse: %v", err)
		}
		if info.StreamStatus != StreamVOD || info.IsPostLiveDVR {
			t.Errorf("single parse = %q (IsPostLiveDVR %v), want vod — the monitors' probes read one response, and the promotion is the cascade's", info.StreamStatus, info.IsPostLiveDVR)
		}
	})
}

// TestParseFormatsReadsTheOTFType: a format's `type` of
// FORMAT_STREAM_TYPE_OTF is the one value upstream reads
// (_video.py:3503-3507), and it is what marks a stream the whole-file path
// cannot fetch.
//
// Mutants: the field never read (the OTF row); any stated type read as OTF
// (the unspecified row).
func TestParseFormatsReadsTheOTFType(t *testing.T) {
	p := &PlayerAPI{logger: noopLogger{}}
	got, _ := p.parseFormats(context.Background(), map[string]any{
		"adaptiveFormats": []any{
			map[string]any{"itag": float64(137), "url": "https://x/otf", "mimeType": `video/mp4; codecs="avc1.640028"`, "width": float64(1920), "height": float64(1080), "type": "FORMAT_STREAM_TYPE_OTF"},
			map[string]any{"itag": float64(136), "url": "https://x/unspecified", "mimeType": `video/mp4; codecs="avc1.4d401f"`, "width": float64(1280), "height": float64(720), "type": "FORMAT_STREAM_TYPE_UNSPECIFIED"},
			map[string]any{"itag": float64(140), "url": "https://x/plain", "mimeType": `audio/mp4; codecs="mp4a.40.2"`},
		},
	})
	if len(got) != 3 {
		t.Fatalf("parsed %d formats, want 3", len(got))
	}
	for _, f := range got {
		if want := f.Itag == 137; f.IsOTF != want {
			t.Errorf("itag %d IsOTF = %v, want %v", f.Itag, f.IsOTF, want)
		}
	}
}

// TestFormatSegmented pins the whole-file path's test for a format it cannot
// fetch: no contentLength, and OTF or live adaptive (targetDurationSec) —
// yt-dlp's two https skips (_video.py:3492-3507). A stated length is a file.
//
// Mutants: the contentLength condition dropped (the two "with a length"
// rows); either kind dropped (its row).
func TestFormatSegmented(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Format
		want bool
	}{
		{"OTF", Format{IsOTF: true}, true},
		{"live adaptive", Format{TargetDurationSec: 5}, true},
		{"OTF with a length", Format{IsOTF: true, ContentLength: "1000"}, false},
		{"live adaptive with a length", Format{TargetDurationSec: 5, ContentLength: "1000"}, false},
		{"open-ended, neither kind", Format{}, false},
	} {
		if got := tc.f.Segmented(); got != tc.want {
			t.Errorf("%s: Segmented = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestHasSplitAdaptiveFormatsTakesOnlyAudioLessVideo: the video half of a
// manifest-free pair is a video-only stream. A muxed format listed without a
// contentLength — itag 18, whose mimeType names a video and an audio codec,
// as yt-dlp's parse_codecs reads it — is the progressive file, and counting
// it handed a whole-file URL to the &sq loop.
//
// Mutant: the video test back to Width > 0 (the muxed row reads split).
func TestHasSplitAdaptiveFormatsTakesOnlyAudioLessVideo(t *testing.T) {
	w, h := 640, 360
	audio := Format{Itag: 140, URL: "https://x/a", MimeType: `audio/mp4; codecs="mp4a.40.2"`}
	for _, tc := range []struct {
		name  string
		video Format
		want  bool
	}{
		{"a muxed itag 18 is no video half", Format{Itag: 18, URL: "https://x/m", MimeType: `video/mp4; codecs="avc1.42001E, mp4a.40.2"`, Width: &w, Height: &h}, false},
		{"a video-only stream is", Format{Itag: 134, URL: "https://x/v", MimeType: `video/mp4; codecs="avc1.4d401e"`, Width: &w, Height: &h}, true},
		{"a mimeType naming no codecs reads video-only", Format{Itag: 134, URL: "https://x/v", MimeType: "video/mp4", Width: &w, Height: &h}, true},
	} {
		if got := HasSplitAdaptiveFormats([]Format{tc.video, audio}); got != tc.want {
			t.Errorf("%s: HasSplitAdaptiveFormats = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDedupKeepsTheWholeFileCopyOfASegmentedStream is G24's dedup half:
// yt-dlp skips an OTF format always, and a live adaptive one unless the
// stream needs live processing, BEFORE its first-seen dedup
// (_video.py:3492-3507), so another client's whole-file copy of the same
// stream is the one it keeps. Moombox ranked by AuthLevel alone, and a stream
// tv listed segmented and the watch page as a file went to tv: the
// whole-file path fetched one fragment of it. The same rule picks each
// class's shadow, so a missing_pot swap lands on a file too.
//
// Mutants (each run): the rule dropped (the outside-live-processing, OTF and
// no-cascade rows keep the tv copy); the rule applied to live adaptive
// copies whatever the verdict (the post-live row loses tv's); OTF kept under
// live processing like a live adaptive copy (the post-live OTF row); the
// class shadows ranked by AuthLevel alone (the shadow row's shadow is tv's).
func TestDedupKeepsTheWholeFileCopyOfASegmentedStream(t *testing.T) {
	w, h := 1920, 1080
	video := func(source string, level int, mod func(*Format)) Format {
		f := Format{Itag: 137, URL: "https://" + source + "/v", MimeType: `video/mp4; codecs="avc1.640028"`, Width: &w, Height: &h}
		mod(&f)
		f.Source, f.AuthLevel = source, &level
		return f
	}
	liveAdaptive := func(f *Format) { f.TargetDurationSec = 2 }
	otf := func(f *Format) { f.IsOTF = true }
	file := func(f *Format) { f.ContentLength = "1048576000" }

	postLive := func() context.Context {
		ctx := withExtractionState(context.Background())
		yes := true
		extractionStateFrom(ctx).notePlaybackLive(playbackLiveFlags{postLiveDvr: &yes})
		return ctx
	}
	vod := func() context.Context { return withExtractionState(context.Background()) }

	for _, tc := range []struct {
		name       string
		ctx        context.Context
		pool       []Format
		wantWinner string
		wantShadow string // the winner's shadow's source, "" for none checked
	}{
		{"a live adaptive copy loses to a file outside live processing", vod(),
			[]Format{video("tv_public", AuthLevelTVPublic, liveAdaptive), video("watch_page", AuthLevelWatchPagePublic, file)},
			"watch_page", ""},
		{"post-live keeps the live adaptive copy first-seen", postLive(),
			[]Format{video("tv_public", AuthLevelTVPublic, liveAdaptive), video("watch_page", AuthLevelWatchPagePublic, file)},
			"tv_public", ""},
		{"an OTF copy loses to a file even post-live", postLive(),
			[]Format{video("tv_public", AuthLevelTVPublic, otf), video("watch_page", AuthLevelWatchPagePublic, file)},
			"watch_page", ""},
		{"outside a cascade the file wins", context.Background(),
			[]Format{video("tv_public", AuthLevelTVPublic, liveAdaptive), video("watch_page", AuthLevelWatchPagePublic, file)},
			"watch_page", ""},
		{"two segmented copies are ranked by AuthLevel", vod(),
			[]Format{video("watch_page", AuthLevelWatchPagePublic, otf), video("tv_public", AuthLevelTVPublic, otf)},
			"tv_public", ""},
		{"a class's shadow is its file too", vod(),
			[]Format{video("tv_public", AuthLevelTVPublic, liveAdaptive), video("watch_page", AuthLevelWatchPagePublic, file), video("android_vr", AuthLevelAndroidVR, file)},
			"watch_page", "android_vr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deduplicateFormats(tc.ctx, tc.pool)
			if len(got) != 1 {
				t.Fatalf("dedup kept %d copies of one stream, want 1", len(got))
			}
			if got[0].Source != tc.wantWinner {
				t.Errorf("winner = %s, want %s", got[0].Source, tc.wantWinner)
			}
			if tc.wantShadow != "" {
				if got[0].TokenFreeAlternate == nil || got[0].TokenFreeAlternate.Source != tc.wantShadow {
					t.Errorf("shadow = %+v, want %s's copy", got[0].TokenFreeAlternate, tc.wantShadow)
				}
			}
		})
	}
}
