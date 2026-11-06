package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// removalFixture seeds a channel with one job in every state a removal sorts,
// two of them parked with something in staging, plus a job of another
// channel. Each row has its history row, as the host creates every job.
func removalFixture(t *testing.T) (db *database.Database, staging, ch string) {
	t.Helper()
	db = openTestDB(t)
	staging = t.TempDir()
	ch, other := "UC_removing", "UC_other"
	for i, row := range []struct {
		id      string
		status  database.JobStatus
		ch      *string
		staged  bool // a file in the job's staging dir
		emptyOK bool // an empty staging dir, which is no footage
	}{
		{"queued", database.StatusQueued, &ch, false, false},
		{"upcoming", database.StatusUpcoming, &ch, false, false},
		{"parked_capture", database.StatusCookies, &ch, true, false},
		{"parked_empty", database.StatusCookies, &ch, false, true},
		{"queued_checkpoint", database.StatusQueued, &ch, true, false},
		{"downloading", database.StatusDownloading, &ch, true, false},
		{"muxing", database.StatusMuxing, &ch, true, false},
		{"finished", database.StatusFinished, &ch, false, false},
		{"errored", database.StatusError, &ch, false, false},
		{"elsewhere", database.StatusQueued, &other, false, false},
	} {
		created := fmt.Sprintf("2026-07-01T00:00:%02dZ", i)
		if _, err := db.AddJob(&database.Job{ID: row.id, VideoID: row.id, URL: "u", Title: "T " + row.id,
			Status: row.status, ChannelID: row.ch, QueuePriority: 1, CreatedAt: created, UpdatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if err := db.AddToHistory(row.id); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(staging, row.id)
		if row.staged || row.emptyOK {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if row.staged {
			if err := os.WriteFile(filepath.Join(dir, "video_00001.ts"), []byte("footage"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db, staging, ch
}

// TestSummarizeChannelRemoval: the confirmation's counts. Every job of the
// channel is in Total; Pending is the Queued/Upcoming/COOKIES? rows with
// nothing in staging; the pending-status rows WITH something staged are named
// in Footage (W25-09: a COOKIES? row can be a live capture parked mid-stream);
// Live/Downloading/Muxing are Active; another channel's job counts nowhere.
//
// Mutants killed: the HasStagingFiles check dropped (the footage rows count
// as pending); the Active arm dropped (Active 0); Muxing missing from it.
func TestSummarizeChannelRemoval(t *testing.T) {
	db, staging, ch := removalFixture(t)
	sum, err := SummarizeChannelRemoval(db, staging, ch, "youtube")
	if err != nil {
		t.Fatalf("SummarizeChannelRemoval: %v", err)
	}
	if sum.Total != 9 || sum.Pending != 3 || sum.Active != 2 {
		t.Errorf("total/pending/active = %d/%d/%d, want 9/3/2", sum.Total, sum.Pending, sum.Active)
	}
	want := []ChannelJobRef{
		{ID: "parked_capture", Title: "T parked_capture", Status: database.StatusCookies},
		{ID: "queued_checkpoint", Title: "T queued_checkpoint", Status: database.StatusQueued},
	}
	if !slices.Equal(sum.Footage, want) {
		t.Errorf("footage = %+v, want %+v", sum.Footage, want)
	}
}

// TestDeletePendingChannelJobs: the "delete its pending jobs" choice deletes
// the pending rows with their history and nothing else — not a parked
// recording with footage (whose row is the only way back to it), not an
// active download, not a finished or failed job, not another channel's.
//
// Mutants killed: the footage keep list not passed to the delete (the parked
// capture and the checkpointed Queued row are deleted, their footage left
// row-less); the pending status set widened to every status (the finished
// and errored rows go too).
func TestDeletePendingChannelJobs(t *testing.T) {
	db, staging, ch := removalFixture(t)
	n, kept, err := DeletePendingChannelJobs(db, staging, ch)
	if err != nil {
		t.Fatalf("DeletePendingChannelJobs: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}
	if len(kept) != 2 || kept[0].ID != "parked_capture" || kept[1].ID != "queued_checkpoint" {
		t.Errorf("kept footage = %+v, want parked_capture and queued_checkpoint", kept)
	}
	gone := map[string]bool{"queued": true, "upcoming": true, "parked_empty": true}
	for _, id := range []string{"queued", "upcoming", "parked_capture", "parked_empty", "queued_checkpoint",
		"downloading", "muxing", "finished", "errored", "elsewhere"} {
		job, _ := db.GetJob(id)
		has, _ := db.HasProcessed(id)
		if want := !gone[id]; (job != nil) != want || has != want {
			t.Errorf("%s: job kept = %v, history kept = %v; want both %v", id, job != nil, has, want)
		}
	}
	if !HasStagingFiles(staging, "parked_capture") {
		t.Error("the parked capture's staging is gone")
	}
}

// TestSummarizeTwitchChannelRemoval: a Twitch channel's jobs carry no
// channel_id, so the summary ties them by the login TwitchJobLogin reads off
// them — the monitor's rows (https://twitch.tv/<login>) and a Web add's
// (https://www.twitch.tv/<login>, a tw_manual_ row among them) — matched
// without regard to case, the config ID being typed by hand. Counted by
// channel_id they were none, and both prompts said "It has no jobs." over a
// capture in progress and one parked with its footage. None is pending: the
// delete reaches rows by channel_id alone, and DeletePendingChannelJobs
// deletes none of a Twitch channel's captures or VODs (its queued clips,
// which carry the login, are TestTwitchChannelRemovalCountsItsQueuedClips').
//
// Mutants killed: the twitch branch of SummarizeChannelRemoval dropped (total
// 0); strings.EqualFold made a lowercased == (the mixed-case config ID
// matches nothing);
// summarizeRemoval called with deletable true for Twitch (the manual row
// reads as pending, a delete the UIs would offer and not perform); the login
// matched by strings.Contains on the URL (another channel whose login extends
// this one's is counted).
func TestSummarizeTwitchChannelRemoval(t *testing.T) {
	db := openTestDB(t)
	staging := t.TempDir()
	for i, row := range []struct {
		id, url, channelName string
		status               database.JobStatus
		staged               bool
	}{
		{"tw_1001", "https://twitch.tv/somestreamer", "SomeStreamer", database.StatusFinished, false},
		{"tw_1002", "https://twitch.tv/somestreamer", "SomeStreamer", database.StatusCookies, true},
		{"tw_1003", "https://twitch.tv/somestreamer", "SomeStreamer", database.StatusDownloading, true},
		{"tw_manual_somestreamer_17", "https://www.twitch.tv/somestreamer", "somestreamer", database.StatusUpcoming, false},
		{"tw_v999", "https://www.twitch.tv/videos/999", "Manual", database.StatusUpcoming, false},
		{"tw_2001", "https://twitch.tv/somestreamer2", "SomeStreamer2", database.StatusCookies, false},
	} {
		created := fmt.Sprintf("2026-07-01T00:00:%02dZ", i)
		if _, err := db.AddJob(&database.Job{ID: row.id, VideoID: row.id, URL: row.url, Title: "T " + row.id,
			ChannelName: row.channelName, Platform: "twitch", Status: row.status,
			CreatedAt: created, UpdatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if row.staged {
			dir := filepath.Join(staging, row.id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "video_00001.ts"), []byte("footage"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	sum, err := SummarizeChannelRemoval(db, staging, "SomeStreamer", "twitch")
	if err != nil {
		t.Fatalf("SummarizeChannelRemoval: %v", err)
	}
	if sum.Total != 4 || sum.Pending != 0 || sum.Active != 1 {
		t.Errorf("total/pending/active = %d/%d/%d, want 4/0/1", sum.Total, sum.Pending, sum.Active)
	}
	want := []ChannelJobRef{{ID: "tw_1002", Title: "T tw_1002", Status: database.StatusCookies}}
	if !slices.Equal(sum.Footage, want) {
		t.Errorf("footage = %+v, want %+v", sum.Footage, want)
	}

	n, kept, err := DeletePendingChannelJobs(db, staging, "SomeStreamer")
	if err != nil || n != 0 || len(kept) != 0 {
		t.Errorf("delete = %d deleted, kept %+v, err %v; want nothing", n, kept, err)
	}
	if j, _ := db.GetJob("tw_manual_somestreamer_17"); j == nil {
		t.Error("the delete reached a Twitch row")
	}
}

// TestTwitchChannelRemovalCountsItsQueuedClips: the clips a Twitch channel's
// twitch_archive_clips sweep queued carry its login as channel_id
// (newTwitchClipJob), so its removal counts them as a YouTube channel's rows
// are — a Queued clip pending, one with a partial in staging named as
// footage, a Finished one in the total alone — and "delete its pending jobs"
// deletes the pending one with its history, while the channel's capture,
// tied by login and pending in status, is counted in the total and never
// deleted.
//
// The VODs its twitch_vod_backlog queued carry its login too and are deleted
// like the clips; a VOD of a listing the operator pasted carries it for
// pacing only, and is counted in the total and never deleted (owner decision
// PR-D1).
//
// Mutants: the carried rows summarized with deletable false (pending 0, a
// delete the UIs would not offer); the carried rows left out of the summary
// (total 4); the pasted VOD counted as pending (pending 3), or not kept by
// the delete (deleted).
func TestTwitchChannelRemovalCountsItsQueuedClips(t *testing.T) {
	db := openTestDB(t)
	staging := t.TempDir()
	login := "vanillatv"
	for i, row := range []struct {
		id, url string
		ch      *string
		status  database.JobStatus
		staged  bool
		manual  bool
	}{
		{"tw_clip_QueuedSlug", "https://www.twitch.tv/vanillatv/clip/QueuedSlug", &login, database.StatusQueued, false, false},
		{"tw_clip_PartialSlug", "https://www.twitch.tv/vanillatv/clip/PartialSlug", &login, database.StatusQueued, true, false},
		{"tw_clip_DoneSlug", "https://www.twitch.tv/vanillatv/clip/DoneSlug", &login, database.StatusFinished, false, false},
		{"tw_manual_vanillatv_1", "https://www.twitch.tv/vanillatv", nil, database.StatusUpcoming, false, false},
		{"tw_v1", "https://www.twitch.tv/videos/1", &login, database.StatusQueued, false, false}, // the backlog's
		{"tw_v2", "https://www.twitch.tv/videos/2", &login, database.StatusQueued, false, true},  // a pasted listing's
	} {
		created := fmt.Sprintf("2026-07-01T00:00:%02dZ", i)
		if _, err := db.AddJob(&database.Job{ID: row.id, VideoID: row.id, URL: row.url, Title: "T " + row.id,
			ChannelName: "VanillaTV", Platform: "twitch", Status: row.status, ChannelID: row.ch, QueuePriority: 1,
			IsVod: strings.HasPrefix(row.id, "tw_v"), ManuallyAdded: row.manual, CreatedAt: created, UpdatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if err := db.AddToHistory(row.id); err != nil {
			t.Fatal(err)
		}
		if row.staged {
			dir := filepath.Join(staging, row.id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "video.mp4"), []byte("partial"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	sum, err := SummarizeChannelRemoval(db, staging, login, "twitch")
	if err != nil {
		t.Fatalf("SummarizeChannelRemoval: %v", err)
	}
	if sum.Total != 6 || sum.Pending != 2 || sum.Active != 0 {
		t.Errorf("total/pending/active = %d/%d/%d, want 6/2/0", sum.Total, sum.Pending, sum.Active)
	}
	if want := []ChannelJobRef{{ID: "tw_clip_PartialSlug", Title: "T tw_clip_PartialSlug", Status: database.StatusQueued}}; !slices.Equal(sum.Footage, want) {
		t.Errorf("footage = %+v, want %+v", sum.Footage, want)
	}

	n, _, err := DeletePendingChannelJobs(db, staging, login)
	if err != nil || n != sum.Pending {
		t.Errorf("delete = %d deleted (%v), want the %d the summary counted", n, err, sum.Pending)
	}
	for _, id := range []string{"tw_clip_QueuedSlug", "tw_v1"} {
		if j, _ := db.GetJob(id); j != nil {
			t.Errorf("%s, the channel's backlog, survived the delete", id)
		}
	}
	for _, id := range []string{"tw_clip_PartialSlug", "tw_clip_DoneSlug", "tw_manual_vanillatv_1", "tw_v2"} {
		if j, _ := db.GetJob(id); j == nil {
			t.Errorf("%s was deleted", id)
		}
	}
}
