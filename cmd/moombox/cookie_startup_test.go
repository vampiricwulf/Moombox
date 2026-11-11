package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// fakeCookieServices records the cookie startup sequence's calls, in order.
// A non-nil hold blocks Start until it is closed or the context ends, which
// is how a test keeps the "first check" running.
type fakeCookieServices struct {
	mu    sync.Mutex
	calls []string
	hold  chan struct{}
	// entered is closed when Start has been called.
	entered  chan struct{}
	panicOn  string
	interval time.Duration
}

func newFakeCookieServices() *fakeCookieServices {
	return &fakeCookieServices{entered: make(chan struct{})}
}

func (f *fakeCookieServices) note(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if f.panicOn == name {
		panic("boom in " + name)
	}
}

func (f *fakeCookieServices) Start(ctx context.Context) {
	close(f.entered)
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
		}
	}
	f.note("check")
}

func (f *fakeCookieServices) StartProfileSeed(context.Context) { f.note("seed") }

func (f *fakeCookieServices) StartPeriodicRefresh(_ context.Context, d time.Duration) {
	f.mu.Lock()
	f.interval = d
	f.mu.Unlock()
	f.note("timer")
}

func (f *fakeCookieServices) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type nopStartupLog struct{}

func (nopStartupLog) Debug(string, ...any) {}
func (nopStartupLog) Info(string, ...any)  {}
func (nopStartupLog) Warn(string, ...any)  {}
func (nopStartupLog) Error(string, ...any) {}

func waitStartup(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the cookie startup goroutine did not return")
	}
}

func sameCalls(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The sequence runs off the caller's goroutine (owner ruling amending R-H2:
// the TUI comes up first too) and keeps its order: nothing of the seed or the
// timer happens while the first check is still running.
//
// Mutants: call the body in line instead of `go` (startCookieSequence does
// not return while the check is held); move the seed above the check.
func TestCookieStartupRunsInTheBackgroundInOrder(t *testing.T) {
	f := newFakeCookieServices()
	f.hold = make(chan struct{})
	returned := make(chan (<-chan struct{}), 1)
	go func() {
		returned <- startCookieSequence(context.Background(),
			cookieStartup{HasFile: true, AutoEnabled: true, RefreshInterval: 7 * time.Minute}, f, f, nopStartupLog{})
	}()
	var done <-chan struct{}
	select {
	case done = <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("startCookieSequence blocked on the first check: the TUI would wait for it")
	}
	<-f.entered
	if got := f.got(); len(got) != 0 {
		t.Fatalf("calls while the first check is running = %v, want none", got)
	}
	close(f.hold)
	waitStartup(t, done)
	if got, want := f.got(), []string{"check", "seed", "timer"}; !sameCalls(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if f.interval != 7*time.Minute {
		t.Errorf("timer interval = %s, want 7m0s", f.interval)
	}
}

// The start conditions run() had in line: no first check without a cookie
// file, the seed always, the timer only with auto_enabled and an interval.
func TestCookieStartupConditions(t *testing.T) {
	cases := []struct {
		name  string
		start cookieStartup
		want  []string
	}{
		{"no cookie file", cookieStartup{}, []string{"seed"}},
		{"file, timer off", cookieStartup{HasFile: true, RefreshInterval: time.Hour}, []string{"check", "seed"}},
		{"file, no interval", cookieStartup{HasFile: true, AutoEnabled: true}, []string{"check", "seed"}},
		{"no file, timer on", cookieStartup{AutoEnabled: true, RefreshInterval: time.Hour}, []string{"seed", "timer"}},
	}
	for _, c := range cases {
		f := newFakeCookieServices()
		waitStartup(t, startCookieSequence(context.Background(), c.start, f, f, nopStartupLog{}))
		if got := f.got(); !sameCalls(got, c.want) {
			t.Errorf("%s: calls = %v, want %v", c.name, got, c.want)
		}
	}
}

// A run that ends during the first check starts neither the seed nor the
// timer, and the goroutine returns: shutdown's bounded wait is not used up.
//
// Mutant: drop the ctx.Err() checks between the steps.
func TestCookieStartupStopsWhenTheRunEnds(t *testing.T) {
	f := newFakeCookieServices()
	f.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := startCookieSequence(ctx, cookieStartup{HasFile: true, AutoEnabled: true, RefreshInterval: time.Hour}, f, f, nopStartupLog{})
	<-f.entered
	cancel()
	waitStartup(t, done)
	if got, want := f.got(), []string{"check"}; !sameCalls(got, want) {
		t.Errorf("calls after the run ended = %v, want %v", got, want)
	}
}

// A panic in one step is recovered and costs that step only.
//
// Mutant: drop step's recover AND the goroutine's (the test binary dies).
func TestCookieStartupStepPanicIsRecovered(t *testing.T) {
	for _, step := range []string{"check", "seed", "timer"} {
		f := newFakeCookieServices()
		f.panicOn = step
		waitStartup(t, startCookieSequence(context.Background(),
			cookieStartup{HasFile: true, AutoEnabled: true, RefreshInterval: time.Hour}, f, f, nopStartupLog{}))
		if got, want := f.got(), []string{"check", "seed", "timer"}; !sameCalls(got, want) {
			t.Errorf("panic in %s: calls = %v, want %v", step, got, want)
		}
	}
}

// With the real services: a shutdown that stops both before the goroutine
// has started them leaves nothing running, and the sequence returns.
func TestCookieStartupAfterBothServicesStopped(t *testing.T) {
	refresh := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopStartupLog{})
	refresh.Stop()
	f := newFakeCookieServices() // the auto-cookie side has its own test (TestStartAfterStopStartsNothing)
	waitStartup(t, startCookieSequence(context.Background(), cookieStartup{HasFile: true}, refresh, f, nopStartupLog{}))
	if refresh.GetStatus().Checked {
		t.Error("a Start after Stop ran a check")
	}
}

func TestAwaitCookieStartup(t *testing.T) {
	if !awaitCookieStartup(nil, time.Hour) {
		t.Error("a nil channel (the sequence never started) must count as returned")
	}
	closed := make(chan struct{})
	close(closed)
	if !awaitCookieStartup(closed, time.Hour) {
		t.Error("a finished sequence reported as still running")
	}
	if awaitCookieStartup(make(chan struct{}), 10*time.Millisecond) {
		t.Error("a sequence that never returns was reported as returned: the wait is not bounded")
	}
}

// checkedLatch: an unchecked status is admitted until a checked one has been,
// and never after. That is what keeps a status read before the first check
// ended from being sent to the TUI after the check's own answer.
//
// Mutant: admit returning true for every status.
func TestCheckedLatchRefusesAStaleUncheckedStatus(t *testing.T) {
	var l checkedLatch
	send := func(a cookies.AuthStatus) bool {
		defer l.enter()()
		return l.admit(a)
	}
	if !send(cookies.AuthStatus{}) {
		t.Error("an unchecked status before any checked one was refused: the TUI would never show checking")
	}
	if !send(cookies.AuthStatus{Checked: true}) {
		t.Error("a checked status was refused")
	}
	if send(cookies.AuthStatus{}) {
		t.Error("an unchecked status after a checked one was admitted: the badge would go back to checking")
	}
	if !send(cookies.AuthStatus{Checked: true}) {
		t.Error("a later checked status was refused")
	}
}

// TestCookieStartupIsOneGoroutine pins the shape the owner ruling asks for:
// startCookieSequence has exactly one `go` statement, the three starts are
// inside it in order (first check, profile seed, refresh timer), and it
// carries a recover. run() itself calls none of the three directly.
//
// Mutants: a second `go` around the seed; the timer above the seed; the
// recover removed; cookieRefresh.Start called in line in run().
func TestCookieStartupIsOneGoroutine(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cookie_startup.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "startCookieSequence" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("cookie_startup.go has no startCookieSequence")
	}
	var gos []*ast.GoStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			gos = append(gos, g)
		}
		return true
	})
	if len(gos) != 1 {
		t.Fatalf("startCookieSequence has %d go statements, want exactly 1", len(gos))
	}
	selPos := func(recv, name string) token.Pos {
		pos := token.NoPos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || pos != token.NoPos {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
					pos = call.Pos()
				}
			}
			return true
		})
		return pos
	}
	prev := gos[0].Pos()
	for _, c := range []struct{ recv, name string }{
		{"cookieRefresh", "Start"},
		{"autoCookieSvc", "StartProfileSeed"},
		{"autoCookieSvc", "StartPeriodicRefresh"},
	} {
		pos := selPos(c.recv, c.name)
		switch {
		case pos == token.NoPos:
			t.Fatalf("startCookieSequence: no call to %s.%s", c.recv, c.name)
		case pos < gos[0].Pos() || pos > gos[0].End():
			t.Errorf("%s.%s (%s) is outside the one goroutine", c.recv, c.name, fset.Position(pos))
		case pos < prev:
			t.Errorf("%s.%s (%s) is out of order: first check, then seed, then timer", c.recv, c.name, fset.Position(pos))
		}
		prev = pos
	}
	lit, ok := gos[0].Call.Fun.(*ast.FuncLit)
	if !ok {
		t.Fatal("the goroutine is not a func literal: its recover cannot be inline")
	}
	// The goroutine's own recover: a top-level defer of the literal's body,
	// not the per-step one nested in a closure.
	hasRecover := false
	for _, st := range lit.Body.List {
		d, ok := st.(*ast.DeferStmt)
		if !ok {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recover" {
					hasRecover = true
				}
			}
			return true
		})
	}
	if !hasRecover {
		t.Error("the cookie startup goroutine has no inline defer/recover")
	}

	run, _ := callsInFunc(t, "main.go", "run")
	for _, c := range []struct{ recv, name string }{
		{"cookieRefresh", "Start"},
		{"autoCookieSvc", "StartProfileSeed"},
		{"autoCookieSvc", "StartPeriodicRefresh"},
	} {
		if pos := firstCall(run, c.recv, c.name); pos != token.NoPos {
			t.Errorf("run() calls %s.%s in line: it belongs to startCookieSequence", c.recv, c.name)
		}
	}
}
