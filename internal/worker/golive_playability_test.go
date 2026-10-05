package worker

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The go-live fetch re-reads the credential half of the playability verdict:
// a members-only job whose cookies died during the wait used to go on to "no
// download strategy available", a plain Error the credential-recovery sweep
// (COOKIES? rows only) never looked at. It now stops with the same sentinel
// the initial Process uses — but only when the fetch has no formats, so a
// stream that can still be downloaded is never held back by the verdict.
//
// Mutant: completeStreamTransition without the check — the dead-cookie row
// downloads.
func TestGoLiveStopsOnDeadCredentials(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "golive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sp := &StreamProcessor{db: db, logger: nopWorkerLogger{}}

	walled := func(formats int) *youtube.VideoInfo {
		info := &youtube.VideoInfo{
			StreamStatus:     youtube.StreamLive,
			PlayabilityError: youtube.PlayabilityMembersOnly,
			SessionAuth:      youtube.SessionAuthLoggedOut,
		}
		for range formats {
			info.Formats = append(info.Formats, youtube.Format{})
		}
		return info
	}
	for _, tc := range []struct {
		name     string
		info     *youtube.VideoInfo
		download bool
	}{
		{"dead cookies, nothing to download", walled(0), false},
		{"walled verdict but formats served", walled(2), true},
		{"healthy", &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: []youtube.Format{{}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &database.Job{ID: "yt_" + tc.name, VideoID: "v", Platform: "youtube", Status: database.StatusUpcoming}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			res := sp.completeStreamTransition(job, tc.info, nil)
			if res.ShouldDownload != tc.download {
				t.Fatalf("ShouldDownload = %v, want %v (%+v)", res.ShouldDownload, tc.download, res)
			}
			if !tc.download && !errors.Is(res.ErrSentinel, ErrCookiesRequired) {
				t.Errorf("sentinel = %v, want ErrCookiesRequired so the job parks at COOKIES?", res.ErrSentinel)
			}
		})
	}
}
