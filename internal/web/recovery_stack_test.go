package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// panicLineLogger records the one Error line RecoveryMiddleware writes, both
// rendered (to search for anything that must not be there) and by key.
type panicLineLogger struct {
	line   string
	fields map[string]any
}

func (l *panicLineLogger) Error(msg string, args ...any) {
	l.line = msg
	l.fields = map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		l.line += fmt.Sprintf(" %v=%v", args[i], args[i+1])
		l.fields[fmt.Sprint(args[i])] = args[i+1]
	}
}

// handlerThatPanics is the bug an operator has to find from the log line.
func handlerThatPanics(http.ResponseWriter, *http.Request) {
	panic("boom")
}

// recursesThenPanics puts depth frames of its own between the handler and
// the panic.
func recursesThenPanics(depth int) {
	if depth == 0 {
		panic("deep boom")
	}
	recursesThenPanics(depth - 1)
}

// TestRecoveryLogsTheStackThatPanicked is W24-15: the panic line named the
// panic value, the method, the path, the peer and the request ID, and nothing
// that says WHERE — a panic reported from the field could not be located from
// its log, although SPEC and three spec docs said the stack was logged.
//
// The stack is one line (the ring buffer, the dashboards and the TUI log
// panel get the line as it is), starts at the function that panicked rather
// than inside the runtime or the recover handler, is bounded, and carries
// nothing from the request beyond what the line already logged: no query
// string, no header, no cookie, because it holds no argument values at all.
//
// Mutants this kills:
//   - the "stack" field dropped: no stack.
//   - the runtime.gopanic cut dropped: the stack starts at the recover
//     handler, not at the function that panicked.
//   - the panicStackFrames cap dropped: the deep stack runs to all its frames.
//   - debug.Stack (or frames joined by newlines) in place of panicStack: the
//     value spans lines.
func TestRecoveryLogsTheStackThatPanicked(t *testing.T) {
	serve := func(t *testing.T, h http.HandlerFunc) *panicLineLogger {
		t.Helper()
		log := &panicLineLogger{}
		req := httptest.NewRequest(http.MethodGet, "/api/jobs/x?token=QUERY-SECRET", nil)
		req.Header.Set("Authorization", "Bearer HEADER-SECRET")
		req.AddCookie(&http.Cookie{Name: "moombox_session", Value: "COOKIE-SECRET"})
		rr := httptest.NewRecorder()
		RecoveryMiddleware(log)(h).ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status %d, want 500", rr.Code)
		}
		if log.fields == nil {
			t.Fatal("the panic was not logged")
		}
		return log
	}
	stackOf := func(t *testing.T, log *panicLineLogger) string {
		t.Helper()
		stack, ok := log.fields["stack"].(string)
		if !ok || stack == "" {
			t.Fatalf("the panic line has no stack: %s", log.line)
		}
		if strings.ContainsAny(stack, "\r\n") {
			t.Errorf("the stack spans lines; the ring buffer, the dashboards and the TUI get it raw:\n%s", stack)
		}
		return stack
	}

	t.Run("names the function that panicked first", func(t *testing.T) {
		log := serve(t, handlerThatPanics)
		stack := stackOf(t, log)
		first, _, _ := strings.Cut(stack, " < ")
		if !strings.HasSuffix(strings.Fields(first)[0], ".handlerThatPanics") || !strings.Contains(first, "recovery_stack_test.go:") {
			t.Errorf("the stack must start at the frame that panicked, with its file and line; it starts %q", first)
		}
		for _, secret := range []string{"QUERY-SECRET", "HEADER-SECRET", "COOKIE-SECRET"} {
			if strings.Contains(log.line, secret) {
				t.Errorf("the panic line carries %s from the request: %s", secret, log.line)
			}
		}
	})

	t.Run("is bounded", func(t *testing.T) {
		const depth = 200
		stack := stackOf(t, serve(t, func(http.ResponseWriter, *http.Request) { recursesThenPanics(depth) }))
		frames := strings.Split(stack, " < ")
		if len(frames) > panicStackFrames+1 {
			t.Errorf("%d frames logged, want at most %d and a count of the rest", len(frames), panicStackFrames)
		}
		if last := frames[len(frames)-1]; !strings.Contains(last, "more") {
			t.Errorf("a cut stack must say how much it left out; it ends %q", last)
		}
		if !strings.Contains(frames[0], ".recursesThenPanics") {
			t.Errorf("the cut must drop the OUTERMOST frames, not the panic site; it starts %q", frames[0])
		}
	})
}
