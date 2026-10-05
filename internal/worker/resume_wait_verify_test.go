package worker

import (
	"os"
	"strings"
	"testing"
	"time"
)

// active is the wait the loop may keep up: started, and inside its budget.
//
// Mutant: active ignoring the budget — the expired episode still reads active.
func TestWaitDeadlineActive(t *testing.T) {
	const budget = 10 * time.Minute
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var d waitDeadline
	if d.active(start, budget) {
		t.Error("an episode that never started is active")
	}
	d.exceeded(start, budget) // latches the start
	if !d.active(start.Add(time.Minute), budget) {
		t.Error("an episode inside its budget is not active")
	}
	if d.active(start.Add(budget), budget) {
		t.Error("an episode past its budget is still active")
	}
	d.reset()
	if d.active(start.Add(time.Minute), budget) {
		t.Error("a reset episode is still active")
	}
}

// The quality-loss branch waits for an interrupted broadcast once; the
// downloaders it cancelled then bring every later look to the stream-end
// verify branch, which spent a live check on each and so ended the wait after
// maxConsecutiveLiveChecks — about half an hour — however long
// interruption_timeout allowed, showing "verifying end" all the while. That
// branch now asks the same wait episode, refunds the check when it may keep
// waiting, and labels the sleep a wait. runLiveStreamDownload cannot be driven,
// so this is pinned by source.
//
// Mutants: the verify branch's noteRefreshFailure call removed; the refund
// removed; the label keyed back to ActivityVerifyingEnd alone.
func TestTheVerifyBranchKeepsUpAResumeWait(t *testing.T) {
	src, err := os.ReadFile("orchestrator_youtube.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	const call = "noteRefreshFailure(&waitedForResume, &waitEpisode, refreshErr, jobCtx.Interruption.fresh(), mayResume, jobCtx.Config.InterruptionTimeout, time.Now())"
	if n := strings.Count(body, call); n != 2 {
		t.Fatalf("noteRefreshFailure is asked from %d live-loop sites, want 2 (quality loss, stream-end verify)", n)
	}
	second := strings.LastIndex(body, call)
	if refund := strings.Index(body[second:], "consecutiveLiveChecks.Add(-1)"); refund < 0 || refund > 300 {
		t.Error("the verify branch's wait does not refund its live check")
	}
	if !strings.Contains(body, "if waitEpisode.active(time.Now(), jobCtx.Config.InterruptionTimeout) {\n\t\t\t\ttracker.SetWaitActivity(engine.ActivityWaitingResume)") {
		t.Error("the still-live sleep is not labelled a wait inside a resume wait")
	}
}
