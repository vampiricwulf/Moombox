package web

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// errorLineLogger records every Error line, rendered, for a server built by
// NewServer (which wants the whole logger).
type errorLineLogger struct {
	testWSLogger
	mu    sync.Mutex
	lines []string
}

func (l *errorLineLogger) Error(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := msg
	for i := 0; i+1 < len(args); i += 2 {
		line += fmt.Sprintf(" %v=%v", args[i], args[i+1])
	}
	l.lines = append(l.lines, line)
}

func (l *errorLineLogger) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// upgradeThatPanics stands in for a panic in the upgrade path ahead of the
// router: interceptUpgrades' gates, or an upgrade handler without a recover.
func upgradeThatPanics(http.ResponseWriter, *http.Request) {
	panic("boom ahead of the chain")
}

// TestAPanicAheadOfTheChainIsLoggedAndAnswered: a panic outside
// RecoveryMiddleware's reach — chi's RequestID and DrainMiddleware run ahead
// of it, interceptUpgrades' gates ahead of the router — reached net/http's
// own recover, which reports to the server's ErrorLog. That is io.Discard, so
// the panic was logged nowhere and the client saw its connection dropped.
// serverHandler, the server's handler, now has outermostRecovery around it:
// the panic is logged as RecoveryMiddleware logs one (the stack that raised
// it, the path without its query), and the client gets the 500.
//
// Mutants: serverHandler returning the handler without outermostRecovery (as
// Start built it) — the client's request fails with EOF and no line is
// logged; outermostRecovery without its log line — nothing logged; without
// its 500 — the client gets an empty 200 from net/http instead.
func TestAPanicAheadOfTheChainIsLoggedAndAnswered(t *testing.T) {
	log := &errorLineLogger{}
	s := NewServer(config.NewStore(config.Defaults(), ""), log)
	s.SetWebSocketHandler(upgradeThatPanics)
	srv := httptest.NewServer(s.serverHandler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/ws?token=QUERY-SECRET", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("the client's request failed (%v) — the panic escaped to net/http, which drops the connection", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "Internal server error") {
		t.Errorf("status %d, body %q; want the 500 RecoveryMiddleware answers", resp.StatusCode, body)
	}

	lines := log.all()
	if len(lines) != 1 {
		t.Fatalf("logged %d Error lines, want the one panic line: %q", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{"panic=boom ahead of the chain", "path=/ws", "method=GET", "stack=", ".upgradeThatPanics"} {
		if !strings.Contains(line, want) {
			t.Errorf("the panic line lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "QUERY-SECRET") {
		t.Errorf("the panic line carries the query: %s", line)
	}
}

// TestOutermostRecovery pins the wrapper's three answers: a gate that panics
// before anything was written gets the 500 and a line; a panic after the
// response began cannot be answered, so it is logged and handed back to
// net/http as http.ErrAbortHandler, which drops the connection quietly; and
// a handler that aborts on purpose with http.ErrAbortHandler passes through
// unlogged.
//
// Mutants: re-panicking the original value after a partial write — the
// "after a write" row recovers "boom after a write"; writing the 500 there
// instead — that row recovers nothing; logging http.ErrAbortHandler — the
// "deliberate abort" row logs a line.
func TestOutermostRecovery(t *testing.T) {
	serve := func(h http.Handler) (rec *httptest.ResponseRecorder, recovered any, log *panicLineLogger) {
		log = &panicLineLogger{}
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Upgrade", "websocket")
		func() {
			defer func() { recovered = recover() }()
			outermostRecovery(log, h).ServeHTTP(rec, req)
		}()
		return rec, recovered, log
	}

	t.Run("a gate that panics", func(t *testing.T) {
		// A nil store makes interceptUpgrades' IP gate panic, as a gate bug
		// would.
		rec, recovered, log := serve(interceptUpgrades(nil, http.NotFoundHandler(), func(http.ResponseWriter, *http.Request) {}))
		if recovered != nil {
			t.Fatalf("the panic escaped the wrapper: %v", recovered)
		}
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status %d, want 500", rec.Code)
		}
		if !strings.Contains(log.line, "ipAllowedByNetworkAccess") && !strings.Contains(log.line, "EffectiveClientIP") {
			t.Errorf("the line must locate the gate that panicked: %s", log.line)
		}
	})

	t.Run("after a write", func(t *testing.T) {
		rec, recovered, log := serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusSwitchingProtocols)
			panic("boom after a write")
		}))
		if recovered != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler handed back to net/http", recovered)
		}
		if rec.Code != http.StatusSwitchingProtocols {
			t.Errorf("status %d — nothing may be written over a response that began", rec.Code)
		}
		if !strings.Contains(log.line, "boom after a write") {
			t.Errorf("the panic must still be logged: %q", log.line)
		}
	})

	t.Run("a deliberate abort", func(t *testing.T) {
		_, recovered, log := serve(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}))
		if recovered != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler passed through", recovered)
		}
		if log.fields != nil {
			t.Errorf("a deliberate abort was logged: %s", log.line)
		}
	})
}

// TestTheSchemeRedirectServerRecoversAPanic: the cross-scheme redirect
// server (serveSchemeRedirect) discards its ErrorLog as the main server does,
// but had no outermostRecovery around its handler, so a panic in it reached
// net/http's own recover — logged nowhere, and the client's connection
// dropped. Its handler is now wrapped the way serverHandler wraps the main
// server's: the panic is logged and the client gets the 500.
//
// Mutants: schemeRedirectServer setting Handler to the bare handler (as
// serveSchemeRedirect built it) — the request fails with EOF and no line is
// logged; serveSchemeRedirect building its own server again rather than
// schemeRedirectServer's — the panic escapes the published server's handler.
func TestTheSchemeRedirectServerRecoversAPanic(t *testing.T) {
	log := &errorLineLogger{}
	s := NewServer(config.NewStore(config.Defaults(), ""), log)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = s.schemeRedirectServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom in the redirect")
	}))
	ts.Start()
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/jobs?token=QUERY-SECRET")
	if err != nil {
		t.Fatalf("the client's request failed (%v) — the panic escaped to net/http, which drops the connection", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "Internal server error") {
		t.Errorf("status %d, body %q; want the 500", resp.StatusCode, body)
	}
	lines := log.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "panic=boom in the redirect") || !strings.Contains(lines[0], "path=/jobs") {
		t.Fatalf("logged %q, want the one panic line", lines)
	}
	if strings.Contains(lines[0], "QUERY-SECRET") {
		t.Errorf("the panic line carries the query: %s", lines[0])
	}

	// And serveSchemeRedirect runs the server this builds: a writer whose
	// first Header() call panics stands in for a panic inside the redirect
	// handler, served through the server serveSchemeRedirect published.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.serveSchemeRedirect(ln, "https")
	}()
	defer func() { ln.Close(); <-served }()
	var srv *http.Server
	for deadline := time.Now().Add(10 * time.Second); srv == nil; {
		if srv = s.redirectServer.Load(); srv == nil {
			if time.Now().After(deadline) {
				t.Fatal("serveSchemeRedirect never published its server")
			}
			time.Sleep(time.Millisecond)
		}
	}
	w := &headerPanicsOnce{ResponseWriter: httptest.NewRecorder()}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("the panic escaped serveSchemeRedirect's handler: %v", r)
			}
		}()
		srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.local:774/x", nil))
	}()
	if lines := log.all(); len(lines) != 2 || !strings.Contains(lines[1], "panic=a writer that panics") {
		t.Errorf("logged %q, want a second panic line from serveSchemeRedirect's server", lines)
	}
}

// headerPanicsOnce is a ResponseWriter whose first Header() call panics.
type headerPanicsOnce struct {
	http.ResponseWriter
	called bool
}

func (w *headerPanicsOnce) Header() http.Header {
	if !w.called {
		w.called = true
		panic("a writer that panics")
	}
	return w.ResponseWriter.Header()
}
