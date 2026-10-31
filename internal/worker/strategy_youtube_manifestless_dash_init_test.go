package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// TestDownloadManifestlessDashMarksWhereTheInitLives pins the strategy half of
// G3 (2026-10-31 audit, wave W3): a manifest-free stream carries its ftyp+moov
// only inline at sq=0, so every downloader the strategy builds must tell the
// engine where its init comes from. A start that is not forced — a fresh
// capture, or a live crash recovery seeded from the DB position, which the
// engine resumes only into a file that already holds the init — sets
// InitInlineAtStart, so the engine's first-segment hunt never steps off sq=0
// and no media opens an empty file without it. A forced part (quality split,
// restart discovery, the post-live refresh rebuild) instead points InitURL at
// sq=0 with InitFromSegment, and never both.
//
// Mutants: dropping InitInlineAtStart from either DownloaderOptions literal —
// the fresh and DB-seeded rows find it false for that stream; setting it on
// every start — the forced-part row finds both init sources armed.
func TestDownloadManifestlessDashMarksWhereTheInitLives(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		videoStart, audioStart int  // the orchestrator's forced sequences
		dbSeq                  *int // the job's persisted last_*_seq
		wantInline             bool
	}{
		{name: "fresh start", wantInline: true},
		{name: "live crash recovery from the DB position", dbSeq: new(500), wantInline: true},
		{name: "forced part", videoStart: 120, audioStart: 121},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var built []engine.DownloaderOptions
			prev := newSegmentDownloader
			newSegmentDownloader = func(opts engine.DownloaderOptions) *engine.SegmentDownloader {
				built = append(built, opts)
				return engine.NewSegmentDownloader(opts)
			}
			t.Cleanup(func() { newSegmentDownloader = prev })

			job, _ := vodPotJob(t)
			job.VideoStartSeq, job.AudioStartSeq = tc.videoStart, tc.audioStart
			job.Job.LastVideoSeq, job.Job.LastAudioSeq = tc.dbSeq, tc.dbSeq
			if _, err := DownloadManifestlessDash(context.Background(), job, manifestlessPotInfo("web", "web"), stubCipherSolver{}, nil, nil, nil); err != nil {
				t.Fatalf("DownloadManifestlessDash: %v", err)
			}
			if len(built) != 2 {
				t.Fatalf("built %d downloaders, want video and audio", len(built))
			}
			for i, opts := range built {
				stream := [...]string{"video", "audio"}[i]
				if opts.InitInlineAtStart != tc.wantInline {
					t.Errorf("%s: InitInlineAtStart = %v, want %v", stream, opts.InitInlineAtStart, tc.wantInline)
				}
				forced := !tc.wantInline
				if (opts.InitURL != "") != forced || opts.InitFromSegment != forced {
					t.Errorf("%s: InitURL = %q, InitFromSegment = %v — want the sq=0 init URL exactly when the start is forced (%v)",
						stream, opts.InitURL, opts.InitFromSegment, forced)
				}
				if forced && opts.InitURL != manifestlessSq0URL(opts.BaseURL) {
					t.Errorf("%s: InitURL = %q, want sq=0 of the stream's own base %q", stream, opts.InitURL, manifestlessSq0URL(opts.BaseURL))
				}
			}
		})
	}
}
