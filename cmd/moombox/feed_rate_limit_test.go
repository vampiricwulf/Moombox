package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestMonitorProbeMarksARateLimitDistinctly: a status probe the session's
// rate limit refused reaches the monitors marked monitor.ErrProbeRateLimited
// beside ErrProbeInconclusive, so the feed walk holds the row for the
// backlog's rate-limit hold rather than climb its ladder (wave FC); a bot
// wall stays inconclusive alone. Text and chain are kept either way.
//
// Mutant: monitorProbe marking a rate limit like any inconclusive probe (the
// rate-limited row lacks ErrProbeRateLimited); marking every inconclusive
// probe rate-limited (the bot-wall row carries it).
func TestMonitorProbeMarksARateLimitDistinctly(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *youtube.StatusProbeInconclusiveError
		want bool
	}{
		{"a rate limit", &youtube.StatusProbeInconclusiveError{Playability: youtube.PlayabilityRateLimited, Reason: "try again later"}, true},
		{"a bot wall", &youtube.StatusProbeInconclusiveError{Playability: youtube.PlayabilityLoginRequired, Escalation: youtube.StatusProbeEscalationWebEmbedded, EscalationErr: errors.New("refused too")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := monitorProbe(func(context.Context, string) (*youtube.VideoInfo, error) { return nil, tc.err })(context.Background(), "test1234567")
			if !errors.Is(err, monitor.ErrProbeInconclusive) {
				t.Errorf("%v not marked inconclusive", err)
			}
			if got := errors.Is(err, monitor.ErrProbeRateLimited); got != tc.want {
				t.Errorf("marked rate-limited: %v, want %v", got, tc.want)
			}
			if err == nil || err.Error() != tc.err.Error() || !errors.Is(err, tc.err) {
				t.Errorf("error %v, want the probe's own text and chain", err)
			}
		})
	}
}

// TestFeedRateLimitHoldIsWired pins the production value of the feed walk's
// rate-limit seam (monitor.FeedMonitor.RateLimitHeld): the backlog
// scheduler's own hold, assigned exactly once. Left nil, a rate-limited row
// would be asked every cycle into the limit the hold stands for.
//
// Mutant: dropping the assignment, or wiring anything else.
func TestFeedRateLimitHoldIsWired(t *testing.T) {
	const want = "s.dlWorker.Scheduler().RateLimitHeld"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range as.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "RateLimitHeld" && i < len(as.Rhs) {
					got = append(got, types.ExprString(lhs)+" = "+types.ExprString(as.Rhs[i]))
				}
			}
			return true
		})
	}
	if len(got) != 1 || got[0] != "s.feedMon.RateLimitHeld = "+want {
		t.Errorf("RateLimitHeld assignments = %v, want exactly s.feedMon.RateLimitHeld = %s", got, want)
	}
}
