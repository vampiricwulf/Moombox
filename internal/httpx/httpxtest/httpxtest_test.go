package httpxtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

// TestNewClientStreamsEachWriteAsItIsMade: inside a bubble, the client reads
// each chunk at the instant the handler wrote it, on the bubble's clock, and
// the status and headers the handler set arrive with the response.
//
// Mutant: buffering the body until the handler returns — the first chunk
// arrives at 2 s instead of 0.
func TestNewClientStreamsEachWriteAsItIsMade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Test", "yes")
			w.WriteHeader(http.StatusPartialContent)
			for range 3 {
				w.Write([]byte("ab"))
				w.(http.Flusher).Flush()
				time.Sleep(time.Second)
			}
		}))
		start := time.Now()
		resp, err := c.Get("http://example.test/x")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("X-Test") != "yes" {
			t.Fatalf("got %d, X-Test %q; want 206, yes", resp.StatusCode, resp.Header.Get("X-Test"))
		}
		buf := make([]byte, 16)
		for i := range 3 {
			n, err := resp.Body.Read(buf)
			if err != nil || string(buf[:n]) != "ab" {
				t.Fatalf("read %d = %q, %v; want \"ab\"", i, buf[:n], err)
			}
			if at, want := time.Since(start), time.Duration(i)*time.Second; at != want {
				t.Errorf("read %d at %s on the bubble's clock, want %s", i, at, want)
			}
		}
		if n, err := resp.Body.Read(buf); n != 0 || err != io.EOF {
			t.Errorf("after the handler returned: %d bytes, %v; want 0, EOF", n, err)
		}
	})
}

// TestNewClientFailsACancelledBody: once the request is cancelled, the
// client's read fails with the cancellation, even when the handler, seeing
// the same cancellation, returns at once and would end the body cleanly.
// Over a socket the read fails the same way, and a download that read the
// clean end instead would count a stalled transfer as a complete one.
//
// Mutant: the handler's return closing the pipe with a plain Close. Whether
// that close or the cancel watcher's lands first is the scheduler's choice:
// under -race (CI's race flavour) a round reads EOF in every run, while a
// plain build has run the watcher first every time it was tried.
func TestNewClientFailsACancelledBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("a"))
			<-r.Context().Done()
		}))
		for round := range 50 {
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test/x", nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("round %d: Do: %v", round, err)
			}
			if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
				t.Fatalf("round %d: first byte: %v", round, err)
			}
			cancel()
			synctest.Wait()
			if _, err := io.ReadAll(resp.Body); !errors.Is(err, context.Canceled) {
				t.Fatalf("round %d: read after the cancel = %v, want context.Canceled", round, err)
			}
			resp.Body.Close()
		}
	})
}

// TestNewClientAnswersAHandlerThatWritesNothing: 200 and an empty body, as
// net/http's server answers, and the handler sees a server's request — a
// body even for a GET, the host and the request URI.
//
// Mutant: no WriteHeader on the handler's return — Do never returns.
func TestNewClientAnswersAHandlerThatWritesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got *http.Request
		c := NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r }))
		resp, err := c.Get("http://example.test/a/b?c=d")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || len(body) != 0 {
			t.Fatalf("got %d, %q, %v; want 200, empty, nil", resp.StatusCode, body, err)
		}
		if got.Body == nil || got.Host != "example.test" || got.RequestURI != "/a/b?c=d" {
			t.Errorf("handler saw body %v, host %q, URI %q; want a body, example.test, /a/b?c=d", got.Body, got.Host, got.RequestURI)
		}
	})
}
