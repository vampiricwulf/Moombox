package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// realisticProgressJob is the row the verifier measured: a long title, full
// Windows output paths, an avatar URL and a 5 KB description — the shape a
// real YouTube archive carries while it downloads.
func realisticProgressJob() *database.Job {
	seq := func(n int) *int { return &n }
	return &database.Job{
		ID:                "dQw4w9WgXcQ",
		VideoID:           "dQw4w9WgXcQ",
		URL:               "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Title:             "【歌枠】Singing Stream with a very long title that real archives actually have",
		ChannelName:       "Example Channel Ch.",
		Platform:          "youtube",
		Status:            database.StatusDownloading,
		Progress:          "V:12345 A:12345 C:67890",
		Percent:           37.5,
		ETA:               "01:23:45",
		Speed:             "12.3 MB/s",
		CreatedAt:         "2026-09-17T10:00:00Z",
		UpdatedAt:         "2026-09-17T11:23:45Z",
		LastVideoSeq:      seq(12345),
		LastAudioSeq:      seq(12345),
		TotalVideoSeq:     seq(20000),
		TotalAudioSeq:     seq(20000),
		TotalChatMessages: seq(67890),
		ThumbnailURL:      "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg",
		Description:       strings.Repeat("A typical YouTube description line.\n", 140), // ~5 KB
		OutputFile:        `D:\media\output\Example Channel Ch\2026-09-17 Singing Stream [dQw4w9WgXcQ].mp4`,
		Filename:          `Example Channel Ch\2026-09-17 Singing Stream [dQw4w9WgXcQ].mp4`,
		OutputDirectory:   `D:\media\output`,
		ChannelAvatarURL:  "https://yt3.ggpht.com/ytc/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=s176-c-k-c0x00ffffff-no-rj",
	}
}

// TestProgressFrameIsAFractionOfTheFullRow is the WEB-5 measurement, kept as a
// gate. The whole job row went out 60 times a second per dashboard per active
// job; the slim frame carries only what a tick moves.
//
// THE MUTANT: newJobProgressFrame returning the job itself (or the frame
// gaining Description / Gaps) — both assertions fail at once.
func TestProgressFrameIsAFractionOfTheFullRow(t *testing.T) {
	job := realisticProgressJob()

	full, err := json.Marshal(web.WSMessage{Type: "job_update", Payload: job})
	if err != nil {
		t.Fatalf("marshal job_update: %v", err)
	}
	slim, err := json.Marshal(web.WSMessage{Type: "job_progress", Payload: newJobProgressFrame(job)})
	if err != nil {
		t.Fatalf("marshal job_progress: %v", err)
	}
	t.Logf("job_update %d B -> job_progress %d B (%.1f%%)", len(full), len(slim),
		100*float64(len(slim))/float64(len(full)))

	if len(slim) > 400 {
		t.Errorf("job_progress frame is %d B; the tick's mutable fields fit in well under 400 B — "+
			"something immutable is riding along", len(slim))
	}
	if len(slim)*10 > len(full) {
		t.Errorf("job_progress (%d B) is not an order of magnitude under job_update (%d B) — at 60 Hz "+
			"per dashboard per job that is the whole point", len(slim), len(full))
	}
}

// TestProgressFrameCarriesEveryFieldTheCardAndDialogRead pins the wire keys.
// They must be spelled exactly as database.Job spells them, because the client
// merges the frame onto the held row with an object spread: a renamed key
// would ADD a property instead of updating one, and the card would freeze on
// its first value while the row silently grew a twin field.
//
// THE MUTANT: drop totalVideoSeq/totalAudioSeq from the frame — the details
// dialog's "V: 12345 / 20000" totals freeze mid-download.
func TestProgressFrameCarriesEveryFieldTheCardAndDialogRead(t *testing.T) {
	raw, err := json.Marshal(newJobProgressFrame(realisticProgressJob()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{
		"id", "status", "progress", "percent", "speed", "eta",
		"lastVideoSeq", "lastAudioSeq", "totalVideoSeq", "totalAudioSeq",
		"totalChatMessages", "updatedAt",
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("job_progress frame has no %q — the client's spread merge leaves the held row's "+
				"stale value in place", k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("job_progress frame has %d keys, want exactly %d (%v) — an extra key is bytes on the "+
			"60 Hz path: got %v", len(got), len(want), want, got)
	}

	// The keys must match database.Job's own tags, or the merge writes twins.
	var row map[string]any
	rowRaw, _ := json.Marshal(realisticProgressJob())
	json.Unmarshal(rowRaw, &row)
	for _, k := range want {
		if _, ok := row[k]; !ok {
			t.Errorf("database.Job does not marshal a %q key — the frame and the row disagree on a name", k)
		}
	}
}

// TestOnlyProgressColumnsTakeTheSlimFrame is the classifier.
//
// THE MUTANTS:
//   - add "status" to progressOnlyColumns: a Finished transition arrives as a
//     progress frame, the client never re-sorts or re-evaluates the archive
//     boundary, and the row keeps its old badge.
//   - return true for an empty change set: a change we cannot classify is sent
//     as a progress frame and every non-listed field goes stale.
//   - forget total_video_seq / total_audio_seq: every real tick is classified
//     as a transition and the whole change is inert.
func TestOnlyProgressColumnsTakeTheSlimFrame(t *testing.T) {
	cases := []struct {
		name    string
		changes []string
		want    bool
	}{
		{"a full progress tick", []string{
			"progress", "percent", "speed", "total_chat_messages",
			"last_video_seq", "total_video_seq", "last_audio_seq", "total_audio_seq", "eta",
		}, true},
		{"an activity-message tick", []string{"progress", "speed", "eta"}, true},
		{"a chat-count-only tick", []string{"total_chat_messages"}, true},
		{"a status transition", []string{"status"}, false},
		{"a status transition riding with progress", []string{"progress", "status"}, false},
		{"an error being recorded", []string{"error"}, false},
		{"a chat_status change", []string{"chat_status"}, false},
		{"a membership park", []string{"status", "park_reason", "park_identity"}, false},
		{"the mux naming its output", []string{"output_file", "filename", "file_size"}, false},
		{"an unclassifiable empty set", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProgressOnlyChange(tc.changes); got != tc.want {
				t.Errorf("isProgressOnlyChange(%v) = %v, want %v", tc.changes, got, tc.want)
			}
		})
	}
}

// TestJobChangeSubscriberRoutesProgressTicksToTheSlimFrame pins the CALL SITE.
// Driving the real subscriber needs a whole runState, so this reads the source
// the way internal/web/routes' call-site pins do.
//
// THE MUTANT: the helpers exist, the tests above are green, and nobody calls
// them — the whole row still goes out 60 times a second.
func TestJobChangeSubscriberRoutesProgressTicksToTheSlimFrame(t *testing.T) {
	src, err := os.ReadFile("monitor_callbacks.go")
	if err != nil {
		t.Fatalf("read monitor_callbacks.go: %v", err)
	}
	text := string(src)
	for _, want := range []string{
		"if isProgressOnlyChange(ev.Changes) {",
		"s.wsHub.BroadcastJobProgress(newJobProgressFrame(job))",
		"s.wsHub.BroadcastJobUpdate(job)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the OnJobChange subscriber does not contain %q — progress ticks are not being "+
				"routed to the slim frame", want)
		}
	}
}

// downloadTickChangeSets is what the ~60 Hz writer actually emits over an
// archive's life, in the order a download produces them
// (internal/worker/progress.go): the chat-only tick before either media
// stream has reported, the video-only tick, the full tick once both report,
// and the activity-message tick that blanks speed and eta during a stall.
func downloadTickChangeSets(ticks int) [][]string {
	shapes := [][]string{
		{"progress", "percent", "speed", "total_chat_messages"},
		{"progress", "percent", "speed", "total_chat_messages", "last_video_seq", "total_video_seq"},
		{"progress", "percent", "speed", "total_chat_messages",
			"last_video_seq", "total_video_seq", "last_audio_seq", "total_audio_seq", "eta"},
		{"progress", "speed", "eta"},
		{"total_chat_messages"},
	}
	out := make([][]string, 0, ticks)
	for i := range ticks {
		out = append(out, shapes[i%len(shapes)])
	}
	return out
}

// TestEveryProgressTickStillProducesExactlyOneFrame is the cadence
// differential the protected ruling demands: this change makes each update
// CHEAPER, never RARER. Before it, the OnJobChange subscriber broadcast once —
// unconditionally — per JobChange; after it, every change set a progress tick
// produces must take the slim branch, so the same tick that emitted one
// job_update emits exactly one job_progress. One frame in, one frame out, at
// the same 60 Hz.
//
// The second half pins the dispatch as a CONTIGUOUS block: each branch holds
// exactly one broadcast call, with nothing between the classifier and the send
// where a throttle, a coalescer or a "skip if unchanged" early return could
// swallow a tick. The per-call-count half of the differential lives in
// internal/web (TestBroadcastJobProgressIsOneFramePerCallLikeJobUpdate), where
// the hub's own queue can be counted.
//
// THE MUTANTS:
//   - drop total_video_seq (or any other tick column) from
//     progressOnlyColumns: those ticks fall through to job_update and the
//     whole change is inert — the first half fails, naming the tick shape.
//   - insert a throttle or a coalescer into the dispatch block (a time.Since
//     gate, a bare `return` on an unchanged percent): a tick that used to emit
//     one frame now emits zero — the contiguous-block assertion fails.
func TestEveryProgressTickStillProducesExactlyOneFrame(t *testing.T) {
	ticks := downloadTickChangeSets(60)
	slim := 0
	for i, changes := range ticks {
		if !isProgressOnlyChange(changes) {
			t.Errorf("tick %d (%v) is classified as a transition — it would still send the whole row, "+
				"and the change is inert for that tick shape", i, changes)
			continue
		}
		slim++
	}
	if slim != len(ticks) {
		t.Errorf("%d of %d download ticks take the slim frame; every one of them must, or the 60 Hz "+
			"path still carries the full row", slim, len(ticks))
	}

	src, err := os.ReadFile("monitor_callbacks.go")
	if err != nil {
		t.Fatalf("read monitor_callbacks.go: %v", err)
	}
	dispatch := "\t\tif isProgressOnlyChange(ev.Changes) {\n" +
		"\t\t\ts.wsHub.BroadcastJobProgress(newJobProgressFrame(job))\n" +
		"\t\t\treturn\n" +
		"\t\t}\n" +
		"\t\ts.wsHub.BroadcastJobUpdate(job)\n"
	if !strings.Contains(string(src), dispatch) {
		t.Errorf("the OnJobChange dispatch is not the exact block\n%s\n— anything inserted between the "+
			"classifier and the broadcast can drop or coalesce a tick, and the cadence is protected "+
			"(cheaper, never rarer)", dispatch)
	}
}
