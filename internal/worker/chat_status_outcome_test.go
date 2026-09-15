package worker

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestChatStatusForOutcome pins the rule O2 is about: the verdict is what the
// downloader DID, not what it counted.
//
// Mutants this kills:
//   - checking the count first (today's shape): a stalled VOD chat with 5,000
//     messages on disk reads "finished", both UIs show a short archive as
//     complete, and nothing tells the operator to act.
//   - dropping the outcome arm entirely: same result.
//   - reporting "unavailable" for a stall that captured nothing: the archive
//     did not turn out to be empty, the capture stopped — the row says so.
func TestChatStatusForOutcome(t *testing.T) {
	stalled := errors.New("vod chat paging stalled at offset 12 cursor \"abc\": cursor did not advance")
	for _, tc := range []struct {
		name    string
		count   int
		outcome error
		want    string
	}{
		{"clean run with messages", 5000, nil, "finished"},
		{"clean run with no chat at all", 0, nil, "unavailable"},
		{"stalled run with messages", 5000, stalled, "incomplete"},
		{"stalled run with no messages", 0, stalled, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatStatusForOutcome(tc.count, tc.outcome); got != tc.want {
				t.Errorf("chatStatusForOutcome(%d, %v) = %q, want %q", tc.count, tc.outcome, got, tc.want)
			}
		})
	}
}

// TestChatOutcomeKeepsTheLastRunsVerdict pins the relaunch rule. The Twitch
// orchestrator relaunches chat after a connectivity outage, so one job can run
// several chat sessions; the job's verdict belongs to the LAST of them.
//
// Mutant: a recorder that keeps the FIRST error — a job whose chat recovered
// after an outage would still be reported incomplete forever.
func TestChatOutcomeKeepsTheLastRunsVerdict(t *testing.T) {
	var rec chatOutcome
	if rec.verdict() != nil {
		t.Fatalf("a recorder that has seen nothing has verdict %v, want nil", rec.verdict())
	}
	first := errors.New("connectivity lost")
	rec.record(first)
	if !errors.Is(rec.verdict(), first) {
		t.Fatalf("verdict = %v, want the recorded error", rec.verdict())
	}
	rec.record(nil)
	if rec.verdict() != nil {
		t.Errorf("verdict = %v after a later run succeeded, want nil — the relaunch's outcome is "+
			"the job's outcome", rec.verdict())
	}
}

// TestChatFileStatusDoesNotOverwriteAnIncompleteVerdict pins the mux path.
//
// copyAssets and the per-part chat block both set chat_status to "finished"
// whenever they COPY a chat file, and they run after the verdict is written —
// so without this guard the fix would be undone at mux time by a file that is
// short precisely because the capture stalled.
//
// Mutant: the unconditional `"finished"` those two sites carried before.
func TestChatFileStatusDoesNotOverwriteAnIncompleteVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		jobCtx *JobContext
		want   string
	}{
		{"incomplete verdict survives the copy", &JobContext{ChatStatus: "incomplete"}, "incomplete"},
		{"no verdict recorded", &JobContext{}, "finished"},
		{"unavailable verdict still yields to an archived file", &JobContext{ChatStatus: "unavailable"}, "finished"},
		{"finished verdict", &JobContext{ChatStatus: "finished"}, "finished"},
		{"no context at all (standalone Mux action)", nil, "finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatFileStatus(tc.jobCtx); got != tc.want {
				t.Errorf("chatFileStatus(%+v) = %q, want %q", tc.jobCtx, got, tc.want)
			}
		})
	}
}

// TestRecordChatOutcomeWritesTheRowAndRemembersItOnTheContext is the wiring:
// the verdict has to reach BOTH the job row (which is what the two UIs read)
// and the JobContext (which is what stops the mux path overwriting it).
//
// Mutant: writing the DB row without setting jobCtx.ChatStatus — the row is
// right until copyAssets runs, and then the job finishes reporting "finished".
func TestRecordChatOutcomeWritesTheRowAndRemembersItOnTheContext(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&database.Job{ID: "j1", VideoID: "v1", URL: "https://twitch.tv/videos/1", Platform: "twitch"}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	o := &DownloadOrchestrator{db: db, logger: discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: "j1"}}
	o.recordChatOutcome(jobCtx, 4211, errors.New("vod chat paging stalled"))

	if jobCtx.ChatStatus != "incomplete" {
		t.Errorf("jobCtx.ChatStatus = %q, want \"incomplete\" — the mux path reads this to avoid "+
			"overwriting the verdict when it copies the (short) chat file", jobCtx.ChatStatus)
	}
	job, err := db.GetJob("j1")
	if err != nil || job == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.ChatStatus != "incomplete" {
		t.Errorf("job.ChatStatus = %q, want \"incomplete\"", job.ChatStatus)
	}
	if job.TotalChatMessages == nil || *job.TotalChatMessages != 4211 {
		t.Errorf("job.TotalChatMessages = %v, want 4211 — the count is still recorded, it is just "+
			"no longer what decides the status", job.TotalChatMessages)
	}
}
