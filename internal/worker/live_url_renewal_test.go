package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestManifestlessLiveDownloadersRenewBeforeExpiry pins audit G48's worker
// half: each live manifest-free DASH downloader is built with the engine's
// proactive renewal armed — formatURLExpiry reading its URL's expire=, and an
// OnProactiveRefresh that re-extracts and re-resolves its itag — while a
// post-live run's are not (yt-dlp's url_feed is live only). The renewal skips
// the 403 recovery's cache invalidation: the session's visitor data survives
// it, where the recovery (OnCredentialRefresh) drops it.
//
// Mutants: URLExpiry left out of either downloader's options (its row);
// liveURLRenewal arming a post-live run (the post-live row); the renewal
// built on refreshGvsCredentials, or renewGvsCredentials invalidating
// whatever recovering says (the visitor data is gone after the renewal).
func TestManifestlessLiveDownloadersRenewBeforeExpiry(t *testing.T) {
	orig := refreshVideoInfo
	refreshVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
		fresh := manifestlessPotInfo("tv_auth", "tv_auth")
		for i := range fresh.Formats {
			fresh.Formats[i].URL = strings.Replace(fresh.Formats[i].URL, "127.0.0.1:1", "fresh.invalid", 1) + "&expire=1900000000"
		}
		return fresh, nil
	}
	t.Cleanup(func() { refreshVideoInfo = orig })

	for _, tc := range []struct {
		name      string
		status    youtube.StreamStatus
		wantArmed bool
	}{
		{"live", youtube.StreamLive, true},
		{"post-live", youtube.StreamPostLive, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := recordDownloaders(t)
			job, _ := vodPotJob(t)
			job.YT = youtube.NewService(nil, &discardLogger{})
			job.YT.SetVisitorData("vd-123")
			info := manifestlessPotInfo("tv_auth", "tv_auth")
			info.StreamStatus = tc.status
			if _, err := DownloadManifestlessDash(context.Background(), job, info, stubCipherSolver{}, nil, nil, nil); err != nil {
				t.Fatalf("DownloadManifestlessDash: %v", err)
			}
			if len(*built) != 2 {
				t.Fatalf("built %d downloaders, want video and audio", len(*built))
			}
			for i, opts := range *built {
				if (opts.URLExpiry != nil) != tc.wantArmed || opts.OnProactiveRefresh == nil {
					t.Fatalf("downloader %d: URLExpiry set %v, OnProactiveRefresh set %v; want renewal armed %v",
						i, opts.URLExpiry != nil, opts.OnProactiveRefresh != nil, tc.wantArmed)
				}
				if !tc.wantArmed {
					continue
				}
				if exp, ok := opts.URLExpiry("https://r1.googlevideo.com/videoplayback?expire=1800000000&itag=299"); !ok || !exp.Equal(time.Unix(1_800_000_000, 0)) {
					t.Errorf("downloader %d: URLExpiry = %v, %v; want expire= read (formatURLExpiry)", i, exp, ok)
				}
				url, _ := opts.OnProactiveRefresh()
				if !strings.Contains(url, "fresh.invalid") || !strings.Contains(url, "expire=1900000000") {
					t.Errorf("downloader %d: renewal URL = %q, want the re-extraction's", i, url)
				}
				if vd := job.YT.GetVisitorData(); vd != "vd-123" {
					t.Errorf("downloader %d: visitor data after the renewal = %q, want it kept (no 403 invalidation)", i, vd)
				}
			}
			if tc.wantArmed {
				// The 403 recovery, by contrast, drops it.
				(*built)[0].OnCredentialRefresh()
				if vd := job.YT.GetVisitorData(); vd != "" {
					t.Errorf("visitor data after the 403 recovery = %q, want it invalidated", vd)
				}
			}
		})
	}
}

// TestLiveURLRenewalReadsExpire pins liveURLRenewal's production value: the
// VOD slot wait's own expire= reader, for a live stream only.
func TestLiveURLRenewalReadsExpire(t *testing.T) {
	read := liveURLRenewal(&youtube.VideoInfo{StreamStatus: youtube.StreamLive})
	for _, raw := range []string{
		"https://r1.googlevideo.com/videoplayback?expire=1800000000&itag=299",
		"https://r1.googlevideo.com/videoplayback/expire/1800000000/itag/299",
		"https://r1.googlevideo.com/videoplayback?itag=299",
	} {
		gotT, gotOK := read(raw)
		wantT, wantOK := formatURLExpiry(raw)
		if !gotT.Equal(wantT) || gotOK != wantOK {
			t.Errorf("%s: liveURLRenewal read %v, %v; formatURLExpiry %v, %v", raw, gotT, gotOK, wantT, wantOK)
		}
	}
	for _, info := range []*youtube.VideoInfo{nil, {StreamStatus: youtube.StreamPostLive}, {StreamStatus: youtube.StreamVOD}} {
		if liveURLRenewal(info) != nil {
			t.Errorf("liveURLRenewal(%v) armed a stream that is not live", info)
		}
	}
}
