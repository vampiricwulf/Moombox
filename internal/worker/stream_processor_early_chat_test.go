package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sync/atomic"
	"testing"
	"time"

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
	now := time.Now()
	longAgo := now.Add(-2 * earlyChatMinRestartInterval)
	justNow := now.Add(-time.Second)

	cases := []struct {
		name        string
		chatDl      *chat.ChatDownloader
		finished    bool
		lastRestart time.Time
		want        bool
	}{
		{"never started — nothing to keep", nil, false, longAgo, true},
		{"never started, and the interval never applies to the first start", nil, false, justNow, true},
		{"run in progress — leave it alone", dl, false, longAgo, false},
		{"run ended and past the interval — start a new one", dl, true, longAgo, true},
		{"run ended but within the interval — wait", dl, true, justNow, false},
		{"no downloader, stale flag — still start one", nil, true, justNow, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := earlyChatNeedsRestart(c.chatDl, c.finished, c.lastRestart, now); got != c.want {
				t.Errorf("earlyChatNeedsRestart(hasDownloader=%v, finished=%v, since=%v) = %v, want %v",
					c.chatDl != nil, c.finished, now.Sub(c.lastRestart), got, c.want)
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

// assignPos returns the position of the first assignment in n whose left-hand
// side is the bare identifier name, or token.NoPos when there is none.
func assignPos(n ast.Node, name string) token.Pos {
	found := token.NoPos
	ast.Inspect(n, func(node ast.Node) bool {
		if found != token.NoPos {
			return false
		}
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			return true
		}
		if ident, ok := assign.Lhs[0].(*ast.Ident); ok && ident.Name == name {
			found = assign.Pos()
			return false
		}
		return true
	})
	return found
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

// TestRunEarlyChatRecordsEndOnEveryPath is the producer side of the restart
// flag. It is deliberately NOT ChatDownloader.OnFinish: Start's own recovery
// defer returns before OnFinish is reached, so a run that panicked never fired
// it — the downloader then sat non-nil and dead for the rest of the wait,
// never restarted, which is the exact shape earlyChatNeedsRestart exists to
// eliminate, reached through a different door.
func TestRunEarlyChatRecordsEndOnEveryPath(t *testing.T) {
	t.Run("normal return", func(t *testing.T) {
		var finished atomic.Bool
		panics := 0
		runEarlyChat(&finished, func(any) { panics++ }, func() {})
		if !finished.Load() {
			t.Error("a run that returned normally must be recorded as ended")
		}
		if panics != 0 {
			t.Errorf("onPanic fired %d time(s) for a clean run, want 0", panics)
		}
	})

	t.Run("panicking run", func(t *testing.T) {
		var finished atomic.Bool
		var got any
		panics := 0
		runEarlyChat(&finished, func(r any) { panics++; got = r }, func() { panic("chat blew up") })
		if !finished.Load() {
			t.Error("a run that PANICKED must still be recorded as ended — otherwise nothing ever restarts it")
		}
		if panics != 1 || got != "chat blew up" {
			t.Errorf("onPanic fired %d time(s) with %v, want 1 with the panic value", panics, got)
		}
	})
}

// TestTryStartEarlyChatRunsThroughRunEarlyChat pins the wiring: the background
// goroutine must go through runEarlyChat, or the end-of-run flag is never set
// and earlyChatNeedsRestart's finished arm is dead. tryStartEarlyChat itself
// begins with youtube.FetchWatchPage, so only the shape is assertable here —
// the same approach cmd/moombox/recheck_callsite_test.go takes.
func TestTryStartEarlyChatRunsThroughRunEarlyChat(t *testing.T) {
	_, file := parseYouTubeProcessor(t)
	fn := funcDeclNamed(t, file, "tryStartEarlyChat")

	wired := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		goStmt, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		if ident, ok := goStmt.Call.Fun.(*ast.Ident); ok && ident.Name == "runEarlyChat" {
			wired = true
			return false
		}
		return true
	})
	if !wired {
		t.Error("tryStartEarlyChat must launch its run as `go runEarlyChat(...)` — that defer is the only thing that records an early-chat run ending, panics included")
	}
}

// TestWaitForLiveRestartsEarlyChatInOrder pins the consumer side: the retry
// block is gated on earlyChatNeedsRestart (not the old `chatDl == nil`), the
// downloader being replaced is untracked BEFORE the new one is started, and
// the restart-interval clock is re-armed AFTER it.
//
// Untracking afterwards would remove the wrong entry on any future refactor
// that reuses the variable, and leaving it out entirely grows activeChats by
// one finished downloader per restart. Leaving out the clock re-arm is
// quieter and worse: earlyChatMinRestartInterval would then be measured from
// the FIRST start forever, so the throttle would hold exactly once and every
// later probe would restart an instantly-dying run again — the whole point of
// F3 gone, with no behavioural test in the tree able to see it.
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

	armed := assignPos(block.Body, "chatStartedAt")
	if armed == token.NoPos {
		t.Fatal("the restart block must re-arm chatStartedAt — without it earlyChatMinRestartInterval is measured from the first start forever and the throttle holds exactly once")
	}
	if armed <= start {
		t.Errorf("chatStartedAt (line %d) must be re-armed AFTER sp.tryStartEarlyChat (line %d) — it times the run that was just started",
			fset.Position(armed).Line, fset.Position(start).Line)
	}
}
