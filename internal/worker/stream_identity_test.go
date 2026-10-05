package worker

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The refresh guard compared video by width/height/fps alone and never looked
// at audio, so a refresh that came back with a different codec at the same
// size (avc1 299 and vp9 303 are both 1080p60), or that lost the opus itag and
// fell back to AAC, was judged a match and appended new-codec fragments under
// the old init segment. The DASH strategies record their itags now, and a
// known itag that differs is a mismatch; an unknown one (HLS) decides nothing.
//
// Mutant: dropping streamIdentityChanged from refreshFormatMatches, or either
// half of it.
func TestRefreshGuardComparesItags(t *testing.T) {
	base := func() *DownloadResult {
		return &DownloadResult{HasVideo: true, HasAudio: true, VideoWidth: 1920, VideoHeight: 1080, VideoFps: 60, VideoItag: 299, AudioItag: 251}
	}
	if !refreshFormatMatches(base(), base()) {
		t.Fatal("identical renditions must match")
	}
	codec := base()
	codec.VideoItag = 303
	if refreshFormatMatches(base(), codec) {
		t.Error("a same-size video codec swap (299 → 303) was judged a match")
	}
	audio := base()
	audio.AudioItag = 140
	if refreshFormatMatches(base(), audio) {
		t.Error("an audio swap (opus 251 → aac 140) was judged a match")
	}
	unknown := base()
	unknown.VideoItag, unknown.AudioItag = 0, 0
	if !refreshFormatMatches(base(), unknown) {
		t.Error("an unknown itag (HLS) must not be read as a change")
	}
}

// Both DASH strategies record the itags they chose.
//
// Mutant: dropping either VideoItag/AudioItag assignment from either strategy.
func TestDashStrategiesRecordTheirItags(t *testing.T) {
	fakeVodMint(t, "tok", nil)
	job, _ := vodPotJob(t)
	res, err := DownloadManifestlessDash(context.Background(), job, manifestlessPotInfo("visionos", "visionos"), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadManifestlessDash: %v", err)
	}
	if res.VideoItag != 299 || res.AudioItag != 140 {
		t.Errorf("manifestless recorded video=%d audio=%d, want 299/140", res.VideoItag, res.AudioItag)
	}

	srv := newManifestServer(t, potTestMPD)
	job, _ = vodPotJob(t)
	res, err = DownloadDash(context.Background(), job, &youtube.VideoInfo{
		StreamStatus: youtube.StreamLive, DashManifestURL: srv.URL + "/api/manifest/dash/id/abc", DashManifestSource: "visionos",
	}, nil, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadDash: %v", err)
	}
	if res.VideoItag != 299 || res.AudioItag != 140 {
		t.Errorf("DASH recorded video=%d audio=%d, want 299/140", res.VideoItag, res.AudioItag)
	}
}

// The live loop treats a changed itag as it treats a changed quality: the
// quality-change branch's same-quality continue and the stall refresh both
// split on it, because a refreshed downloader continues the current part's
// files. Pinned on runLiveStreamDownload's source, as the stall split is
// (TestStillLiveRefreshSplitsOnAChangedQuality).
//
// Mutant: dropping streamIdentityChanged from either condition.
func TestLiveLoopSplitsOnAChangedItag(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *DownloadOrchestrator) runLiveStreamDownload(")
	end := strings.Index(body, "\nfunc qualityChangeInfo(")
	if start < 0 || end < start {
		t.Fatal("runLiveStreamDownload not found")
	}
	fn := body[start:end]
	for _, want := range []string{
		"if !newQuality.Changed(currentQuality) && !streamIdentityChanged(result, refreshResult) {",
		"newQuality.Changed(currentQuality) || streamIdentityChanged(result, refreshResult) {",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("runLiveStreamDownload lacks %q", want)
		}
	}
}
