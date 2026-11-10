package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestJobNeedsStagingMatchesTheCleanupVerdict is the K17 parity pin: the
// orphan sweep's jobNeedsStaging must shield a Finished job's staging
// exactly when cleanupStagingAfterMux's decideStagingCleanup keeps it,
// except that the tail and chat keeps expire on the one age rule
// (incompleteStagingExpired) while the four media keeps never do. Every
// subset of the six shields, fresh and aged, on disk as the cleanup would
// meet it.
//
// Mutants: putting a media keep under the age rule (stagingKeep.expires), or
// moving the tail or chat keep ahead of a media keep in decideStagingCleanup
// — jobNeedsStaging reads only the first keep, so an aged expiring one would
// hide the media behind it (the shield-set oracle below).
func TestJobNeedsStagingMatchesTheCleanupVerdict(t *testing.T) {
	const (
		shAside = 1 << iota
		shUnmuxedPart
		shUnusedRoot
		shMuxGap
		shTail
		shChat
		shAll = 1<<iota - 1
	)
	cfg := &config.MoomboxConfig{Downloader: config.DownloaderConfig{
		IncompleteStagingExpiryDays: config.FlexDuration{Value: 7},
	}}
	ages := map[string]string{
		"fresh": time.Now().UTC().Format(time.RFC3339),
		"aged":  time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	write := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, db := testWorkerSetup(t)
	for set := 0; set <= shAll; set++ {
		for ageName, updated := range ages {
			name := fmt.Sprintf("shields=%06b/%s", set, ageName)
			t.Run(name, func(t *testing.T) {
				jobID := fmt.Sprintf("j%02d%s", set, ageName)
				dir := filepath.Join(t.TempDir(), jobID)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, Status: database.StatusFinished}); err != nil {
					t.Fatal(err)
				}
				if set&shAside != 0 {
					write(t, filepath.Join(dir, "video.mp4.restart-1700000000"), "set aside")
				}
				if set&shUnmuxedPart != 0 {
					write(t, filepath.Join(dir, "seg_1", "video_stream"), "\x00\x00\x00\x18ftypdash")
				}
				if set&shUnusedRoot != 0 {
					write(t, filepath.Join(dir, "video.mp4"), "\x00\x00\x00\x18ftypdash")
					if err := db.AddSegment(&database.Segment{JobID: jobID, SegmentIndex: 0, Filename: "x - part1.mp4"}); err != nil {
						t.Fatal(err)
					}
				}
				if set&shMuxGap != 0 {
					media := filepath.Join(dir, "seg_9", "audio_stream")
					write(t, media, "staged bytes")
					if err := writeMuxGapRecord(media, muxGapRecord{Size: int64(len("staged bytes")), Dropped: []string{"bytes 1-2"}}); err != nil {
						t.Fatal(err)
					}
					if err := db.AddSegment(&database.Segment{JobID: jobID, SegmentIndex: 9, Filename: "x - part10.mp4"}); err != nil {
						t.Fatal(err)
					}
				}
				job := &database.Job{ID: jobID, Status: database.StatusFinished, UpdatedAt: updated,
					IncompleteTail: set&shTail != 0}
				if set&shChat != 0 {
					job.ChatStatus = database.ChatIncomplete
				}

				v := decideStagingCleanup(db, job, jobID, dir)
				expiring := v.keep == keepStagingForTail || v.keep == keepStagingChatOnly
				want := v.keep != removeStaging && !(expiring && incompleteStagingExpired(cfg, job))
				got := jobNeedsStaging(db, cfg, job, dir)
				if got != want {
					t.Errorf("jobNeedsStaging = %v, cleanup verdict %d (aged %v) wants %v", got, v.keep, ageName == "aged", want)
				}
				// The same answer from the shield SET alone, with no reading
				// of the verdict: any media shield holds for good, the tail
				// and chat shields only until they age. jobNeedsStaging reads
				// only the verdict's FIRST keep and lifts it when it expires,
				// so it agrees with this only while every never-expiring keep
				// is checked ahead of the two expiring ones in
				// decideStagingCleanup. A new shield gets its bit here, on the
				// side of this rule it belongs to.
				//
				// Mutant: decideStagingCleanup checking the tail (or chat)
				// keep ahead of a media keep — the aged rows with both read
				// false here.
				const mediaShields = shAside | shUnmuxedPart | shUnusedRoot | shMuxGap
				oracle := set&mediaShields != 0 || (set&(shTail|shChat) != 0 && ageName == "fresh")
				if got != oracle {
					t.Errorf("jobNeedsStaging = %v, the shield set %06b (aged %v) wants %v — a never-expiring keep is checked after an expiring one", got, set, ageName == "aged", oracle)
				}
				// Each shield on its own must actually shield (fresh), so the
				// parity above cannot pass on two functions that both see
				// nothing.
				if ageName == "fresh" && set != 0 && v.keep == removeStaging {
					t.Errorf("decideStagingCleanup kept nothing for shields %06b — the fixture built no shield", set)
				}
				// Never for a job that is not Finished.
				job.Status = database.StatusError
				if jobNeedsStaging(db, cfg, job, dir) {
					t.Error("jobNeedsStaging shielded a job that is not Finished")
				}
			})
		}
	}
}
