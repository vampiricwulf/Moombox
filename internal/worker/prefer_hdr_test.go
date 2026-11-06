package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestBuildJobContextSnapshotsPreferHDR pins the production value of the
// JobConfig.PreferHDR seam: buildJobContext copies downloader.prefer_hdr, so
// every selection the job makes ranks by the operator's setting. A
// JobConfig built by hand (most tests) leaves it false.
//
// Mutant: the PreferHDR line removed from buildJobContext's JobConfig.
func TestBuildJobContextSnapshotsPreferHDR(t *testing.T) {
	w, _ := testWorkerSetup(t)
	for _, want := range []bool{true, false} {
		w.cfg.Downloader.PreferHDR = want
		jc := w.buildJobContext(&database.Job{ID: "j", VideoID: "v"})
		if jc.Config.PreferHDR != want {
			t.Errorf("prefer_hdr=%v: JobConfig.PreferHDR = %v", want, jc.Config.PreferHDR)
		}
	}
}

// TestVodSelectionHonoursPreferHDR: the whole-file VOD path ranks by the
// job's own prefer_hdr — a VP9.2 HDR 335 beside the AV1 SDR 399 at 1080p60
// is taken with it on and passed over with it off (owner decision D13).
//
// Mutant: selectVodFormats passing preferHDR true whatever the job says.
func TestVodSelectionHonoursPreferHDR(t *testing.T) {
	pool := []youtube.Format{
		vodVideo(399, `video/mp4; codecs="av01.0.09M.08"`, 1920, 1080, 60, 4_000_000, ""),
		vodVideo(335, `video/webm; codecs="vp09.02.41.10.01.09.16.09.00"`, 1920, 1080, 60, 3_000_000, ""),
		vodAudio(251, `audio/webm; codecs="opus"`, 160_000, ""),
	}
	for _, tc := range []struct {
		preferHDR bool
		want      int
	}{{true, 335}, {false, 399}} {
		job := vodJob(t, 2160, true, "")
		job.Config.PreferHDR = tc.preferHDR
		_, res := selectVodFormats(job, pool, nil, nil)
		if res.VideoFormat == nil || res.VideoFormat.Itag != tc.want {
			t.Errorf("prefer_hdr=%v: video %+v, want itag %d", tc.preferHDR, res.VideoFormat, tc.want)
		}
	}
}
