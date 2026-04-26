package httpx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClientUsesSharedTransport verifies multiple Client() calls share
// the same transport so keep-alive amortisation works across packages.
func TestClientUsesSharedTransport(t *testing.T) {
	c1 := Client(5 * time.Second)
	c2 := Client(30 * time.Second)
	if c1.Transport != c2.Transport {
		t.Error("Client(): expected shared transport across calls")
	}
}

// TestClientHonoursTimeout exercises the round-trip with a sleeping
// server and asserts the per-client timeout fires.
func TestClientHonoursTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := Client(50 * time.Millisecond)
	start := time.Now()
	_, err := c.Get(srv.URL)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Client with 50ms timeout: want error, got nil")
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("Client returned after %v — timeout did not fire promptly", elapsed)
	}
}

// TestClientZeroTimeoutDoesNotTimeoutClientLevel verifies timeout=0 is
// the documented "no client-level timeout" sentinel.
func TestClientZeroTimeoutDoesNotTimeoutClientLevel(t *testing.T) {
	c := Client(0)
	if c.Timeout != 0 {
		t.Errorf("Client(0): want Timeout=0, got %v", c.Timeout)
	}
}

// TestNewTransportOverridesDefaults verifies per-option overrides
// propagate to the returned transport.
func TestNewTransportOverridesDefaults(t *testing.T) {
	tr := NewTransport(TransportOptions{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
		DisableHTTP2:        true,
	})
	if tr.MaxIdleConnsPerHost != 4 {
		t.Errorf("MaxIdleConnsPerHost: want 4, got %d", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 30*time.Second {
		t.Errorf("IdleConnTimeout: want 30s, got %v", tr.IdleConnTimeout)
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("DisableHTTP2: ForceAttemptHTTP2 should be false")
	}
}

// TestNewTransportUsesDefaultsForZeroValues verifies a zero-valued
// option leaves the default in place rather than overwriting.
func TestNewTransportUsesDefaultsForZeroValues(t *testing.T) {
	tr := NewTransport(TransportOptions{})
	def := newDefaultTransport()
	if tr.MaxIdleConns != def.MaxIdleConns {
		t.Errorf("default MaxIdleConns: want %d, got %d", def.MaxIdleConns, tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != def.MaxIdleConnsPerHost {
		t.Errorf("default MaxIdleConnsPerHost: want %d, got %d", def.MaxIdleConnsPerHost, tr.MaxIdleConnsPerHost)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("default ForceAttemptHTTP2: want true")
	}
}

// TestClientWithTransportPropagatesCustomTransport — useful for engine's
// per-downloader transport.
func TestClientWithTransportPropagatesCustomTransport(t *testing.T) {
	custom := NewTransport(TransportOptions{MaxIdleConnsPerHost: 16})
	c := ClientWithTransport(time.Second, custom)
	if c.Transport != custom {
		t.Error("ClientWithTransport: did not propagate the custom transport")
	}
	got := c.Transport.(*http.Transport).MaxIdleConnsPerHost
	if got != 16 {
		t.Errorf("MaxIdleConnsPerHost on returned client: want 16, got %d", got)
	}
}

// TestClientRoundTripsHappyPath sanity check: a real request via the
// shared transport returns the expected body.
func TestClientRoundTripsHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, "ok")
	}))
	t.Cleanup(srv.Close)

	c := Client(5 * time.Second)
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}
