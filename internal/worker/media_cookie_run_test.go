package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A run holds one media-cookie state (owner ruling KA-6, runRecord): every
// setup of either DASH strategy hands its video and audio downloader the
// same one, so the downloaders a mid-stream refresh or a part split builds
// (refreshDownload runs the setup again) do not start with the cookies
// withheld, pay one more refused request and log the Warn again. The state
// was a local of each setup.
//
// Mutant: either strategy building its own state again
// (&engine.MediaCookieState{}): its row's downloaders differ from the first
// setup's.
func TestDashSetupsOfOneRunShareTheMediaCookieState(t *testing.T) {
	fakeVodMint(t, "tok", nil)
	job, _ := vodPotJob(t)
	job.Run = &runRecord{}
	srv := newManifestServer(t, potTestMPD)
	dash := func() (*DownloadResult, error) {
		return DownloadDash(context.Background(), job, &youtube.VideoInfo{
			StreamStatus: youtube.StreamLive, DashManifestURL: srv.URL + "/api/manifest/dash/id/abc", DashManifestSource: "visionos",
		}, nil, nil, &bgutils.PotProvider{}, nil)
	}
	manifestless := func() (*DownloadResult, error) {
		return DownloadManifestlessDash(context.Background(), job, manifestlessPotInfo("visionos", "visionos"), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
	}

	first, err := dash()
	if err != nil {
		t.Fatalf("DownloadDash: %v", err)
	}
	want := first.VideoDownloader.MediaCookies()
	if want == nil || first.AudioDownloader.MediaCookies() != want {
		t.Fatalf("the first setup's video and audio downloaders do not share one state")
	}
	for _, c := range []struct {
		name  string
		setup func() (*DownloadResult, error)
	}{
		{"a refresh on the DASH manifest", dash},
		{"a refresh on the manifest-free formats", manifestless},
	} {
		res, err := c.setup()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.VideoDownloader.MediaCookies() != want || res.AudioDownloader.MediaCookies() != want {
			t.Errorf("%s: its downloaders have a media-cookie state of their own, want the run's", c.name)
		}
	}
}
