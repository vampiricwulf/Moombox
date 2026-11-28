package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestLatencyModeStoredOnceAtTheFirstExtraction is owner ruling RV-19's
// write: the first extraction that shows the broadcast begun stores its
// mode (a live capture's, a past stream's VOD), an answer with videoDetails
// and no class stores "unstated", and nothing replaces a stored mode. An
// upcoming broadcast, an ordinary upload (a VOD with no class) and an answer
// with no videoDetails store nothing.
//
// Mutants: the job.LatencyMode guard dropped (the later LOW answer replaces
// NORMAL); the VOD arm storing a classless answer (the upload reads
// unstated); the upcoming case storing; the in-memory job not synced.
func TestLatencyModeStoredOnceAtTheFirstExtraction(t *testing.T) {
	const normal = "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL"
	info := func(status youtube.StreamStatus, class string, asked bool) *youtube.VideoInfo {
		i := &youtube.VideoInfo{LatencyClass: class, LatencyClassAsked: asked}
		i.SetStatus(status)
		return i
	}
	for _, tc := range []struct {
		name string
		info *youtube.VideoInfo
		want database.LatencyMode
	}{
		{"live", info(youtube.StreamLive, normal, true), database.LatencyModeNormal},
		{"post_live", info(youtube.StreamPostLive, "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_ULTRA_LOW", true), database.LatencyModeUltraLow},
		{"a past stream's VOD", info(youtube.StreamVOD, "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW", true), database.LatencyModeLow},
		{"live, YouTube would not say", info(youtube.StreamLive, "", true), database.LatencyModeUnstated},
		{"an ordinary upload", info(youtube.StreamVOD, "", true), database.LatencyModeNone},
		{"upcoming", info(youtube.StreamUpcoming, normal, true), database.LatencyModeNone},
		{"not a stream", info(youtube.StreamNotAStream, "", true), database.LatencyModeNone},
		{"no videoDetails", info(youtube.StreamLive, "", false), database.LatencyModeNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, db := newTestSP(t)
			job := addTestJob(t, db, &database.Job{ID: "yt_lm", VideoID: "lm", Platform: "youtube", Status: database.StatusUpcoming})
			sp.updateJobMetadata(job, tc.info, true)
			got, _ := db.GetJob("yt_lm")
			if got.LatencyMode != tc.want || job.LatencyMode != tc.want {
				t.Errorf("stored %q, local job %q, want %q", got.LatencyMode, job.LatencyMode, tc.want)
			}
		})
	}

	t.Run("kept", func(t *testing.T) {
		sp, db := newTestSP(t)
		job := addTestJob(t, db, &database.Job{ID: "yt_keep", VideoID: "keep", Platform: "youtube", Status: database.StatusUpcoming})
		sp.updateJobMetadata(job, info(youtube.StreamLive, normal, true), true)
		sp.updateJobMetadata(job, info(youtube.StreamLive, "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW", true), true)
		sp.updateJobMetadata(job, info(youtube.StreamPostLive, "", true), true)
		// A resume reads the row afresh.
		fresh, _ := db.GetJob("yt_keep")
		sp.updateJobMetadata(fresh, info(youtube.StreamVOD, "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_ULTRA_LOW", true), true)
		if got, _ := db.GetJob("yt_keep"); got.LatencyMode != database.LatencyModeNormal {
			t.Errorf("after later answers: %q, want the first, normal", got.LatencyMode)
		}
	})
}
