package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/chat"
)

// TestEarlyChatNeedsRestart pins the upcoming-wait loop's restart decision.
//
// The loop used to gate the retry on `chatDl == nil` alone, so a downloader
// whose run had ENDED — YouTube resets a waiting-room chat after a period of
// inactivity, recoverStaleContinuation then exhausts its ~50-minute budget and
// the run leaves — stayed non-nil forever and nothing captured the waiting
// room again until the process restarted.
//
// The "finished" arm cannot be IsRunning(): that is also false in the window
// between NewChatDownloader and the background goroutine reaching Start, so it
// would restart a downloader that has not begun yet. The flag comes from
// OnFinish, which the chat downloader fires only after its final flush.
func TestEarlyChatNeedsRestart(t *testing.T) {
	dl := chat.NewChatDownloader(chat.ChatDownloaderOptions{VideoID: "v1", OutputFile: "unused"})

	cases := []struct {
		name     string
		chatDl   *chat.ChatDownloader
		finished bool
		want     bool
	}{
		{"never started — nothing to keep", nil, false, true},
		{"run in progress — leave it alone", dl, false, false},
		{"run ended — start a new one", dl, true, true},
		{"no downloader, stale flag — still start one", nil, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := earlyChatNeedsRestart(c.chatDl, c.finished); got != c.want {
				t.Errorf("earlyChatNeedsRestart(%v, %v) = %v, want %v",
					c.chatDl != nil, c.finished, got, c.want)
			}
		})
	}
}

// TestUntrackChatRemovesOnlyThatDownloader pins the registry half of the
// restart: the loop replaces chatDl, so the run it is dropping has to leave
// activeChats or StreamProcessor.Stop keeps Stop()ing a finished downloader
// forever and the slice grows once per restart.
func TestUntrackChatRemovesOnlyThatDownloader(t *testing.T) {
	sp := &StreamProcessor{}
	a := chat.NewChatDownloader(chat.ChatDownloaderOptions{VideoID: "a", OutputFile: "unused"})
	b := chat.NewChatDownloader(chat.ChatDownloaderOptions{VideoID: "b", OutputFile: "unused"})

	sp.trackChat(a)
	sp.trackChat(b)
	sp.untrackChat(a)

	if len(sp.activeChats) != 1 || sp.activeChats[0] != b {
		t.Fatalf("activeChats holds %d downloader(s), want exactly the one that was not untracked", len(sp.activeChats))
	}
}

// --- structural pins -----------------------------------------------------
//
// tryStartEarlyChat's first act is youtube.FetchWatchPage, and the restart
// block sits inside waitForLive's probe loop behind ProbeVideoStatus — both
// are live network calls, so neither can be driven from a unit test. What can
// be asserted here is the SHAPE, the way cmd/moombox/recheck_callsite_test.go
// pins its five re-check sites: the wiring that makes the pure decision above
// mean anything.

func parseYouTubeProcessor(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "stream_processor_youtube.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse stream_processor_youtube.go: %v", err)
	}
	return fset, file
}

func funcDeclNamed(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("no function %q in stream_processor_youtube.go", name)
	return nil
}

// selectorCallPos returns the position of the first call to recv.method in n,
// or token.NoPos when there is none.
func selectorCallPos(n ast.Node, recv, method string) token.Pos {
	found := token.NoPos
	ast.Inspect(n, func(node ast.Node) bool {
		if found != token.NoPos {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == recv {
			found = call.Pos()
			return false
		}
		return true
	})
	return found
}

// TestTryStartEarlyChatWiresOnFinish pins the producer side: without the
// OnFinish assignment nothing ever sets the flag earlyChatNeedsRestart reads,
// and the restart arm is dead code that no behavioural test in the tree can
// see.
func TestTryStartEarlyChatWiresOnFinish(t *testing.T) {
	_, file := parseYouTubeProcessor(t)
	fn := funcDeclNamed(t, file, "tryStartEarlyChat")

	wired := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "OnFinish" {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "dl" {
			wired = true
			return false
		}
		return true
	})
	if !wired {
		t.Error("tryStartEarlyChat must assign dl.OnFinish — it is the only thing that records that an early-chat run ended, and earlyChatNeedsRestart's finished arm is dead without it")
	}
}

// TestWaitForLiveRestartsEarlyChatInOrder pins the consumer side: the retry
// block is gated on earlyChatNeedsRestart (not the old `chatDl == nil`), and
// the downloader being replaced is untracked BEFORE the new one is started.
// Untracking afterwards would remove the wrong entry on any future refactor
// that reuses the variable, and leaving it out entirely grows activeChats by
// one finished downloader per restart.
func TestWaitForLiveRestartsEarlyChatInOrder(t *testing.T) {
	fset, file := parseYouTubeProcessor(t)
	fn := funcDeclNamed(t, file, "waitForLive")

	var block *ast.IfStmt
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		uses := false
		ast.Inspect(ifStmt.Cond, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "earlyChatNeedsRestart" {
				uses = true
				return false
			}
			return true
		})
		if uses {
			block = ifStmt
			return false
		}
		return true
	})
	if block == nil {
		t.Fatal("waitForLive must gate its early-chat (re)start on earlyChatNeedsRestart — a bare `chatDl == nil` never restarts a run that ended")
	}

	untrack := selectorCallPos(block.Body, "sp", "untrackChat")
	start := selectorCallPos(block.Body, "sp", "tryStartEarlyChat")
	if untrack == token.NoPos {
		t.Fatal("the restart block must untrack the downloader it is replacing")
	}
	if start == token.NoPos {
		t.Fatal("the restart block must call sp.tryStartEarlyChat")
	}
	if untrack >= start {
		t.Errorf("sp.untrackChat (line %d) must come BEFORE sp.tryStartEarlyChat (line %d) in the restart block",
			fset.Position(untrack).Line, fset.Position(start).Line)
	}
}
