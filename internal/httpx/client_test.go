package httpx

import (
	"errors"
	"fmt"
	"net"
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

// TestClientHonoursTimeout exercises the round-trip against a server that
// never answers and asserts the per-client timeout fires: only the timeout
// can end the request, so it must, as a timeout, and no sooner than the
// timeout (timers never fire early). It was timed against 250 ms for a
// 50 ms timeout, which a 300 ms pause of the test process failed.
//
// Mutant: Client dropping its timeout argument — the request never returns
// (cut at 10 s).
func TestClientHonoursTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // until the client gives up, or the test ends
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: Close waits for the handler

	const timeout = 50 * time.Millisecond
	c := Client(timeout)
	type result struct {
		err  error
		took time.Duration
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("Get panicked: %v", r)}
			}
		}()
		start := time.Now()
		resp, err := c.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		done <- result{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		var ne net.Error
		if !errors.As(r.err, &ne) || !ne.Timeout() {
			t.Fatalf("Client with a %v timeout vs a server that never answers: err = %v, want a timeout", timeout, r.err)
		}
		if r.took < timeout {
			t.Errorf("Client gave up after %v, before its %v timeout", r.took, timeout)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Client with a %v timeout had not returned after 10 s — the timeout did not fire", timeout)
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
	// The flag alone doesn't disable h2 — net/http auto-enables it unless
	// TLSNextProto is a non-nil empty map. Assert the effective mechanism.
	if tr.TLSNextProto == nil {
		t.Error("DisableHTTP2: TLSNextProto must be a non-nil empty map to actually disable HTTP/2")
	} else if len(tr.TLSNextProto) != 0 {
		t.Errorf("DisableHTTP2: TLSNextProto should be empty, got %d entries", len(tr.TLSNextProto))
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
