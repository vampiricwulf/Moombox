package web

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type muxTestLogger struct{}

func (muxTestLogger) Debug(string, ...any) {}
func (muxTestLogger) Info(string, ...any)  {}
func (muxTestLogger) Warn(string, ...any)  {}
func (muxTestLogger) Error(string, ...any) {}

func TestSchemeRedirectHandler(t *testing.T) {
	h := schemeRedirectHandler("https", "774")

	req := httptest.NewRequest("GET", "http://example.local:774/tasks?filter=live", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status: want 307, got %d", rec.Code)
	}
	want := "https://example.local:774/tasks?filter=live"
	if loc := rec.Header().Get("Location"); loc != want {
		t.Errorf("Location: want %q, got %q", want, loc)
	}

	// Empty Host can't produce a valid Location.
	req = httptest.NewRequest("GET", "/x", nil)
	req.Host = ""
	rec = httptest.NewRecorder()
	schemeRedirectHandler("http", "774").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty host: want 400, got %d", rec.Code)
	}
}

// TestSchemeRedirectToHTTPClearsHSTS: with https on, every response pins
// the host for a year, and a browser that trusted the certificate keeps the
// pin after https is turned off — it upgrades http to https, the redirect
// sends it back to http, and the two loop. The redirect down clears the pin
// (it is served over TLS, so the browser honours it); the redirect up must
// not set one of its own.
//
// Mutant: drop the max-age=0 header — the first check fails.
func TestSchemeRedirectToHTTPClearsHSTS(t *testing.T) {
	rec := httptest.NewRecorder()
	schemeRedirectHandler("http", "774").ServeHTTP(rec, httptest.NewRequest("GET", "https://example.local:774/", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=0" {
		t.Errorf("https→http redirect HSTS = %q, want max-age=0", got)
	}
	rec = httptest.NewRecorder()
	schemeRedirectHandler("https", "774").ServeHTTP(rec, httptest.NewRequest("GET", "http://example.local:774/", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("http→https redirect set HSTS %q; a plain-http response cannot", got)
	}
}

// TestSchemeRedirectHandlerDefaultPorts pins the port-80/443 deployment
// behavior: browsers omit the source scheme's default port from Host, but
// the same socket serves both schemes — the redirect must re-pin OUR port
// whenever it isn't the target scheme's default, or it points at a port
// nothing listens on.
func TestSchemeRedirectHandlerDefaultPorts(t *testing.T) {
	cases := []struct {
		name         string
		targetScheme string
		listenPort   string
		host         string
		want         string
	}{
		{"port 80, http→https pins :80", "https", "80", "example.local", "https://example.local:80/x"},
		{"port 443, https→http pins :443", "http", "443", "example.local", "http://example.local:443/x"},
		{"port 80, https→http stays bare", "http", "80", "example.local", "http://example.local/x"},
		{"explicit port kept verbatim", "https", "774", "example.local:774", "https://example.local:774/x"},
		{"ipv6 host re-pinned with brackets", "https", "80", "[::1]", "https://[::1]:80/x"},
		{"unknown listen port trusts host", "https", "", "example.local", "https://example.local/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://placeholder/x", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			schemeRedirectHandler(tc.targetScheme, tc.listenPort).ServeHTTP(rec, req)
			if loc := rec.Header().Get("Location"); loc != tc.want {
				t.Errorf("Location: want %q, got %q", tc.want, loc)
			}
		})
	}
}

// noFollow returns a client that surfaces redirects instead of following them.
func noFollow(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// startMuxTopology builds a scheme mux over a fresh loopback listener with
// the main handler on mainLn and the redirect server on redirLn, returning
// the address. tlsMain selects which branch carries TLS.
func startMuxTopology(t *testing.T, tlsMain bool) string {
	t.Helper()

	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { real.Close() })

	dir := t.TempDir()
	tlsCfg, err := LoadOrGenerateTLSConfig(
		filepath.Join(dir, "test.crt"), filepath.Join(dir, "test.key"),
		"localhost", muxTestLogger{})
	if err != nil {
		t.Fatalf("test cert: %v", err)
	}

	tlsRaw, plainRaw := newSchemeMux(real, muxTestLogger{})

	mainHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("main"))
	})

	var mainLn, redirLn net.Listener
	var redirScheme string
	if tlsMain {
		mainLn = tls.NewListener(tlsRaw, tlsCfg)
		redirLn, redirScheme = plainRaw, "https"
	} else {
		mainLn = plainRaw
		redirLn, redirScheme = tls.NewListener(tlsRaw, tlsCfg), "http"
	}

	go (&http.Server{Handler: mainHandler}).Serve(mainLn)
	go (&http.Server{Handler: schemeRedirectHandler(redirScheme, listenerPort(redirLn))}).Serve(redirLn)

	return real.Addr().String()
}

func TestSchemeMuxHTTPSEnabledRedirectsPlainHTTP(t *testing.T) {
	addr := startMuxTopology(t, true)

	// Plain HTTP hits the redirect branch.
	resp, err := noFollow(nil).Get("http://" + addr + "/dash?a=1")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("plain GET: want 307, got %d", resp.StatusCode)
	}
	if want := "https://" + addr + "/dash?a=1"; resp.Header.Get("Location") != want {
		t.Errorf("Location: want %q, got %q", want, resp.Header.Get("Location"))
	}

	// TLS reaches the main handler.
	tlsClient := noFollow(&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}})
	resp2, err := tlsClient.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("tls GET: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK || string(body) != "main" {
		t.Errorf("tls GET: want 200 'main', got %d %q", resp2.StatusCode, body)
	}
}

func TestSchemeMuxHTTPSDisabledRedirectsTLS(t *testing.T) {
	addr := startMuxTopology(t, false)

	// TLS hits the redirect branch (terminated with the leftover cert).
	tlsClient := noFollow(&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}})
	resp, err := tlsClient.Get("https://" + addr + "/jobs")
	if err != nil {
		t.Fatalf("tls GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("tls GET: want 307, got %d", resp.StatusCode)
	}
	if want := "http://" + addr + "/jobs"; resp.Header.Get("Location") != want {
		t.Errorf("Location: want %q, got %q", want, resp.Header.Get("Location"))
	}

	// Plain HTTP reaches the main handler.
	resp2, err := noFollow(nil).Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK || string(body) != "main" {
		t.Errorf("plain GET: want 200 'main', got %d %q", resp2.StatusCode, body)
	}
}

// tempAcceptErr is an accept error whose Temporary() is true — the shape
// net/http's Serve loop retries. On a 24/7 downloader the realistic producers
// are EMFILE / ENFILE / WSAEMFILE; poll.FD.Accept already swallows
// ECONNABORTED internally.
type tempAcceptErr struct{}

func (tempAcceptErr) Error() string   { return "temporary accept failure" }
func (tempAcceptErr) Timeout() bool   { return false }
func (tempAcceptErr) Temporary() bool { return true }

// flakyListener yields `remaining` temporary accept errors and then delegates
// every later Accept to the real listener.
type flakyListener struct {
	net.Listener
	remaining atomic.Int32
	attempts  atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.attempts.Add(1)
	if l.remaining.Add(-1) >= 0 {
		return nil, tempAcceptErr{}
	}
	return l.Listener.Accept()
}

// TestSchemeMuxRetriesATemporaryAcceptError is the WEB-3 pin (O-N).
//
// The sniff loop returned on ANY Accept error, so one transient EMFILE ended
// it, close(done) made every muxedListener.Accept answer net.ErrClosed, both
// http.Servers returned, and the dashboard was dead until the process
// restarted — while net/http's own Serve loop would have slept 5 ms and
// carried on. Only https_enabled installs (and ones with a leftover cert pair)
// reach this code at all.
//
// THE MUTANT: restore the bare `return` on any error. The first Accept fails,
// the loop ends, and the GET below never gets a response — the request hangs
// until the client's 5 s timeout, which is the failure this test reports.
func TestSchemeMuxRetriesATemporaryAcceptError(t *testing.T) {
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { real.Close() })

	flaky := &flakyListener{Listener: real}
	flaky.remaining.Store(1)

	_, plainRaw := newSchemeMux(flaky, muxTestLogger{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("main"))
	})}
	go srv.Serve(plainRaw)
	t.Cleanup(func() { srv.Close() })

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + real.Addr().String() + "/x")
	if err != nil {
		t.Fatalf("GET after one temporary accept error: %v — the loop gave up instead of retrying "+
			"(net/http's Serve sleeps 5ms and continues)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if n := flaky.attempts.Load(); n < 2 {
		t.Errorf("Accept attempts: want >= 2 (the failure plus the retry), got %d", n)
	}
}

// TestSchemeMuxStopsOnAPermanentAcceptError is the other half: a closed
// listener must still end the loop rather than spin. MUTANT: retry EVERY
// error, not just the temporary ones — Close() would never stop the goroutine
// and shutdown would spin at 1 Hz forever.
func TestSchemeMuxStopsOnAPermanentAcceptError(t *testing.T) {
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsRaw, plainRaw := newSchemeMux(real, muxTestLogger{})
	real.Close()

	done := make(chan error, 1)
	go func() {
		_, err := plainRaw.Accept()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept on a torn-down mux: want an error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the sniff loop never exited after the real listener closed — a permanent error is " +
			"being retried")
	}
	tlsRaw.Close()
}

// muxFakeAddr is the address of the scripted listeners below — they never bind
// a socket, but muxedListener publishes real.Addr() to both branches.
type muxFakeAddr struct{}

func (muxFakeAddr) Network() string { return "tcp" }
func (muxFakeAddr) String() string  { return "127.0.0.1:0" }

// alwaysTempListener fails every Accept with a temporary error and does NOT
// notice its own Close — so the only thing that can end the accept loop is the
// loop's own cancellation of the backoff wait.
type alwaysTempListener struct {
	attempted chan struct{}
}

func (l *alwaysTempListener) Accept() (net.Conn, error) {
	select {
	case l.attempted <- struct{}{}:
	default:
	}
	return nil, tempAcceptErr{}
}

func (l *alwaysTempListener) Close() error   { return nil }
func (l *alwaysTempListener) Addr() net.Addr { return muxFakeAddr{} }

// TestSchemeMuxCloseDuringBackoffEndsTheLoop pins the shutdown half of the
// backoff: a Close landing mid-sleep ends the accept goroutine promptly
// instead of after up to acceptRetryMaxDelay — and, when the listener keeps
// producing temporary errors, ends it AT ALL.
//
// THE MUTANT: back off with a plain time.Sleep(retryDelay) that ignores the
// close signal. The loop wakes, calls Accept, gets another temporary error and
// sleeps again forever; `exited` never fires and this test reports the 2 s
// timeout.
func TestSchemeMuxCloseDuringBackoffEndsTheLoop(t *testing.T) {
	ln := &alwaysTempListener{attempted: make(chan struct{}, 1)}
	tlsRaw, plainRaw := newSchemeMux(ln, muxTestLogger{})

	exited := make(chan struct{})
	go func() {
		plainRaw.Accept() // returns once the loop closes done
		close(exited)
	}()

	// Let the ladder climb a rung or two so the Close lands inside a sleep
	// rather than between two Accepts.
	for i := 0; i < 3; i++ {
		select {
		case <-ln.attempted:
		case <-time.After(2 * time.Second):
			t.Fatal("the accept loop stopped retrying the temporary error")
		}
	}
	tlsRaw.Close()

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() during the backoff never ended the accept loop — the wait is not cancellable")
	}
}

// muxDiscardConn is a connection the sniffer immediately throws away: the
// first read returns EOF, so the per-connection goroutine closes it and
// returns without forwarding it to either branch. It stands in for a
// successful Accept in the ladder script.
type muxDiscardConn struct{}

func (muxDiscardConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (muxDiscardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (muxDiscardConn) Close() error                     { return nil }
func (muxDiscardConn) LocalAddr() net.Addr              { return muxFakeAddr{} }
func (muxDiscardConn) RemoteAddr() net.Addr             { return muxFakeAddr{} }
func (muxDiscardConn) SetDeadline(time.Time) error      { return nil }
func (muxDiscardConn) SetReadDeadline(time.Time) error  { return nil }
func (muxDiscardConn) SetWriteDeadline(time.Time) error { return nil }

// scriptedListener plays a fixed sequence of Accept outcomes — true = a
// temporary error, false = a connection — and then answers net.ErrClosed
// forever, the permanent error that ends the loop.
type scriptedListener struct {
	mu     sync.Mutex
	script []bool
	idx    int
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.idx >= len(l.script) {
		return nil, net.ErrClosed
	}
	temporary := l.script[l.idx]
	l.idx++
	if temporary {
		return nil, tempAcceptErr{}
	}
	return muxDiscardConn{}, nil
}

func (l *scriptedListener) Close() error   { return nil }
func (l *scriptedListener) Addr() net.Addr { return muxFakeAddr{} }

// TestSchemeMuxAcceptRetryLadder pins the backoff ladder itself against
// net/http's Serve: 5 ms on the first temporary error, doubling, capped at
// 1 s, back to zero after a successful Accept. The sleep is injected, so the
// whole table runs without waiting out a single rung.
//
// THE MUTANTS, one per row: replace `retryDelay *= 2` with
// `retryDelay = acceptRetryMinDelay` (row 1 sees 5 ms forever); drop the
// `min(retryDelay, acceptRetryMaxDelay)` clamp (row 1's last two rungs come
// back 1.28 s / 2.56 s); delete the `retryDelay = 0` after a successful
// Accept (row 2's third rung comes back 20 ms instead of 5 ms); back off on
// every error instead of only temporary ones (row 3 sleeps on the terminal
// net.ErrClosed instead of returning).
func TestSchemeMuxAcceptRetryLadder(t *testing.T) {
	cases := []struct {
		name string
		// script: true = a temporary accept error, false = a connection.
		script []bool
		want   []time.Duration
	}{
		{
			name:   "doubles from 5ms and holds at the 1s ceiling",
			script: []bool{true, true, true, true, true, true, true, true, true, true},
			want: []time.Duration{
				5 * time.Millisecond,
				10 * time.Millisecond,
				20 * time.Millisecond,
				40 * time.Millisecond,
				80 * time.Millisecond,
				160 * time.Millisecond,
				320 * time.Millisecond,
				640 * time.Millisecond,
				1 * time.Second,
				1 * time.Second,
			},
		},
		{
			name:   "a successful Accept resets the ladder",
			script: []bool{true, true, false, true},
			want: []time.Duration{
				5 * time.Millisecond,
				10 * time.Millisecond,
				5 * time.Millisecond,
			},
		},
		{
			name:   "a permanent error never sleeps",
			script: []bool{false},
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var got []time.Duration
			sleep := func(d time.Duration, cancel <-chan struct{}) bool {
				mu.Lock()
				got = append(got, d)
				mu.Unlock()
				return true // the ladder is the assertion; never actually wait
			}

			_, plainRaw := newSchemeMuxWithRetryWait(&scriptedListener{script: tc.script}, muxTestLogger{}, sleep)

			// The script ends in net.ErrClosed, so the loop returns and
			// every muxedListener.Accept answers ErrClosed — that return is
			// the signal that the script has been played out.
			exited := make(chan struct{})
			go func() {
				plainRaw.Accept()
				close(exited)
			}()
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				t.Fatal("the accept loop never reached the end of the script")
			}

			mu.Lock()
			defer mu.Unlock()
			if len(got) != len(tc.want) {
				t.Fatalf("backoff rungs: want %v, got %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("rung %d: want %v, got %v (full ladder %v)", i, tc.want[i], got[i], got)
				}
			}
		})
	}
}
