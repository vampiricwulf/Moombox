// Package httpx provides a single source of truth for the *http.Client
// shapes Moombox uses across packages. Callers pass in a timeout and
// get back a client backed by the shared keep-alive-tuned transport,
// so per-package tuning drift can't accumulate.
//
// Audit cross-cutting (bgutils DEDUP-2 + cipher DU5 + utils HTTP
// transport tuning).
package httpx

import (
	"net/http"
	"time"
)

// defaultTransport is the shared *http.Transport used by Client() and
// ClientWithTimeout(). Tuning matches utils' previous utilsHTTPClient:
//
//   - ForceAttemptHTTP2: true — match upstream YouTube/Twitch CDN
//     expectations; HTTP/2 multiplexing reduces handshake cost on
//     parallel segment fetches.
//   - MaxIdleConnsPerHost: 8 — Go's default of 2 is too low; bumping
//     to 8 lets 4-6 concurrent fetches reuse keep-alive connections
//     instead of triggering fresh handshakes.
//   - IdleConnTimeout: 90s — keep idle conns warm long enough that
//     periodic monitor/refresh ticks reuse them.
//
// Engine downloads use a custom-built transport with ParallelDownloads-
// aware MaxIdleConnsPerHost (~ParallelDownloads + 2) — see engine
// package for that. All other packages should use this default via
// Client() or ClientWithTimeout().
//
// The transport is package-private so callers can't mutate it; if a
// package needs a different shape, build a fresh Transport via
// NewTransport(opts) and pass it to ClientWithTransport.
var defaultTransport = newDefaultTransport()

func newDefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// Client returns a new *http.Client with the given timeout and the
// shared default transport. timeout=0 means "no client-level timeout"
// — the caller is expected to use ctx.WithTimeout for per-request
// cancellation.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: defaultTransport}
}

// ClientWithTransport returns a new *http.Client with the given
// timeout and a caller-supplied transport. Useful when a package
// needs MaxIdleConnsPerHost-style tuning beyond the default.
func ClientWithTransport(timeout time.Duration, transport http.RoundTripper) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}

// TransportOptions configure a NewTransport build. Zero values fall
// back to the defaults baked into newDefaultTransport().
type TransportOptions struct {
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	IdleConnTimeout       time.Duration
	TLSHandshakeTimeout   time.Duration
	ExpectContinueTimeout time.Duration
	DisableHTTP2          bool
}

// NewTransport builds a fresh *http.Transport from the given options.
// Callers that need fine-grained tuning (e.g. engine's
// ParallelDownloads-aware MaxIdleConnsPerHost) use this; everyone else
// should use Client() with the shared default.
func NewTransport(opts TransportOptions) *http.Transport {
	t := newDefaultTransport()
	if opts.MaxIdleConns > 0 {
		t.MaxIdleConns = opts.MaxIdleConns
	}
	if opts.MaxIdleConnsPerHost > 0 {
		t.MaxIdleConnsPerHost = opts.MaxIdleConnsPerHost
	}
	if opts.IdleConnTimeout > 0 {
		t.IdleConnTimeout = opts.IdleConnTimeout
	}
	if opts.TLSHandshakeTimeout > 0 {
		t.TLSHandshakeTimeout = opts.TLSHandshakeTimeout
	}
	if opts.ExpectContinueTimeout > 0 {
		t.ExpectContinueTimeout = opts.ExpectContinueTimeout
	}
	if opts.DisableHTTP2 {
		t.ForceAttemptHTTP2 = false
	}
	return t
}
