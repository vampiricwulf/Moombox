package web

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type muxTestLogger struct{}

func (muxTestLogger) Debug(string, ...any) {}
func (muxTestLogger) Info(string, ...any)  {}
func (muxTestLogger) Warn(string, ...any)  {}
func (muxTestLogger) Error(string, ...any) {}

func TestSchemeRedirectHandler(t *testing.T) {
	h := schemeRedirectHandler("https")

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
	schemeRedirectHandler("http").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty host: want 400, got %d", rec.Code)
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
	go (&http.Server{Handler: schemeRedirectHandler(redirScheme)}).Serve(redirLn)

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
