package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The still-live stall refresh (runLiveStreamDownload's `case
// youtube.StreamLive:`) re-creates the downloaders with no forced start, so
// they resume the CURRENT part through its sidecar. It used to adopt the
// refresh unconditionally — so a stream that came back from the stall at a
// different quality (an encoder restart is the usual cause) appended the new
// rendition's fragments under the old init segment, and the mixed tail was
// muxed into the part before the monitor's next tick split it. The refresh
// must be judged first, and a changed quality must take the same split a
// quality change takes.
//
// Pinned structurally rather than driven: the branch sits behind a
// five-minute verify sleep and the live strategies' real manifest fetches.
//
// Mutant: deleting the Changed() guard, or adopting the refresh
// (`result = refreshResult`) ahead of it.
func TestStillLiveRefreshSplitsOnAChangedQuality(t *testing.T) {
	const name = "orchestrator_youtube.go"
	fset := token.NewFileSet()
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	text := func(n ast.Node) string {
		if n == nil {
			return ""
		}
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}

	var liveCase *ast.CaseClause
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runLiveStreamDownload" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if ok && len(cc.List) == 1 && text(cc.List[0]) == "youtube.StreamLive" {
				liveCase = cc
			}
			return liveCase == nil
		})
		return false
	})
	if liveCase == nil {
		t.Fatal("no `case youtube.StreamLive:` in runLiveStreamDownload")
	}

	refresh, guard, adopt := -1, -1, -1
	for i, stmt := range liveCase.Body {
		body := text(stmt)
		switch {
		case refresh < 0 && strings.Contains(body, "o.refreshDownload("):
			refresh = i
		case guard < 0:
			ifs, ok := stmt.(*ast.IfStmt)
			if ok && strings.Contains(text(ifs.Init)+text(ifs.Cond), ".Changed(currentQuality)") &&
				strings.Contains(text(ifs.Body), "splitPart(") {
				guard = i
			}
		}
		if adopt < 0 && strings.TrimSpace(body) == "result = refreshResult" {
			adopt = i
		}
	}
	if refresh < 0 {
		t.Fatal("the still-live branch no longer calls o.refreshDownload")
	}
	if guard < 0 {
		t.Fatal("the still-live branch adopts its refresh without checking the quality it came back at — " +
			"a changed rendition resumes into the current part under the old init segment")
	}
	if adopt < 0 || !(refresh < guard && guard < adopt) {
		t.Errorf("the quality guard must sit between the refresh and its adoption: refresh at %d, guard at %d, adopt at %d",
			refresh, guard, adopt)
	}
}
