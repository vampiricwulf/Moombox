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
