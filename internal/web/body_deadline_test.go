package web

import (
	"errors"
	"fmt"
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
// gets no deadline, and none is set once a body has reached its end. A real
// WebSocket upgrade is the bodiless case: it reaches the upgrade handler
// with no deadline on the connection the socket will keep.
//
// Each handler is held until the deadline its request could have been given
// is well past, then reports whether its context was cancelled.
//
// Mutants: setting a deadline on a bodyless request fails "without a body"
// and "a bodiless upgrade"; moving the import deadline ahead of a Read made
// after EOF fails "import, read again after EOF". "with a body" holds the
// rule against a change that arms a deadline after the body is done by any
// other route.
func TestALongHandlerOutlivesTheBodyDeadline(t *testing.T) {
	const timeout = 400 * time.Millisecond
	for _, tc := range []struct {
		name, method, path, body, upgrade string
	}{
		{"with a body", http.MethodPost, "/api/x", `{"a":1}`, ""},
		{"import, read again after EOF", http.MethodPost, importPath, "PK", ""},
		{"without a body", http.MethodGet, "/api/x", "", ""},
		{"a bodiless upgrade", http.MethodGet, "/", "", "websocket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bodyDone := make(chan struct{})
			release := make(chan struct{})
			srv := bodyDeadlineServer(t, timeout, func(w http.ResponseWriter, r *http.Request) {
				if tc.body != "" {
					if _, err := io.ReadAll(r.Body); err != nil {
						close(bodyDone)
						http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
						return
					}
					// A second read at EOF, as a reader that asks again does.
					_, _ = r.Body.Read(make([]byte, 1))
				}
				close(bodyDone)
				<-release
				if err := r.Context().Err(); err != nil {
					http.Error(w, "context cancelled under the handler", http.StatusInternalServerError)
					return
				}
				w.Write([]byte("done"))
			})
			go func() {
				// Any deadline was set before the handler ran, so one and a
				// half timeouts after the body is done it has fired.
				<-bodyDone
				time.Sleep(timeout * 3 / 2)
				close(release)
			}()
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.upgrade != "" {
				req.Header.Set("Upgrade", tc.upgrade)
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

// trickle sends an n-byte body one byte at a time, every gap, on path, and
// stops early once stop is closed, then keeps the connection open until stop
// is. It runs on its own goroutine, so it reports with Error.
func trickle(t *testing.T, srv *httptest.Server, path string, n int, gap time.Duration, stop <-chan struct{}) {
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	defer func() { <-stop }()
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", path, n); err != nil {
		t.Error(err)
		return
	}
	for range n {
		select {
		case <-stop:
			return
		case <-time.After(gap):
		}
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
// Mutant: dropping the per-read extension fails the import leg; applying
// it to every route fails the other.
func TestTheImportUploadIsBoundedPerReadNotInTotal(t *testing.T) {
	// leg trickles n bytes, gap apart, at a server with the given timeout,
	// and returns what the handler's read of the whole body ended with.
	leg := func(t *testing.T, path string, timeout time.Duration, n int, gap time.Duration) error {
		t.Helper()
		readErr := make(chan error, 1)
		srv := bodyDeadlineServer(t, timeout, func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			readErr <- err
		})
		stop, sent := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(sent)
			trickle(t, srv, path, n, gap, stop)
		}()
		defer func() { close(stop); <-sent }()
		select {
		case err := <-readErr:
			return err
		case <-time.After(20 * time.Second):
			t.Fatalf("the handler's read on %s never returned", path)
			return nil
		}
	}
	t.Run("import", func(t *testing.T) {
		t.Parallel()
		// 80 bytes at least 10 ms apart: longer in total than the 600 ms
		// budget, with every gap a sixtieth of it.
		if err := leg(t, importPath, 600*time.Millisecond, 80, 10*time.Millisecond); err != nil {
			t.Errorf("a slow import that kept sending failed: %v", err)
		}
	})
	t.Run("any other route", func(t *testing.T) {
		t.Parallel()
		// The budget ends 100 ms in, long before the last of 200 bytes;
		// the trickle stops as soon as the handler has its error.
		if err := leg(t, "/api/jobs", 100*time.Millisecond, 200, 10*time.Millisecond); err == nil {
			t.Error("a body slower than the whole budget was read in full on an ordinary route")
		}
	})
}

// TestAnUpgradeHeaderDoesNotExemptAStalledBody: the Upgrade header used to
// switch the deadline off whatever else the request carried, so any peer
// could send `Upgrade: websocket` with a body it never finished and pin the
// connection exactly as before. A request with that header and a body is not
// a handshake (a real one is a bodiless GET): it is answered 400 without
// reaching the upgrade handler, and the connection closes at the deadline.
//
// Mutants: skipping the deadline on the header: the handler is reached and
// the connection never closes; passing the request on with the deadline
// armed: the handler is reached.
func TestAnUpgradeHeaderDoesNotExemptAStalledBody(t *testing.T) {
	reached := make(chan struct{}, 1)
	srv := bodyDeadlineServer(t, 200*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
	})
	conn := stalledRequest(t, srv, "/", "Upgrade: websocket\r\n")

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("the server kept the connection open past the deadline: %v", err)
	}
	if !strings.HasPrefix(string(answer), "HTTP/1.1 400 ") || !strings.Contains(string(answer), `{"error":`) {
		t.Errorf("got %q, want a 400 with the JSON error body", answer)
	}
	select {
	case <-reached:
		t.Error("an upgrade request with a body reached the handler")
	default:
	}
}
