package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestDashManifestWarnsADamagedPinOnce: on the DASH-manifest path
// (DashStrategy), a manual itag the manifest honours and whose copy in the
// player response's formats is damaged logs the whole-file path's one Warn —
// "Manual video itag N is possibly damaged (shorter than the video);
// recording it as pinned", or audio — and logs it once per job run however
// many times the strategy is set up again (a mid-stream refresh re-runs it).
// An intact pin, a pin the manifest could not honour, and a damaged itag no
// one pinned (the config default, or the selection's own pick) say nothing.
// Before, a damaged pin was warned of on the whole-file path only (PK2), and
// a live or post-live capture recorded it with no word.
//
// Mutants: drop warnDamagedManifestPins from DownloadDash — no row finds
// its line; drop the honoured check (pin.stream.Itag) — the unhonoured row
// warns; drop the Once gate — the refreshed row warns twice; read the
// config default as a pin — the config row warns.
func TestDashManifestWarnsADamagedPinOnce(t *testing.T) {
	videoLine := "[FormatSelector] Manual video itag 299 is possibly damaged (shorter than the video); recording it as pinned"
	audioLine := "[FormatSelector] Manual audio itag 140 is possibly damaged (shorter than the video); recording it as pinned"
	formats := func(damagedVideo, damagedAudio bool) []youtube.Format {
		return []youtube.Format{
			{Itag: 299, MimeType: `video/mp4; codecs="avc1.64002a"`, Damaged: damagedVideo},
			{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`, Damaged: damagedAudio},
			// Damaged, and listed by the player response, but not in the
			// manifest: a pin of it falls back to 299 (warnUnhonouredPins).
			{Itag: 303, MimeType: `video/webm; codecs="vp9"`, Damaged: true},
		}
	}
	itag := func(n int) *int { return &n }
	for _, tc := range []struct {
		name         string
		video, audio *int
		configVideo  int
		damagedV     bool
		damagedA     bool
		setups       int
		wantV, wantA int
	}{
		{name: "damaged video pin", video: itag(299), damagedV: true, setups: 1, wantV: 1},
		{name: "damaged audio pin", audio: itag(140), damagedA: true, setups: 1, wantA: 1},
		{name: "damaged video pin, refreshed twice", video: itag(299), damagedV: true, setups: 3, wantV: 1},
		{name: "intact pins", video: itag(299), audio: itag(140), setups: 1},
		{name: "unhonoured pin", video: itag(303), damagedV: true, setups: 1},
		{name: "unpinned damaged pick", damagedV: true, damagedA: true, setups: 1},
		{name: "config default", configVideo: 299, damagedV: true, setups: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newManifestServer(t, potTestMPD)
			job, logs := vodPotJob(t)
			job.Once = &runOnce{}
			job.Job.SelectedVideoItag, job.Job.SelectedAudioItag = tc.video, tc.audio
			job.Config.VideoItag = tc.configVideo
			info := &youtube.VideoInfo{
				StreamStatus:       youtube.StreamLive,
				DashManifestURL:    srv.URL + "/api/manifest/dash/id/abc",
				DashManifestSource: "tv_auth",
				Formats:            formats(tc.damagedV, tc.damagedA),
			}
			for range tc.setups {
				if _, err := DownloadDash(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil); err != nil {
					t.Fatalf("DownloadDash: %v", err)
				}
			}
			if n := len(potLines(logs, videoLine)); n != tc.wantV {
				t.Errorf("%d damaged video-pin lines, want %d", n, tc.wantV)
			}
			if n := len(potLines(logs, audioLine)); n != tc.wantA {
				t.Errorf("%d damaged audio-pin lines, want %d", n, tc.wantA)
			}
			if all := logLines(logs, func(m string) bool { return strings.Contains(m, "is possibly damaged") }); len(all) != tc.wantV+tc.wantA {
				t.Errorf("damaged-pin lines %v, want %d", all, tc.wantV+tc.wantA)
			}
		})
	}
}

// TestBuildJobContextGivesTheRunItsOnceRecord pins the production value of
// JobContext.Once: every run gets a record of its own, so its once-a-run
// lines and switches are once a run, not once a setup.
func TestBuildJobContextGivesTheRunItsOnceRecord(t *testing.T) {
	w, db := testWorkerSetup(t)
	backlogRetryJob(t, db, "once_ctx", 0, false)
	row, _ := db.GetJob("once_ctx")
	a, b := w.buildJobContext(row), w.buildJobContext(row)
	if a.Once == nil || b.Once == nil || a.Once == b.Once {
		t.Errorf("Once = %p and %p, want a fresh record per run", a.Once, b.Once)
	}
}
