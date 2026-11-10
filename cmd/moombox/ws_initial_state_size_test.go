package main

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// snapshotVocabulary is the word list the measured rows' titles and
// descriptions are drawn from: a few hundred distinct words, links and tags,
// so the text deflates about as natural-language stream descriptions do. One
// repeated sentence would deflate to almost nothing and flatter the
// measurement.
var snapshotVocabulary = func() []string {
	base := strings.Fields(`the a to and of in for on with this that stream live today tonight
		welcome thanks everyone watching chat members membership join subscribe follow schedule
		playlist music song karaoke game gaming play playing part episode final ending chapter
		talk talking zatsudan morning evening night collab guest special anniversary birthday
		celebration announcement new release cover original album concert stage outfit reveal
		drawing art artist illustrator rigger model credit credits bgm opening thumbnail
		rules please be nice do not spam respect others language english japanese only topic
		twitter discord merch store shipping goods limited voice pack donation superchat
		thank you support hashtag fanart clip clips allowed guidelines official channel
		minecraft horror puzzle rhythm racing story adventure retro speedrun challenge ranked`)
	words := append([]string{}, base...)
	for i := 0; i < 60; i++ {
		words = append(words,
			fmt.Sprintf("https://example.com/%c%d/watch?v=%08x", 'a'+i%26, i, i*2654435761),
			fmt.Sprintf("#tag%d", i), fmt.Sprintf("@creator_%d", i))
	}
	return words
}()

// snapshotText is n bytes of space- and newline-separated vocabulary.
func snapshotText(rng *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(snapshotVocabulary[rng.Intn(len(snapshotVocabulary))])
		if rng.Intn(12) == 0 {
			b.WriteByte('\n')
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()[:n]
}

// snapshotJobs is n job rows shaped like a 24/7 recorder's Tasks list: a few
// captures in flight, a backlog of Queued rows, and the Finished rows inside
// the hide window, each with every column a real row of its kind carries.
// Description lengths follow the mix the code's own comments describe
// (YouTube's run to 5 KB, job_progress.go): a fifth of the rows have none or
// a line, most have one to three kilobytes, a fifth are at the cap.
func snapshotJobs(n int) []*database.Job {
	rng := rand.New(rand.NewSource(1))
	descLens := []int{0, 120, 900, 1500, 2200, 3000, 5000, 5000, 1800, 600}
	ip := func(v int) *int { return &v }
	jobs := make([]*database.Job, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", rng.Uint32(), i, i%4096, i%4096, rng.Int63n(1<<48))
		vid := fmt.Sprintf("v%010d", i)
		ch := fmt.Sprintf("UC%022d", i%25)
		j := &database.Job{
			ID: id, VideoID: vid,
			URL:               "https://www.youtube.com/watch?v=" + vid,
			Title:             snapshotText(rng, 40+rng.Intn(50)),
			ChannelName:       snapshotText(rng, 12+rng.Intn(12)),
			Platform:          "youtube",
			CreatedAt:         "2026-11-01T12:00:00Z",
			UpdatedAt:         "2026-11-01T15:42:07Z",
			ThumbnailURL:      "https://i.ytimg.com/vi/" + vid + "/maxresdefault.jpg",
			Description:       snapshotText(rng, descLens[i%len(descLens)]),
			ChannelID:         &ch,
			QualityPreference: "best",
			QueuePriority:     1,
			ChannelAvatarURL:  "https://yt3.ggpht.com/" + snapshotText(rng, 60),
		}
		switch {
		case i%50 == 0:
			j.Status = database.StatusDownloading
			j.Progress = "(V: 12345/20000 A: 12345/20000 C: 5678)"
			j.Percent, j.Speed, j.ETA = 61.7, "4.2 MiB/s", "00:12:34"
			j.LastVideoSeq, j.LastAudioSeq, j.TotalVideoSeq, j.TotalAudioSeq = ip(12345), ip(12345), ip(20000), ip(20000)
			j.StreamStartTime, j.DownloadStartedAt = "2026-11-01T12:00:00Z", "2026-11-01T12:00:05Z"
		case i%3 == 0:
			j.Status, j.IsVod = database.StatusQueued, true
			j.LengthSeconds = ip(7200 + i)
		default:
			size := int64(3_500_000_000 + i)
			dir := filepath.Join(`D:\Archive`, j.ChannelName)
			file := "[20261101] " + j.Title + " (" + vid + ").mp4"
			j.Status, j.Percent = database.StatusFinished, 100
			j.Progress = "(V: 20000/20000 A: 20000/20000 C: 9876)"
			j.OutputDirectory, j.Filename, j.OutputFile = dir, file, filepath.Join(dir, file)
			j.ChatFilename, j.ChatFile = file+".chat.json", filepath.Join(dir, file+".chat.json")
			j.ThumbnailFile = filepath.Join(dir, file+".jpg")
			j.DescriptionFile = filepath.Join(dir, file+".description.txt")
			j.VideoWidth, j.VideoHeight, j.VideoFps, j.FileSize = ip(1920), ip(1080), ip(60), &size
			j.TotalChatMessages, j.LengthSeconds = ip(9876), ip(7200+i)
			j.StreamStartTime, j.StreamEndTime = "2026-11-01T12:00:00Z", "2026-11-01T14:00:00Z"
			j.ChatStatus = database.ChatFinished
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// snapshotFrame is the initial_state frame for jobs as the hub marshals it
// (WebSocketHub.initialStateBytes): the jobs as listed rows inside the
// message envelope. The other keys of the real frame are small or bounded
// (the log ring, three timestamps) and are left out of the measurement.
func snapshotFrame(t *testing.T, jobs []*database.Job) []byte {
	t.Helper()
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	raw, err := json.Marshal(web.WSMessage{Type: "initial_state", Payload: map[string]any{
		"jobs": listedJobs(store, jobs),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// deflated is the size of raw after permessage-deflate as the WebSocket
// library applies it: one compress/flate stream at BestSpeed per message.
func deflated(t *testing.T, raw []byte) int {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Len()
}

// TestInitialStateSnapshotSize is the measurement finding web-9 asked for
// before anything was changed (owner ruling R-B4): how large is the
// initial_state frame a dashboard is sent on every connect and on every
// resync, and what does per-message compression make of it. Run with -v for
// the figures.
//
// What it found, on rows shaped as snapshotJobs describes: about 3.5 KB a
// row, 1.7 MB for a 500-row Tasks list and 7.0 MB for 2,000 rows, with the
// descriptions 57% of it; deflate at the library's level leaves 17%. The
// rows are generated, not read from a real database, and their text is
// drawn from a few hundred words, so real descriptions will deflate less
// well than that — expect a third rather than a sixth. Either way the frame
// is large and mostly text, and the hub negotiates permessage-deflate
// (web.WebSocketHub.HandleUpgrade). The wire shape is unchanged: every
// field, description included, is still in the frame.
//
// The assertions are loose bounds on those figures, so that a change which
// makes the snapshot much heavier per row, or a row shape that no longer
// compresses, is noticed here and re-measured.
func TestInitialStateSnapshotSize(t *testing.T) {
	for _, n := range []int{500, 2000} {
		jobs := snapshotJobs(n)
		raw := snapshotFrame(t, jobs)

		desc := 0
		for _, j := range jobs {
			desc += len(j.Description)
		}
		small := deflated(t, raw)
		perRow := len(raw) / n
		t.Logf("%d rows: %d bytes (%d a row), descriptions %d%%, deflated %d bytes (%d%%)",
			n, len(raw), perRow, desc*100/len(raw), small, small*100/len(raw))

		if perRow > 5000 {
			t.Errorf("%d rows: %d bytes a row, want at most 5000: the snapshot grew, measure it again", n, perRow)
		}
		if small*2 > len(raw) {
			t.Errorf("%d rows: deflate leaves %d of %d bytes, want under half", n, small, len(raw))
		}

		// The frame still carries every row whole: compression is the only
		// remedy the ruling allows, never a trimmed field.
		var back struct {
			Payload struct {
				Jobs []map[string]any `json:"jobs"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if len(back.Payload.Jobs) != n {
			t.Fatalf("%d rows in the frame, want %d", len(back.Payload.Jobs), n)
		}
		for i, row := range back.Payload.Jobs {
			if got, _ := row["description"].(string); got != jobs[i].Description {
				t.Fatalf("row %d: the frame's description is not the row's", i)
			}
		}
	}
}

// TestProgressFramesStayUnderTheCompressionThreshold keeps the ~60 Hz
// job_progress frame out of the compressor: the hub deflates only a message
// of web.WSCompressionThreshold bytes or more, so a progress tick still costs
// one small write and no flate pass per dashboard. A frame that grows past
// the threshold would be compressed sixty times a second per active job per
// client.
func TestProgressFramesStayUnderTheCompressionThreshold(t *testing.T) {
	raw, err := json.Marshal(web.WSMessage{Type: "job_progress", Payload: newJobProgressFrame(realisticProgressJob())})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("job_progress frame: %d bytes, compression threshold %d", len(raw), web.WSCompressionThreshold)
	if len(raw)*2 > web.WSCompressionThreshold {
		t.Errorf("a job_progress frame is %d bytes, want at most half the %d-byte compression threshold",
			len(raw), web.WSCompressionThreshold)
	}
}
