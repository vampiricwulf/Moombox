package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lateStartPlan saves the fixture's usual plan: the capture began 25 s into
// the broadcast, so the opening is the one span to fill.
func lateStartPlan(t *testing.T, f *gapFillFixture, deadline time.Time) {
	t.Helper()
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}}, deadline)
}

// TestRecoveredPartPathEndsOnAStatError: the search for a free recovered-part
// name ends, with the error, on a Stat answer that is neither "found" nor
// "not found" (access denied, a dropped share), where it used to read every
// such answer as "name taken" and never return. The look that hit it keeps
// its plan with the error as the reason, and nothing is written.
//
// Mutants: only IsNotExist ending the loop (this test never returns); the
// error swallowed (the plan's reason does not name it).
func TestRecoveredPartPathEndsOnAStatError(t *testing.T) {
	f := newGapFillFixture(t)
	denied := &os.PathError{Op: "stat", Path: "x", Err: os.ErrPermission}
	stats := 0
	gapFillStat = func(string) (os.FileInfo, error) { stats++; return nil, denied }
	t.Cleanup(func() { gapFillStat = os.Stat })

	if _, err := recoveredPartPath(context.Background(), f.dir, "Streamer - Big day", 1); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want the Stat error", err)
	}
	if stats != 1 {
		t.Errorf("%d Stat calls, want the search to end on the first error", stats)
	}

	lateStartPlan(t, f, gapStart.Add(25*time.Hour))
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))
	if p := f.row(t); p == nil || !strings.Contains(p.LastReason, "the archive's directory could not be read") {
		t.Errorf("plan %+v, want it kept with the directory's error as the reason", p)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.dir, "*recovered*")); len(matches) != 0 {
		t.Errorf("the failed look left %v", matches)
	}
}

// TestRecoveredPartPathIsBoundedAndEndsWithItsContext: every name taken ends
// the search after asideOutputCollisionLimit names, and a cancelled context
// ends it at once. A name that is taken is still skipped, and a free one
// taken.
//
// Mutants: no bound (the first case never returns); no ctx check (the
// cancelled search Stats).
func TestRecoveredPartPathIsBoundedAndEndsWithItsContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a - recovered1.mp4"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := recoveredPartPath(context.Background(), dir, "a", 1); err != nil || filepath.Base(p) != "a - recovered2.mp4" {
		t.Errorf("path %q, %v, want recovered2", p, err)
	}

	stats := 0
	gapFillStat = func(string) (os.FileInfo, error) { stats++; return nil, nil }
	t.Cleanup(func() { gapFillStat = os.Stat })
	if _, err := recoveredPartPath(context.Background(), dir, "a", 1); err == nil || stats != asideOutputCollisionLimit {
		t.Errorf("err %v after %d Stat calls, want an error after %d", err, stats, asideOutputCollisionLimit)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats = 0
	if _, err := recoveredPartPath(ctx, dir, "a", 1); !errors.Is(err, context.Canceled) || stats != 0 {
		t.Errorf("err %v after %d Stat calls, want the context's error and none", err, stats)
	}
}
