package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// renderingLogger records every log line with its ARGS RENDERED as
// key=value pairs. That is the point: this package's other recorders drop
// args on purpose (chat_reauth_test.go's recordingLogger, internal/cookies'
// capturingLogger) so a captured token cannot reach test output. Here the
// hazard IS in the args, so a recorder that dropped them would make the leak
// invisible and these tests vacuous. Nothing it captures is ever printed.
//
// No t.Parallel in this file: installProbeStub swaps a package-level var,
// and the retried-arm test shrinks a package-level delay.
type renderingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *renderingLogger) record(level, msg string, args ...any) {
	var b strings.Builder
	b.WriteString(level + " " + msg)
	for i := 0; i < len(args); i += 2 {
		if i+1 < len(args) {
			fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
		} else {
			fmt.Fprintf(&b, " %v", args[i])
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, b.String())
}

func (l *renderingLogger) Debug(msg string, args ...any) { l.record("DEBUG", msg, args...) }
func (l *renderingLogger) Info(msg string, args ...any)  { l.record("INFO", msg, args...) }
func (l *renderingLogger) Warn(msg string, args ...any)  { l.record("WARN", msg, args...) }
func (l *renderingLogger) Error(msg string, args ...any) { l.record("ERROR", msg, args...) }

// countLinesContaining reports how many captured lines contain sub.
func (l *renderingLogger) countLinesContaining(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// allLines joins every captured line with a newline, for a single fragment
// scan across the whole log rather than one marker-Contains check per line.
func (l *renderingLogger) allLines() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// gqlLeakMarker stands in for what actually rides in an intermediary's error
// page: an echo of the request's Authorization header. A synthetic literal,
// so it is safe to search for -- and no assertion below prints it.
const gqlLeakMarker = "OAuth-echoed-credential-marker"

// gqlLeakBody is the stub's answer for every arm. 42 bytes.
const gqlLeakBody = `{"error":"` + gqlLeakMarker + `"}`

// assertNoBodyFragment fails if ANY 8-byte window of gqlLeakBody appears
// anywhere in text. A whole-marker strings.Contains(text, gqlLeakMarker)
// check can be defeated by a mutant that leaks only a body PREFIX shorter
// than the marker (e.g. gqlBodySize rendering the first 32 of the marker's
// 42 bytes and then the byte count) -- the marker never appears whole, so
// that check passes while a fragment of the upstream body still reached the
// caller. Scanning every 8-byte window closes that gap regardless of where
// in the body the leak starts or how much of it survives. Like every other
// assertion in this file, it never prints what it found -- only that it did.
func assertNoBodyFragment(t *testing.T, where, text string) {
	t.Helper()
	const window = 8
	for i := 0; i+window <= len(gqlLeakBody); i++ {
		if strings.Contains(text, gqlLeakBody[i:i+window]) {
			t.Errorf("%s carries a fragment of the response body; not printed here on purpose", where)
			return
		}
	}
}

// TestGQLRetriedArmsNeverLogOrReturnBody drives the two RETRIED arms -- 429
// without a Retry-After header, and 5xx -- to exhaustion with the backoff
// shrunk to a millisecond, so every retry Debug line fires and the error the
// caller finally receives is gqlRequest's own. Before this fix the retry
// line logged lastErr verbatim and lastErr interpolated the raw body, so
// every 5xx or 429 wrote an upstream error page into a log Moombox fans out
// over the WebSocket to the dashboard and the TUI.
func TestGQLRetriedArmsNeverLogOrReturnBody(t *testing.T) {
	prevDelay := gqlBaseRetryDelay
	gqlBaseRetryDelay = time.Millisecond
	t.Cleanup(func() { gqlBaseRetryDelay = prevDelay })

	for _, tc := range []struct {
		status int
		prefix string
	}{
		{http.StatusTooManyRequests, "gql rate limited (429) (TestOp): "},
		{http.StatusServiceUnavailable, "gql http 503 (TestOp): "},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := installProbeStub(t, tc.status, gqlLeakBody)
			log := &renderingLogger{}
			a := NewAPI(log)

			_, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, "")
			if err == nil {
				t.Fatalf("a %d answer must not succeed", tc.status)
			}
			if got := calls.Load(); got != gqlMaxRetries+1 {
				t.Fatalf("stub answered %d times, want %d (the first attempt plus every retry)", got, gqlMaxRetries+1)
			}
			if n := log.countLinesContaining("twitch gql retry"); n != gqlMaxRetries {
				t.Fatalf("%d retry Debug lines, want %d -- without them this test is vacuous", n, gqlMaxRetries)
			}

			// The log: no body (not even a fragment of it), no rendering
			// of the previous error at all (not even its sanitised
			// form), and the status the retry followed.
			assertNoBodyFragment(t, "a retry log line", log.allLines())
			last := errors.Unwrap(err) // the exhausted wrap's %w: the final lastErr
			if last == nil {
				t.Fatal("the exhausted-retries error must wrap the last attempt's error")
			}
			if n := log.countLinesContaining(last.Error()); n != 0 {
				t.Errorf("%d retry line(s) render the previous error; the retry line reports prev_status, never prev_err", n)
			}
			if n := log.countLinesContaining(fmt.Sprintf("prev_status=%d", tc.status)); n != gqlMaxRetries {
				t.Errorf("%d retry line(s) report prev_status=%d, want every one of the %d", n, tc.status, gqlMaxRetries)
			}

			// The returned error: the SIZE instead of the bytes (not even
			// a fragment of them), prefix intact -- worker.classifyProbeErr
			// reads the status out of "gql http <code> (" positionally.
			assertNoBodyFragment(t, "the returned error", err.Error())
			want := tc.prefix + fmt.Sprintf("%d-byte body", len(gqlLeakBody))
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the returned error must contain %q -- the prefix verbatim and the body SIZE, not its bytes", want)
			}
		})
	}
}

// TestGQLUnretriedArmsCarryByteCountNotBody covers the three arms that return
// without a retry -- exactly the errors that travel up to callers who log
// them: 401/403 with a token (wraps ErrTwitchAuthExpired), 401/403 without
// one (no sentinel: anonymous GQL legitimately gets 401 on some paths, and
// looping the user through a login flow for it would be pointless), and any
// other 4xx.
func TestGQLUnretriedArmsCarryByteCountNotBody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		token    string
		prefix   string
		sentinel bool
	}{
		{"403 with token", http.StatusForbidden, "a-token", "gql auth failure (403) (TestOp): ", true},
		{"401 without token", http.StatusUnauthorized, "", "gql auth failure (401) (TestOp): ", false},
		{"400 other 4xx", http.StatusBadRequest, "", "gql http 400 (TestOp): ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installProbeStub(t, tc.status, gqlLeakBody)
			log := &renderingLogger{}
			a := NewAPI(log)

			_, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, tc.token)
			if err == nil {
				t.Fatalf("a %d answer must not succeed", tc.status)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("stub answered %d times, want exactly 1 -- this arm must not retry", got)
			}
			if errors.Is(err, ErrTwitchAuthExpired) != tc.sentinel {
				t.Errorf("errors.Is(err, ErrTwitchAuthExpired) = %v, want %v", !tc.sentinel, tc.sentinel)
			}
			assertNoBodyFragment(t, "the returned error", err.Error())
			want := tc.prefix + fmt.Sprintf("%d-byte body", len(gqlLeakBody))
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the returned error must contain %q -- the prefix verbatim and the body SIZE, not its bytes", want)
			}
			assertNoBodyFragment(t, "a log line", log.allLines())
		})
	}
}
