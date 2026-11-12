package web

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// bodyDeadlineServer serves h as the real server does: bodyReadDeadline
// inside outermostRecovery, so the deadline has to reach the connection
// through recoveryWriter's Unwrap.
func bodyDeadlineServer(t *testing.T, timeout time.Duration, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(outermostRecovery(&errorLineLogger{}, bodyReadDeadline(timeout, h)))
	t.Cleanup(srv.Close)
	return srv
}

// stalledRequest opens a connection, sends the request line, headers that
// promise ten body bytes, and three of them, then stops.
func stalledRequest(t *testing.T, srv *httptest.Server, path, extraHeader string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	req := "POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n" + extraHeader + "\r\nabc"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// TestAStalledRequestBodyIsCutAtTheDeadline: a peer that sends its headers
// and part of its body and then nothing used to block the handler's read for
// as long as it kept the socket open. The read now fails at the deadline.
//
// Mutant: bodyReadDeadline setting no deadline: the read never returns and
// the test fails on its own bound.
func TestAStalledRequestBodyIsCutAtTheDeadline(t *testing.T) {
	readErr := make(chan error, 1)
	srv := bodyDeadlineServer(t, 200*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
	})
	stalledRequest(t, srv, "/api/jobs", "")

	select {
	case err := <-readErr:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("the stalled read ended with %v, want a timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler's read of a stalled body never returned")
	}
}

// TestAnUnreadStalledBodyDoesNotPinTheConnection: the unauthenticated case.
// The handler answers without reading the body (as the auth refusal does) and
// net/http then drains what the peer promised before it can reuse or close
// the connection. That drain had no deadline; it now ends at the one left
// armed, and the server closes the connection.
//
// Mutant: clearing the deadline when the handler returns, or never setting
// it: the client's read below times out instead of reaching EOF.
func TestAnUnreadStalledBodyDoesNotPinTheConnection(t *testing.T) {
	srv := bodyDeadlineServer(t, 200*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	conn := stalledRequest(t, srv, "/api/jobs", "")

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("the server kept the connection open past the deadline: %v", err)
	}
}

// TestALongHandlerOutlivesTheBodyDeadline: net/http keeps a background read
// on the connection whenever no body is left to read (from the start for a
// request without one, from the body's EOF otherwise), and a deadline firing
// under it cancels the request context, aborting any handler that works on
// (the formats lookup, the cookie setup's finish). So a request with no body
// gets no deadline, and none is set once a body has reached its end.
//
// Mutants: setting a deadline on a bodyless request fails "without a body";
// moving the import deadline ahead of a Read made after EOF fails "import,
// read again after EOF". "with a body" holds the rule against a change that
// arms a deadline after the body is done by any other route.
func TestALongHandlerOutlivesTheBodyDeadline(t *testing.T) {
	const timeout = 150 * time.Millisecond
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"with a body", http.MethodPost, "/api/x", `{"a":1}`},
		{"import, read again after EOF", http.MethodPost, importPath, "PK"},
		{"without a body", http.MethodGet, "/api/x", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := bodyDeadlineServer(t, timeout, func(w http.ResponseWriter, r *http.Request) {
				if tc.body != "" {
					if _, err := io.ReadAll(r.Body); err != nil {
						http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
						return
					}
					// A second read at EOF, as a reader that asks again does.
					_, _ = r.Body.Read(make([]byte, 1))
				}
				select {
				case <-r.Context().Done():
					http.Error(w, "context cancelled under the handler", http.StatusInternalServerError)
				case <-time.After(4 * timeout):
					w.Write([]byte("done"))
				}
			})
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(got) != "done" {
				t.Fatalf("got %d %q, want 200 \"done\"", resp.StatusCode, got)
			}
		})
	}
}

// trickle sends a ten-byte body one byte at a time, every gap, on path.
func trickle(t *testing.T, srv *httptest.Server, path string, gap time.Duration) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte("POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		time.Sleep(gap)
		if _, err := conn.Write([]byte("z")); err != nil {
			return // the server gave up on the body
		}
	}
}

// TestTheImportUploadIsBoundedPerReadNotInTotal: POST /api/import carries
// archives of up to 500 MB, which no fixed budget fits on a slow link. Its
// deadline moves ahead of every read, so an upload that keeps arriving goes
// through however long it takes, while the same slow body on any other route
// is cut at the fixed budget.
//
// Mutant: dropping the per-read extension fails the import half; applying
// it to every route fails the other.
func TestTheImportUploadIsBoundedPerReadNotInTotal(t *testing.T) {
	// Ten bytes 100 ms apart: a second in total, twice the budget, with
	// every gap a fifth of it.
	const timeout = 500 * time.Millisecond
	readErr := make(chan error, 1)
	srv := bodyDeadlineServer(t, timeout, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
	})
	result := func(path string) error {
		t.Helper()
		trickle(t, srv, path, 100*time.Millisecond)
		select {
		case err := <-readErr:
			return err
		case <-time.After(10 * time.Second):
			t.Fatalf("the handler's read on %s never returned", path)
			return nil
		}
	}
	if err := result("/api/import"); err != nil {
		t.Errorf("a slow import that kept sending failed: %v", err)
	}
	if err := result("/api/jobs"); err == nil {
		t.Error("a body slower than the whole budget was read in full on an ordinary route")
	}
}

// TestAWebSocketUpgradeGetsNoBodyDeadline: the upgraded socket outlives the
// request, so a deadline set for the request would cut it.
//
// Mutant: dropping the Upgrade exclusion: the read below fails at the
// deadline.
func TestAWebSocketUpgradeGetsNoBodyDeadline(t *testing.T) {
	const timeout = 100 * time.Millisecond
	readErr := make(chan error, 1)
	srv := bodyDeadlineServer(t, timeout, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
	})
	conn := stalledRequest(t, srv, "/", "Upgrade: websocket\r\n")

	select {
	case err := <-readErr:
		t.Fatalf("the read on an upgrade request ended with %v; it must have no deadline", err)
	case <-time.After(5 * timeout):
	}
	conn.Close()
	<-readErr
}
