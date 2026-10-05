package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// installOffsetAwareVodCommentStub serves pages like installVodCommentStub,
// but answers an OFFSET request the way the real API does: with the first
// page at or after that offset (a cursor "cur<p>-<i>" still asks for p+1).
func installOffsetAwareVodCommentStub(t *testing.T, pages []vodCommentPageSpec) *[]string {
	t.Helper()
	var mu sync.Mutex
	asked := []string{}
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var q struct {
			Variables struct {
				ContentOffsetSeconds *float64 `json:"contentOffsetSeconds"`
				Cursor               *string  `json:"cursor"`
			} `json:"variables"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		page := -1
		mu.Lock()
		if q.Variables.Cursor != nil && *q.Variables.Cursor != "" {
			asked = append(asked, "cursor:"+*q.Variables.Cursor)
			var p, i int
			fmt.Sscanf(*q.Variables.Cursor, "cur%d-%d", &p, &i)
			page = p + 1
		} else if q.Variables.ContentOffsetSeconds != nil {
			off := *q.Variables.ContentOffsetSeconds
			asked = append(asked, fmt.Sprintf("offset:%v", off))
			for i, p := range pages {
				if p.offset >= off {
					page = i
					break
				}
			}
		}
		mu.Unlock()
		if page < 0 || page >= len(pages) {
			return nil, fmt.Errorf("stub: no page")
		}
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: h,
			Body: io.NopCloser(bytes.NewReader([]byte(vodCommentsReply(page, pages[page])))), Request: req}, nil
	})}
	return &asked
}

// readVodChatIDs parses a VOD chat file and returns its header count and IDs.
func readVodChatIDs(t *testing.T, path string) (header int, ids []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d TwitchChatData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("the VOD chat file does not parse: %v", err)
	}
	for _, m := range d.Messages {
		ids = append(ids, m.ID)
	}
	return d.MessageCount, ids
}

func countDistinct(ids []string) int {
	seen := map[string]struct{}{}
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	return len(seen)
}

func newVodChatForTest(out string) *VodChatDownloader {
	return NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{VodID: "v1", OutputPath: out}, &testLogger{})
}

// TestAFinishedVodChatRerunAppendsNothing: a VOD chat that finished removes
// its sidecar, and a restart that re-processes the job (its video still
// downloading) ran the chat again against the same file from offset 0. The
// dedup knew only the file's last 5000 IDs, so everything else was appended
// a second time — all 12000 of them here. The run now continues from the
// file's newest message.
//
// Mutant: drop `contentOffset = fileOffset` — the file holds 24000 records.
func TestAFinishedVodChatRerunAppendsNothing(t *testing.T) {
	pages := []vodCommentPageSpec{
		{count: 3000, offset: 100, hasNext: true},
		{count: 3000, offset: 200, hasNext: true},
		{count: 3000, offset: 300, hasNext: true},
		{count: 3000, offset: 400, hasNext: false},
	}
	installOffsetAwareVodCommentStub(t, pages)
	out := filepath.Join(t.TempDir(), "chat.json")
	if err := newVodChatForTest(out).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out + ".resume.json"); !os.IsNotExist(err) {
		t.Fatalf("precondition: the finished run removed its sidecar (stat err %v)", err)
	}
	if err := newVodChatForTest(out).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 12000 || countDistinct(ids) != 12000 || header != 12000 {
		t.Errorf("file holds %d records (%d distinct, header %d), want each of the 12000 once", len(ids), countDistinct(ids), header)
	}
}

// TestVodSidecarStaysAtTheLastGoodFlush: the sidecar was saved after every
// flush whether or not it wrote, so a failed one recorded an offset and IDs
// past messages only memory held, and the next run never asked for those
// pages again. checkpoint saves only after a good flush.
//
// Mutant: save the sidecar in checkpoint after a failed flush — pages 1 and 2
// never reach the file.
func TestVodSidecarStaysAtTheLastGoodFlush(t *testing.T) {
	pages := []vodCommentPageSpec{
		{count: 3, offset: 100, hasNext: true},
		{count: 3, offset: 150, hasNext: true},
		{count: 3, offset: 200, hasNext: true},
		{count: 3, offset: 300, hasNext: false},
	}
	installOffsetAwareVodCommentStub(t, pages)
	out := filepath.Join(t.TempDir(), "chat.json")
	prev := newVodChatForTest(out)
	add := func(page int, off float64) {
		for i := range 3 {
			id := fmt.Sprintf("c%d-%d", page, i)
			prev.dedup.Add(id)
			prev.messages = append(prev.messages, TwitchChatMessage{ID: id, OffsetMs: int64(off * 1000), Message: "hello", MessageType: "chat"})
			prev.totalCount.Add(1)
		}
	}
	add(0, 100)
	prev.checkpoint(100)
	add(1, 150)
	add(2, 200)
	// chat.json briefly cannot be written (a stand-in for a Windows lock).
	if err := os.Rename(out, out+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	prev.finishInterrupted(200)
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(out+".bak", out); err != nil {
		t.Fatal(err)
	}

	if err := newVodChatForTest(out).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 12 || countDistinct(ids) != 12 || header != 12 {
		t.Errorf("file holds %v (header %d), want all 12 comments once", ids, header)
	}
}

// TestAVodChatOverADamagedFileIsSalvaged: a crash-torn VOD chat file made
// every flush fail, and the completion path ignored that — the chat was lost
// while the job read "finished". Start now salvages the file first.
//
// Mutant: skip repairDamagedFile — the run's flushes fail and Start reports
// the failed final flush.
func TestAVodChatOverADamagedFileIsSalvaged(t *testing.T) {
	pages := []vodCommentPageSpec{
		{count: 3, offset: 900, hasNext: true},
		{count: 3, offset: 1000, hasNext: false},
	}
	installOffsetAwareVodCommentStub(t, pages)
	out := filepath.Join(t.TempDir(), "chat.json")
	prev := newVodChatForTest(out)
	for i := range 4 {
		id := fmt.Sprintf("old%d", i)
		prev.dedup.Add(id)
		prev.messages = append(prev.messages, TwitchChatMessage{ID: id, OffsetMs: 100_000, Message: "hello", MessageType: "chat"})
		prev.totalCount.Add(1)
	}
	prev.checkpoint(100)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	damaged := append(raw, make([]byte, 600)...)
	if err := os.WriteFile(out, damaged, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := newVodChatForTest(out).Start(context.Background()); err != nil {
		t.Fatalf("Start over a damaged file: %v", err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 10 || header != 10 {
		t.Errorf("file holds %v (header %d), want the 4 old comments and the 6 fetched", ids, header)
	}
	if kept, err := os.ReadFile(out + chatCorruptSuffix); err != nil || !bytes.Equal(kept, damaged) {
		t.Errorf("the damaged original was not kept as .corrupt (err %v)", err)
	}
}

// seedVodChatBelowSidecar leaves what a run that checkpointed three pages
// (c0-* at 100 s, c1-* at 150 s, c2-* at 200 s) leaves on disk — a sidecar at
// offset 200 covering nine comments — and then cuts the file inside c1-0, the
// way a crash tears it: the salvage can keep only c0-*.
func seedVodChatBelowSidecar(t *testing.T, out string) {
	t.Helper()
	prev := newVodChatForTest(out)
	for page, off := range []float64{100, 150, 200} {
		for i := range 3 {
			id := fmt.Sprintf("c%d-%d", page, i)
			prev.dedup.Add(id)
			prev.messages = append(prev.messages, TwitchChatMessage{ID: id, OffsetMs: int64(off * 1000), Message: "hello", MessageType: "chat"})
			prev.totalCount.Add(1)
		}
		prev.checkpoint(off)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(raw, []byte(`"c1-0"`))
	if i < 0 {
		t.Fatalf("no c1-0 in %s", raw)
	}
	if err := os.WriteFile(out, raw[:i+3], 0o644); err != nil {
		t.Fatal(err)
	}
}

// vodSalvageTestPages is the VOD seedVodChatBelowSidecar's run was paging.
var vodSalvageTestPages = []vodCommentPageSpec{
	{count: 3, offset: 100, hasNext: true},
	{count: 3, offset: 150, hasNext: true},
	{count: 3, offset: 200, hasNext: true},
	{count: 3, offset: 300, hasNext: false},
}

// TestAVodSalvageBelowTheSidecarRefetchesWhatTheDamageTook: the resume
// restored the sidecar's offset, count and IDs before the damaged file was
// salvaged, and the file's own offset could only move the run FORWARD — so a
// salvage that kept less than the sidecar covered resumed from the sidecar's
// offset with its dedup, never asked for the pages the damage took, and Start
// returned nil over six of twelve comments. The run now continues from the
// salvaged file.
//
// Mutant: drop the `salvaged || fileGone` reset in Start — the run asks for
// offset 200 and the file holds 6.
func TestAVodSalvageBelowTheSidecarRefetchesWhatTheDamageTook(t *testing.T) {
	asked := installOffsetAwareVodCommentStub(t, vodSalvageTestPages)
	out := filepath.Join(t.TempDir(), "chat.json")
	seedVodChatBelowSidecar(t, out)

	vcd := newVodChatForTest(out)
	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(*asked) == 0 || (*asked)[0] != "offset:100" {
		t.Errorf("the run entered the VOD by %v, want offset:100 — the salvaged file's newest comment", *asked)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 12 || countDistinct(ids) != 12 || header != 12 {
		t.Errorf("file holds %v (header %d), want all 12 comments once", ids, header)
	}
	if got := vcd.MessageCount(); got != 12 {
		t.Errorf("MessageCount %d, want 12", got)
	}
}

// TestAVodSidecarWithoutItsFileStartsOver: a sidecar whose chat file is gone
// restored a count and IDs for messages no longer on disk, so the header
// counted six over an array of three and the first page — dropped as already
// seen — was never fetched again.
//
// Mutant: drop `fileGone` from the reset in Start — header 6 over c1-* alone.
func TestAVodSidecarWithoutItsFileStartsOver(t *testing.T) {
	installOffsetAwareVodCommentStub(t, []vodCommentPageSpec{
		{count: 3, offset: 100, hasNext: true},
		{count: 3, offset: 200, hasNext: false},
	})
	out := filepath.Join(t.TempDir(), "chat.json")
	prev := newVodChatForTest(out)
	for i := range 3 {
		id := fmt.Sprintf("c0-%d", i)
		prev.dedup.Add(id)
		prev.messages = append(prev.messages, TwitchChatMessage{ID: id, OffsetMs: 100_000, Message: "hello", MessageType: "chat"})
		prev.totalCount.Add(1)
	}
	prev.checkpoint(100)
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}

	if err := newVodChatForTest(out).Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 6 || countDistinct(ids) != 6 || header != 6 {
		t.Errorf("file holds %v (header %d), want both pages' 6 comments", ids, header)
	}
}

// TestAVodSalvageMovesTheSidecarAtOnce: after the salvage the old sidecar
// still pointed past what the file holds, and the file now read intact — so a
// process killed before the run's first checkpoint resumed from that sidecar
// next time and lost the same pages after all. Start saves the sidecar as soon
// as it has continued from the salvaged file.
//
// Mutant: drop the saveResumeState after the adoption in Start — the second
// run resumes at offset 200 and the file holds 6.
func TestAVodSalvageMovesTheSidecarAtOnce(t *testing.T) {
	orig := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = orig })
	out := filepath.Join(t.TempDir(), "chat.json")
	seedVodChatBelowSidecar(t, out)

	// The process dies on the first page fetch: a panic unwinds Start with no
	// checkpoint at all, which is as much as a kill leaves behind.
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(*http.Request) (*http.Response, error) {
		panic("killed")
	})}
	if err := newVodChatForTest(out).Start(context.Background()); err == nil {
		t.Fatal("precondition: the first run must die on its first page")
	}

	installOffsetAwareVodCommentStub(t, vodSalvageTestPages)
	if err := newVodChatForTest(out).Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 12 || countDistinct(ids) != 12 || header != 12 {
		t.Errorf("file holds %v (header %d), want all 12 comments once", ids, header)
	}
}

// TestAVodRestartOfTheSameDownloaderDoesNotDuplicate: the orchestrator
// re-Starts the same VodChatDownloader after a connectivity outage. A run
// whose final flush failed kept its batch in memory while the sidecar stayed
// at the last good flush; the re-Start restored that sidecar's count and
// dedup, paged again from its offset and buffered the same comments behind
// the first copy — 13 records, 7 distinct, under a header of 7.
//
// Mutant: drop `vcd.messages = nil` at the top of Start — 13 records.
func TestAVodRestartOfTheSameDownloaderDoesNotDuplicate(t *testing.T) {
	installOffsetAwareVodCommentStub(t, []vodCommentPageSpec{
		{count: 3, offset: 100, hasNext: true},
		{count: 3, offset: 200, hasNext: false},
	})
	out := filepath.Join(t.TempDir(), "chat.json")
	prev := newVodChatForTest(out)
	prev.dedup.Add("x0")
	prev.messages = append(prev.messages, TwitchChatMessage{ID: "x0", OffsetMs: 10_000, Message: "hello", MessageType: "chat"})
	prev.totalCount.Add(1)
	prev.checkpoint(10)

	real := appendChatMessages
	t.Cleanup(func() { appendChatMessages = real })
	appendChatMessages = func(string, []TwitchChatMessage, int, utils.ChatFileLogger) error {
		return fmt.Errorf("%w: disk full", utils.ErrChatFilePartialWrite)
	}
	vcd := newVodChatForTest(out)
	if err := vcd.Start(context.Background()); err == nil {
		t.Fatal("precondition: the first run's final flush must fail")
	}
	appendChatMessages = real
	if err := vcd.Start(context.Background()); err != nil { // the relaunch, same instance
		t.Fatalf("Start again: %v", err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 7 || countDistinct(ids) != 7 || header != 7 {
		t.Errorf("file holds %v (header %d), want x0 and the 6 fetched once each", ids, header)
	}
}

// TestAVodRestartWithoutASidecarFetchesTheDroppedBatchAgain: the same
// relaunch where no flush ever wrote — so no sidecar replaces anything — must
// not keep the dropped batch's IDs or count: kept, every comment fetched again
// was filtered as already seen and the file never got any, or the header
// counted each one twice.
//
// Mutants: drop `vcd.dedup.Restore(nil)` at the top of Start — no file is
// ever written; drop `vcd.totalCount.Store(0)` — header 12 over 6.
func TestAVodRestartWithoutASidecarFetchesTheDroppedBatchAgain(t *testing.T) {
	installOffsetAwareVodCommentStub(t, []vodCommentPageSpec{
		{count: 3, offset: 100, hasNext: true},
		{count: 3, offset: 200, hasNext: false},
	})
	out := filepath.Join(t.TempDir(), "chat.json")
	// A directory where the chat file belongs: nothing the first run writes
	// can land, so it ends holding the whole VOD's chat in memory.
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	vcd := newVodChatForTest(out)
	if err := vcd.Start(context.Background()); err == nil {
		t.Fatal("precondition: the first run's final flush must fail")
	}
	if _, err := os.Stat(out + ".resume.json"); !os.IsNotExist(err) {
		t.Fatalf("precondition: no flush wrote, so no sidecar (stat err %v)", err)
	}
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if err := vcd.Start(context.Background()); err != nil { // the relaunch, same instance
		t.Fatalf("Start again: %v", err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 6 || countDistinct(ids) != 6 || header != 6 {
		t.Errorf("file holds %v (header %d), want both pages' 6 comments once", ids, header)
	}
}

// TestAVodChatThatFetchesNothingStillRepairsItsFile: the flush salvages a
// damaged file only when there is something to write. A run whose remaining
// pages are empty never flushes, so without the repair at Start it finished
// over a file that does not parse.
//
// Mutant: skip repairDamagedFile — the file is left torn.
func TestAVodChatThatFetchesNothingStillRepairsItsFile(t *testing.T) {
	installOffsetAwareVodCommentStub(t, []vodCommentPageSpec{{count: 0, offset: 900, hasNext: false}})
	out := filepath.Join(t.TempDir(), "chat.json")
	prev := newVodChatForTest(out)
	for i := range 4 {
		prev.messages = append(prev.messages, TwitchChatMessage{ID: fmt.Sprintf("old%d", i), OffsetMs: 100_000, Message: "hello", MessageType: "chat"})
		prev.totalCount.Add(1)
	}
	prev.checkpoint(100)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(raw, make([]byte, 600)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := newVodChatForTest(out).Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if header, ids := readVodChatIDs(t, out); len(ids) != 4 || header != 4 {
		t.Errorf("file holds %v (header %d), want the 4 intact comments", ids, header)
	}
}

// TestAVodFinalFlushFailureIsReported: the completion path flushed, ignored a
// failure, removed the sidecar and returned nil — "finished" over a chat
// missing its tail. It now returns the failure (chat_status "incomplete").
//
// Mutant: ignore the final flush's error again — Start returns nil.
func TestAVodFinalFlushFailureIsReported(t *testing.T) {
	installOffsetAwareVodCommentStub(t, []vodCommentPageSpec{{count: 3, offset: 10, hasNext: false}})
	out := filepath.Join(t.TempDir(), "chat.json")
	// A directory where the chat file belongs: no write can land there.
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	err := newVodChatForTest(out).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "final flush failed") {
		t.Fatalf("Start = %v, want the failed final flush", err)
	}
}

// TestAPartialVodAppendKeepsTheBatch: an append whose write failed puts the
// file's end back and holds none of the batch, which the VOD writer dropped
// while totalCount kept it — the header over-counted the array for good.
//
// Mutant: treat ErrChatFilePartialWrite as written again — c3..c5 are lost
// and the header says 9 over an array of 6.
func TestAPartialVodAppendKeepsTheBatch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	vcd := newVodChatForTest(out)
	add := func(from, to int) {
		for i := from; i < to; i++ {
			vcd.messages = append(vcd.messages, TwitchChatMessage{ID: fmt.Sprintf("c%d", i), OffsetMs: int64(i) * 1000, Message: "hello", MessageType: "chat"})
			vcd.totalCount.Add(1)
		}
	}
	add(0, 3)
	if err := vcd.flush(); err != nil {
		t.Fatal(err)
	}
	real := appendChatMessages
	t.Cleanup(func() { appendChatMessages = real })
	appendChatMessages = func(string, []TwitchChatMessage, int, utils.ChatFileLogger) error {
		return fmt.Errorf("%w: file too large", utils.ErrChatFilePartialWrite)
	}
	add(3, 6)
	if err := vcd.flush(); !errors.Is(err, utils.ErrChatFilePartialWrite) {
		t.Fatalf("flush during the failure = %v", err)
	}
	appendChatMessages = real
	add(6, 9)
	if err := vcd.flush(); err != nil {
		t.Fatal(err)
	}
	header, ids := readVodChatIDs(t, out)
	if len(ids) != 9 || header != 9 {
		t.Errorf("file holds %v (header %d), want all 9", ids, header)
	}
}
