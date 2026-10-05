package web

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{
			name:       "IPv4 with port",
			remoteAddr: "192.168.1.1:12345",
			expected:   "192.168.1.1",
		},
		{
			name:       "IPv4 without port",
			remoteAddr: "192.168.1.1",
			expected:   "192.168.1.1",
		},
		{
			name:       "IPv6 loopback with port",
			remoteAddr: "[::1]:8080",
			expected:   "::1",
		},
		{
			name:       "IPv6 full with port",
			remoteAddr: "[2001:db8::1]:443",
			expected:   "2001:db8::1",
		},
		{
			name:       "localhost with port",
			remoteAddr: "127.0.0.1:9090",
			expected:   "127.0.0.1",
		},
		{
			name:       "does not trust X-Forwarded-For",
			remoteAddr: "10.0.0.1:1234",
			expected:   "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{
				RemoteAddr: tt.remoteAddr,
				Header:     http.Header{},
			}
			// Set X-Forwarded-For to prove it's ignored
			r.Header.Set("X-Forwarded-For", "99.99.99.99")
			r.Header.Set("X-Real-IP", "88.88.88.88")

			result := ExtractIP(r)
			if result != tt.expected {
				t.Errorf("ExtractIP(RemoteAddr=%q) = %q, expected %q", tt.remoteAddr, result, tt.expected)
			}
		})
	}
}

func TestIsAllowedOrigin(t *testing.T) {
	tests := []struct {
		name          string
		origin        string
		networkAccess string
		// host/scheme describe the request the Origin arrived on. Empty means
		// the default fixture in the runner — every localhost/lan row predates
		// the same-host rule and must keep passing without naming a host.
		host   string
		scheme string
		// identity is the certificate-attested host list. nil means "no
		// certificate", which is what every pre-existing row wants.
		identity []string
		expected bool
	}{
		{
			name:          "local mode allows localhost",
			origin:        "http://localhost:3000",
			networkAccess: "localhost",
			expected:      true,
		},
		{
			name:          "local mode allows 127.0.0.1",
			origin:        "http://127.0.0.1:8080",
			networkAccess: "localhost",
			expected:      true,
		},
		{
			name:          "local mode rejects LAN IP",
			origin:        "http://192.168.1.100:3000",
			networkAccess: "localhost",
			expected:      false,
		},
		{
			name:          "local mode rejects external",
			origin:        "http://example.com",
			networkAccess: "localhost",
			expected:      false,
		},
		{
			name:          "lan mode allows localhost",
			origin:        "http://localhost:3000",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows 127.0.0.1",
			origin:        "http://127.0.0.1:3000",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows private IP 192.168",
			origin:        "http://192.168.1.50:8080",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows private IP 10.x",
			origin:        "http://10.0.0.5:8080",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows private IP 172.16",
			origin:        "http://172.16.0.1:8080",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode rejects external",
			origin:        "http://example.com",
			networkAccess: "lan",
			expected:      false,
		},
		{
			name:          "default (empty) mode allows localhost",
			origin:        "http://localhost:3000",
			networkAccess: "",
			expected:      true,
		},
		{
			name:          "default (empty) mode rejects external",
			origin:        "http://example.com",
			networkAccess: "",
			expected:      false,
		},
		{
			name:          "invalid origin URL",
			origin:        "://bad",
			networkAccess: "public",
			expected:      false,
		},
		{
			name:          "empty origin",
			origin:        "",
			networkAccess: "public",
			expected:      false,
		},
		// The same-host rows (sweep T1-6), and the mutants each one kills:
		// restoring `case "external", "public": return true` in isAllowedOrigin
		// fails all four `false` rows below; dropping the port comparison in
		// sameSiteOrigin fails the ":8080" row; dropping the net.ParseIP
		// canonicalisation in splitAuthority fails the IPv6 row; comparing with
		// strings.HasSuffix instead of equality fails the "evil-dash" row.
		{
			name:          "public mode rejects a foreign origin",
			origin:        "http://example.com",
			networkAccess: "public",
			host:          "dash.example",
			expected:      false,
		},
		{
			name:          "public mode rejects an external HTTPS origin",
			origin:        "https://evil.example.org",
			networkAccess: "public",
			host:          "dash.example",
			expected:      false,
		},
		{
			name:          "external mode allows the request's own host",
			origin:        "http://dash.example",
			networkAccess: "external",
			host:          "dash.example",
			expected:      true,
		},
		{
			name:          "external mode allows a TLS-terminated portless pair",
			origin:        "https://dash.example",
			networkAccess: "external",
			host:          "dash.example", // proxy forwarded the client's Host verbatim
			expected:      true,
		},
		{
			name:          "external mode compares ports once either side names one",
			origin:        "http://dash.example:8080",
			networkAccess: "external",
			host:          "dash.example:774",
			expected:      false,
		},
		{
			name:          "external mode matches an IPv6 literal whatever its spelling",
			origin:        "http://[0:0:0:0:0:0:0:1]:774",
			networkAccess: "external",
			host:          "[::1]:774",
			expected:      true,
		},
		{
			name:          "external mode rejects a host that merely shares a suffix",
			origin:        "http://evil-dash.example",
			networkAccess: "external",
			host:          "dash.example",
			expected:      false,
		},
		// The certificate-identity rows (chain-close O3a), and the mutants each
		// one kills: making the identity arm REPLACE sameSiteOrigin instead of
		// conjoining it fails the ":8080" row; dropping hostInSANs from the
		// external arm fails the rebinding row; denying when identity is empty
		// fails the certless row; dropping the "*." clause fails the wildcard
		// row; letting the wildcard span a dot fails the two-label row;
		// dropping hostInSANs from the lan arm fails the "dash.lan" row.
		{
			name:          "external mode allows a certificate-attested host",
			origin:        "http://dash.example",
			networkAccess: "external",
			host:          "dash.example",
			identity:      []string{"dash.example"},
			expected:      true,
		},
		{
			name:          "external mode refuses a rebinding page once a certificate names the deployment",
			origin:        "http://attacker.dns",
			networkAccess: "external",
			host:          "attacker.dns", // the rebinding page controls BOTH
			identity:      []string{"dash.example"},
			expected:      false,
		},
		{
			name:          "external mode without a certificate keeps the same-host rule alone",
			origin:        "http://attacker.dns",
			networkAccess: "external",
			host:          "attacker.dns",
			identity:      nil,
			expected:      true,
		},
		{
			name:          "external mode still compares ports with a certificate present",
			origin:        "http://dash.example:8080",
			networkAccess: "external",
			host:          "dash.example:774",
			identity:      []string{"dash.example"},
			expected:      false,
		},
		{
			name:          "external mode still allows a TLS-terminated portless pair",
			origin:        "https://dash.example",
			networkAccess: "external",
			host:          "dash.example",
			identity:      []string{"dash.example"},
			expected:      true,
		},
		{
			name:          "external mode expands a wildcard SAN by one label",
			origin:        "https://dash.example.com",
			networkAccess: "external",
			host:          "dash.example.com",
			identity:      []string{"*.example.com"},
			expected:      true,
		},
		{
			name:          "external mode refuses a wildcard SAN spanning two labels",
			origin:        "https://a.b.example.com",
			networkAccess: "external",
			host:          "a.b.example.com",
			identity:      []string{"*.example.com"},
			expected:      false,
		},
		{
			name:          "lan mode allows a certificate-attested name",
			origin:        "https://dash.lan",
			networkAccess: "lan",
			identity:      []string{"dash.lan"},
			expected:      true,
		},
		{
			name:          "lan mode still refuses a bare name with no certificate",
			origin:        "https://dash.lan",
			networkAccess: "lan",
			identity:      nil,
			expected:      false,
		},
		{
			name:          "localhost mode is unchanged when no certificate is loaded",
			origin:        "http://localhost",
			networkAccess: "localhost",
			identity:      nil,
			expected:      true,
		},
		// Fix-round-1 item 1 (review Finding 1 / probe P7): the wildcard clause
		// must NOT reach the localhost/lan/default WIDENING arms, only the
		// external/public conjunction where sameSiteOrigin already pins the
		// host. Mutant: re-enable wildcard expansion in the widening arms
		// (drop the allowWildcard=false argument, or pass true) — the refused
		// row below starts returning true.
		{
			name:          "localhost mode does not expand a wildcard SAN (review P7)",
			origin:        "https://evil.example.com",
			networkAccess: "localhost",
			identity:      []string{"*.example.com"},
			expected:      false,
		},
		{
			name:          "lan mode still allows a literal certificate-attested name (review P7)",
			origin:        "https://dash.lan",
			networkAccess: "lan",
			identity:      []string{"dash.lan"},
			expected:      true,
		},
		// Final review Finding 3: the row above only pinned the localhost arm
		// (:369) against wildcard expansion; the lan arm (middleware.go:372)
		// and the unset-default arm (middleware.go:382) had no such row, so
		// each arm's hostInSANs(..., false) survived a mutant flipping it to
		// true against the committed suite. These two rows close that gap.
		{
			// Mutant: middleware.go:372, hostInSANs(hostname, identity, false)
			// -> hostInSANs(hostname, identity, true).
			name:          "lan mode does not expand a wildcard SAN (review P7)",
			origin:        "https://evil.example.com",
			networkAccess: "lan",
			identity:      []string{"*.example.com"},
			expected:      false,
		},
		{
			// Mutant: middleware.go:382, hostInSANs(hostname, identity, false)
			// -> hostInSANs(hostname, identity, true).
			name:          "unset default mode does not expand a wildcard SAN (review P7)",
			origin:        "https://evil.example.com",
			networkAccess: "",
			identity:      []string{"*.example.com"},
			expected:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := tt.host
			if host == "" {
				host = "127.0.0.1:774"
			}
			scheme := tt.scheme
			if scheme == "" {
				scheme = "http"
			}
			result := isAllowedOrigin(tt.origin, tt.networkAccess, host, scheme, tt.identity)
			if result != tt.expected {
				t.Errorf("isAllowedOrigin(%q, %q, host=%q, scheme=%q, identity=%v) = %v, expected %v",
					tt.origin, tt.networkAccess, host, scheme, tt.identity, result, tt.expected)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		{name: "127.0.0.1", ip: "127.0.0.1", expected: true},
		{name: "127.0.0.2", ip: "127.0.0.2", expected: true},
		{name: "::1", ip: "::1", expected: true},
		{name: "localhost string", ip: "localhost", expected: true},
		{name: "192.168.1.1 not loopback", ip: "192.168.1.1", expected: false},
		{name: "10.0.0.1 not loopback", ip: "10.0.0.1", expected: false},
		{name: "8.8.8.8 not loopback", ip: "8.8.8.8", expected: false},
		{name: "empty string", ip: "", expected: false},
		{name: "garbage", ip: "not-an-ip", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isLoopback(tt.ip)
			if result != tt.expected {
				t.Errorf("isLoopback(%q) = %v, expected %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		{name: "10.0.0.1 (class A)", ip: "10.0.0.1", expected: true},
		{name: "10.255.255.255 (class A end)", ip: "10.255.255.255", expected: true},
		{name: "172.16.0.1 (class B start)", ip: "172.16.0.1", expected: true},
		{name: "172.31.255.255 (class B end)", ip: "172.31.255.255", expected: true},
		{name: "172.32.0.1 (outside class B)", ip: "172.32.0.1", expected: false},
		{name: "192.168.0.1 (class C)", ip: "192.168.0.1", expected: true},
		{name: "192.168.255.255 (class C end)", ip: "192.168.255.255", expected: true},
		{name: "127.0.0.1 (loopback counts as private)", ip: "127.0.0.1", expected: true},
		{name: "::1 (IPv6 loopback counts as private)", ip: "::1", expected: true},
		{name: "8.8.8.8 (public)", ip: "8.8.8.8", expected: false},
		{name: "1.1.1.1 (public)", ip: "1.1.1.1", expected: false},
		{name: "fc00::1 (IPv6 private)", ip: "fc00::1", expected: true},
		{name: "fd00::1 (IPv6 private)", ip: "fd00::1", expected: true},
		{name: "2001:db8::1 (IPv6 public)", ip: "2001:db8::1", expected: false},
		{name: "fe80::1 (IPv6 link-local)", ip: "fe80::1", expected: true},
		{name: "fe80::a1b2:c3d4 (IPv6 link-local)", ip: "fe80::a1b2:c3d4", expected: true},
		{name: "169.254.1.1 (IPv4 link-local)", ip: "169.254.1.1", expected: true},
		{name: "169.254.255.255 (IPv4 link-local end)", ip: "169.254.255.255", expected: true},
		{name: "invalid IP", ip: "not-an-ip", expected: false},
		{name: "empty string", ip: "", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isPrivateIP(tt.ip)
			if result != tt.expected {
				t.Errorf("isPrivateIP(%q) = %v, expected %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestShouldSkipCompression(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{
			name:     "video endpoint",
			path:     "/api/jobs/abc123/video",
			expected: true,
		},
		{
			name:     "video endpoint with different job ID",
			path:     "/api/jobs/xyz-789/video",
			expected: true,
		},
		{
			name:     "jobs list endpoint",
			path:     "/api/jobs",
			expected: false,
		},
		{
			name:     "job detail (not video)",
			path:     "/api/jobs/abc123",
			expected: false,
		},
		{
			name:     "status endpoint",
			path:     "/api/status",
			expected: false,
		},
		{
			name:     "root path",
			path:     "/",
			expected: false,
		},
		{
			name:     "video suffix but wrong prefix",
			path:     "/other/jobs/abc123/video",
			expected: false,
		},
		{
			name:     "jobs prefix but no video suffix",
			path:     "/api/jobs/abc123/logs",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldSkipCompression(tt.path)
			if result != tt.expected {
				t.Errorf("shouldSkipCompression(%q) = %v, expected %v", tt.path, result, tt.expected)
			}
		})
	}
}

// A Range request against a chat route answers 206 with a Content-Range
// describing the exact byte span of the underlying resource; chat routes
// are NOT in shouldSkipCompression's /video exemption, so they are wrapped
// by the gzip middleware like any other JSON response. Compressing a 206
// would pair Content-Encoding: gzip with a Content-Range computed on the
// identity encoding — a malformed response the client cannot decode against
// the byte range it asked for (R16). The compression middleware must commit
// a 206 response uncompressed regardless of size or Accept-Encoding.
func TestCompressionMiddlewareNeverCompresses206(t *testing.T) {
	body := strings.Repeat("range-body-byte-", 200) // > gzipMinSize (1024 bytes)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3199/5000")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(body))
	})

	req := httptest.NewRequest("GET", "/api/jobs/abc/chat", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	CompressionMiddleware(handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("want 206, got %d", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want none on a 206", enc)
	}
	if rec.Body.String() != body {
		t.Errorf("body mismatch: got %d bytes, want %d bytes", rec.Body.Len(), len(body))
	}
}

// The 206 rule above is enforced inside WriteHeader, which every other status
// passes through as well — so a guard that also matched 200 would silently
// stop compressing every JSON response the dashboard loads, and nothing in
// this package would notice. Pin the ordinary path: a response over
// gzipMinSize whose client accepts gzip arrives gzipped, and decodes back to
// exactly the bytes the handler wrote.
func TestCompressionMiddlewareCompressesLargeOK(t *testing.T) {
	body := strings.Repeat("compressible-json-byte-", 100) // 2300 bytes > gzipMinSize (1024)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	})

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	CompressionMiddleware(handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip on a %d-byte 200", enc, len(body))
	}
	if rec.Body.Len() >= len(body) {
		t.Errorf("wire body is %d bytes, not smaller than the %d it encodes", rec.Body.Len(), len(body))
	}
	// T4 made ?v= assets public+immutable with ETags, so a SHARED cache may now
	// store this body. Without Vary it would hand the gzipped copy to the next
	// client that negotiated identity, which cannot decode it.
	// THE MUTANT: drop the Vary line from CompressionMiddleware.
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to name Accept-Encoding on a content-negotiated response", v)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if string(got) != body {
		t.Errorf("decoded body = %d bytes, want the original %d", len(got), len(body))
	}
}

func TestIsLoopbackRequest(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   bool
	}{
		{
			name:       "loopback IPv4",
			remoteAddr: "127.0.0.1:8080",
			expected:   true,
		},
		{
			name:       "loopback IPv6",
			remoteAddr: "[::1]:8080",
			expected:   true,
		},
		{
			name:       "LAN IP",
			remoteAddr: "192.168.1.1:8080",
			expected:   false,
		},
		{
			name:       "public IP",
			remoteAddr: "8.8.8.8:1234",
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{
				RemoteAddr: tt.remoteAddr,
				Header:     http.Header{},
			}
			result := IsLoopbackRequest(r)
			if result != tt.expected {
				t.Errorf("IsLoopbackRequest(RemoteAddr=%q) = %v, expected %v",
					tt.remoteAddr, result, tt.expected)
			}
		})
	}
}

// TestCSRFMiddleware exercises the tightened CSRF policy (audit web.md C-1/C-5/C-8).
// Mutating requests must present an allowed Origin/Referer OR the in-process
// internal token; missing Origin on loopback no longer passes.
func TestCSRFMiddleware(t *testing.T) {
	const internalToken = "test-internal-token"

	makeStore := func(networkAccess string) *config.Store {
		cfg := &config.MoomboxConfig{
			Network: config.NetworkConfig{NetworkAccess: networkAccess},
		}
		return config.NewStore(cfg, "")
	}

	makeRequest := func(method, path string, headers map[string]string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(""))
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	passHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	tests := []struct {
		name          string
		networkAccess string
		method        string
		path          string
		headers       map[string]string
		wantStatus    int
		wantReasonSub string // substring expected in body on reject
	}{
		{
			name:          "GET passes without Origin",
			networkAccess: "localhost",
			method:        http.MethodGet,
			path:          "/api/jobs",
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "HEAD passes without Origin",
			networkAccess: "localhost",
			method:        http.MethodHead,
			path:          "/api/jobs",
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "POT /get_pot exempt from CSRF",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/get_pot",
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "POT /invalidate_caches exempt",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/invalidate_caches",
			wantStatus:    http.StatusNoContent,
		},
		// A browser always sends Origin on a POST, so the POT exemption holds
		// only for a caller that names no origin (yt-dlp, curl). Exempt by
		// path alone, a page in the operator's browser could POST to
		// 127.0.0.1 cross-site and drop the PO-token caches at will.
		// Mutant: the exemption without the Origin/Referer test — 204.
		{
			name:          "POT /invalidate_caches from a cross-site page rejected",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/invalidate_caches",
			headers:       map[string]string{"Origin": "https://evil.example"},
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "POT /get_pot from a cross-site page rejected",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/get_pot",
			headers:       map[string]string{"Origin": "https://evil.example"},
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "POT /invalidate_it with a cross-site Referer rejected",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/invalidate_it",
			headers:       map[string]string{"Referer": "https://evil.example/page"},
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "InternalToken header bypasses CSRF on mutating request",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/restart",
			headers:       map[string]string{InternalTokenHeader: internalToken},
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "Wrong InternalToken falls back to Origin check; missing Origin rejected",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/restart",
			headers:       map[string]string{InternalTokenHeader: "wrong-token"},
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "POST without Origin on localhost rejected (tightened policy)",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/jobs",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "PUT without Origin on localhost rejected",
			networkAccess: "localhost",
			method:        http.MethodPut,
			path:          "/api/config",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "DELETE without Origin on localhost rejected",
			networkAccess: "localhost",
			method:        http.MethodDelete,
			path:          "/api/jobs/abc",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "POST with allowed localhost Origin passes",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/jobs",
			headers:       map[string]string{"Origin": "http://localhost:774"},
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "POST with 127.0.0.1 Origin passes",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/jobs",
			headers:       map[string]string{"Origin": "http://127.0.0.1:774"},
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "POST with evil.com Origin rejected on localhost",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/jobs",
			headers:       map[string]string{"Origin": "http://evil.com"},
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "invalid origin",
		},
		{
			name:          "POST with Referer works when Origin absent",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/jobs",
			headers:       map[string]string{"Referer": "http://localhost:774/"},
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "POST without Origin on LAN rejected (no loopback bypass)",
			networkAccess: "lan",
			method:        http.MethodPost,
			path:          "/api/jobs",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "POST without Origin on external rejected",
			networkAccess: "external",
			method:        http.MethodPost,
			path:          "/api/jobs",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "POST /api/restart without Origin rejected (closes audit C-1)",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/restart",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "POST /api/auth/set-password without Origin rejected (closes audit C-5)",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/auth/set-password",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
		{
			name:          "POST /api/jobs/abc/open-folder without Origin rejected (closes audit C-8)",
			networkAccess: "localhost",
			method:        http.MethodPost,
			path:          "/api/jobs/abc/open-folder",
			wantStatus:    http.StatusForbidden,
			wantReasonSub: "missing origin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := makeStore(tt.networkAccess)
			mw := CSRFMiddleware(store, internalToken, &recordingLogger{})
			handler := mw(passHandler)

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, makeRequest(tt.method, tt.path, tt.headers))

			if rr.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if tt.wantReasonSub != "" && !strings.Contains(rr.Body.String(), tt.wantReasonSub) {
				t.Errorf("body %q does not contain %q", rr.Body.String(), tt.wantReasonSub)
			}
		})
	}
}

// TestCSRFOriginPolicyInExternalMode drives the whole CSRFMiddleware on the two
// policies that used to accept every origin. The table in TestCSRFMiddleware
// cannot host these: they need a Host header and a trusted-proxy config, which
// no other row varies.
//
// THE MUTANT: restore `case "external", "public": return true` in
// isAllowedOrigin — the first subtest goes 403 → 204 and a cross-site form post
// reaches /api/restart on every external install.
func TestCSRFOriginPolicyInExternalMode(t *testing.T) {
	newStore := func(networkAccess string, proxies ...string) *config.Store {
		cfg := &config.MoomboxConfig{
			Network: config.NetworkConfig{
				NetworkAccess:  networkAccess,
				TrustedProxies: proxies,
			},
		}
		return config.NewStore(cfg, "")
	}
	pass := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	t.Run("a foreign origin is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/restart", strings.NewReader(""))
		req.Host = "dash.example"
		req.Header.Set("Origin", "https://evil.example")
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore("external"), "tok", &recordingLogger{})(pass).ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — a page on evil.example reached a mutating handler "+
				"of an external install (T1-6): %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "invalid origin") {
			t.Errorf("body %q does not carry CSRFMiddleware's invalid-origin answer", rr.Body.String())
		}
	})

	t.Run("the deployment's own origin is accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/restart", strings.NewReader(""))
		req.Host = "dash.example"
		req.Header.Set("Origin", "http://dash.example")
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore("external"), "tok", &recordingLogger{})(pass).ServeHTTP(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 — the refusals here prove nothing if the dashboard "+
				"cannot drive its own server: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("a trusted proxy's X-Forwarded-Host is honoured", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/restart", strings.NewReader(""))
		req.RemoteAddr = "10.4.0.9:5555"
		req.Host = "10.4.0.9:774" // what nginx's default proxy_set_header sends
		req.Header.Set("X-Forwarded-Host", "dash.example")
		req.Header.Set("Origin", "https://dash.example")
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore("external", "10.0.0.0/8"), "tok", &recordingLogger{})(pass).ServeHTTP(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 — a reverse-proxy deployment that declares its proxy in "+
				"trusted_proxies must still be able to post to itself: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("X-Forwarded-Host from an UNTRUSTED peer is ignored", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/restart", strings.NewReader(""))
		req.RemoteAddr = "10.4.0.9:5555"
		req.Host = "dash.example"
		req.Header.Set("X-Forwarded-Host", "evil.example")
		req.Header.Set("Origin", "https://evil.example")
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore("external"), "tok", &recordingLogger{})(pass).ServeHTTP(rr, req) // no trusted_proxies

		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — reading X-Forwarded-Host without the trusted-proxy "+
				"check lets the attacker choose the host the origin is compared against, which is "+
				"no check at all", rr.Code)
		}
	})
}

// TestCORSReflectionFollowsTheOriginPolicy pins the other half of T1-6: a
// browser only honours a cross-origin READ when the server echoes the origin
// back, so CORS must refuse exactly what CSRF refuses.
//
// THE MUTANT: leave CORSMiddleware calling the old two-argument
// isAllowedOrigin — the first assertion sees
// `Access-Control-Allow-Origin: https://evil.example` with
// `Allow-Credentials: true`, and any page on the internet can read /api/jobs
// out of a LAN browser.
func TestCORSReflectionFollowsTheOriginPolicy(t *testing.T) {
	store := config.NewStore(&config.MoomboxConfig{
		Network: config.NetworkConfig{NetworkAccess: "external"},
	}, "")
	pass := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	get := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
		req.Host = "dash.example"
		req.Header.Set("Origin", origin)
		rr := httptest.NewRecorder()
		CORSMiddleware(store)(pass).ServeHTTP(rr, req)
		return rr
	}

	if got := get("https://evil.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q on a foreign origin, want none", got)
	}
	if got := get("http://dash.example").Header().Get("Access-Control-Allow-Origin"); got != "http://dash.example" {
		t.Errorf("Access-Control-Allow-Origin = %q on the deployment's own origin, want it echoed", got)
	}

	// The preflight branch must agree with the simple-request branch — they
	// were two independent isAllowedOrigin calls before this task.
	req := httptest.NewRequest(http.MethodOptions, "/api/jobs", nil)
	req.Host = "dash.example"
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	CORSMiddleware(store)(pass).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("preflight status = %d on a foreign origin, want 403", rr.Code)
	}
}

func storeWithProxies(proxies ...string) *config.Store {
	cfg := config.Defaults()
	cfg.Network.TrustedProxies = proxies
	return config.NewStore(cfg, "")
}

func TestEffectiveClientIP(t *testing.T) {
	tests := []struct {
		name       string
		proxies    []string
		remoteAddr string
		xff        string
		expected   string
		// untrusted additionally asserts the result is classified as neither
		// loopback nor private — the fail-CLOSED property. It is the
		// load-bearing assertion for the neutralized cases below; their
		// `expected` string only documents the current diagnostic form.
		untrusted bool
		// xffLines sets X-Forwarded-For as SEPARATE header field lines (one
		// Header.Add per entry) instead of the single line `xff` produces.
		// Mutually exclusive with xff.
		xffLines []string
	}{
		// No proxies configured: identical to ExtractIP, XFF ignored.
		{"no proxies, forged xff ignored", nil, "203.0.113.9:5000", "127.0.0.1", "203.0.113.9", false, nil},
		// Direct peer is NOT a trusted proxy: XFF ignored even when configured.
		{"untrusted peer, xff ignored", []string{"172.18.0.2"}, "203.0.113.9:5000", "10.0.0.1", "203.0.113.9", false, nil},
		// Trusted proxy, single-hop XFF: real client returned.
		{"proxy forwards wan client", []string{"172.18.0.2"}, "172.18.0.2:41000", "203.0.113.9", "203.0.113.9", false, nil},
		{"proxy forwards lan client", []string{"172.18.0.2"}, "172.18.0.2:41000", "192.168.1.50", "192.168.1.50", false, nil},
		// Client-forged private prefix through the proxy: proxy appends the
		// real address to the RIGHT, and the rightmost-untrusted walk finds it.
		{"forged private prefix defeated", []string{"172.18.0.2"}, "172.18.0.2:41000", "10.0.0.1, 203.0.113.9", "203.0.113.9", false, nil},
		// Same forgery, but the proxy appends by adding a SECOND header field
		// line instead of extending the first (HAProxy's `option forwardfor`
		// does exactly this). Header.Get would return only the client's line
		// and the walk would hand back the forged private address, failing
		// OPEN through the lan gate and the auth skip. Every field line must
		// be concatenated in wire order before the walk.
		{"multi-line xff: rightmost line wins", []string{"172.18.0.2"}, "172.18.0.2:41000", "", "203.0.113.9", true,
			[]string{"192.168.1.50", "203.0.113.9"}},
		// Two chained trusted proxies (CIDR), then the client.
		{"cidr proxy chain", []string{"172.18.0.0/16"}, "172.18.0.2:41000", "203.0.113.9, 172.18.0.3", "203.0.113.9", false, nil},
		// Trusted proxy but no XFF at all: fall back to the proxy address.
		{"proxy without xff", []string{"172.18.0.2"}, "172.18.0.2:41000", "", "172.18.0.2", false, nil},
		// Every hop trusted (proxy self-call / health check): direct peer.
		{"all hops trusted", []string{"172.18.0.0/16"}, "172.18.0.2:41000", "172.18.0.3", "172.18.0.2", false, nil},
		// Malformed rightmost entry fails CLOSED (returned verbatim → treated
		// as neither loopback nor private downstream), not open.
		{"malformed entry fails closed", []string{"172.18.0.2"}, "172.18.0.2:41000", "garbage-value", "garbage-value", true, nil},
		// A forged entry that SPELLS a trusted class must not inherit it.
		// isLoopback resolves the bare hostname "localhost" to loopback, so an
		// un-neutralized verbatim return here would fail OPEN and, once this
		// feeds the auth predicate, skip authentication outright. Reachable in
		// the headline Docker setup: a sender on the bridge emits
		// "X-Forwarded-For: localhost" and the proxy appends its real bridge
		// address to the right, so the walk skips the trusted hop and lands on
		// the forgery.
		{"forged localhost fails closed", []string{"172.18.0.0/16"}, "172.18.0.2:41000", "localhost, 172.18.0.99", "invalid-localhost", true, nil},
		{"bare forged localhost fails closed", []string{"172.18.0.0/16"}, "172.18.0.2:41000", "localhost", "invalid-localhost", true, nil},
		// IPv6: bracketed with port, plus zone stripping.
		{"ipv6 bracketed with port", []string{"172.18.0.2"}, "172.18.0.2:41000", "[2001:db8::1]:443", "2001:db8::1", false, nil},
		{"ipv6 zone stripped", []string{"172.18.0.2"}, "172.18.0.2:41000", "fe80::1%eth0", "fe80::1", false, nil},
		// IPv6 trusted proxy.
		{"ipv6 proxy", []string{"fd77:4d42::/64"}, "[fd77:4d42::2]:41000", "203.0.113.9", "203.0.113.9", false, nil},
		// Bare (non-CIDR) IPv6 proxy entry: exercises the /128 widening in
		// loadTrustedProxies that every CIDR-shaped case above skips.
		{"bare ipv6 proxy widened to /128", []string{"fd77:4d42::2"}, "[fd77:4d42::2]:41000", "203.0.113.9", "203.0.113.9", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			for _, line := range tt.xffLines {
				r.Header.Add("X-Forwarded-For", line)
			}
			got := EffectiveClientIP(storeWithProxies(tt.proxies...), r)
			if got != tt.expected {
				t.Errorf("EffectiveClientIP() = %q, want %q", got, tt.expected)
			}
			if tt.untrusted {
				if isLoopback(got) {
					t.Errorf("EffectiveClientIP() = %q classifies as LOOPBACK; must fail closed", got)
				}
				if isPrivateIP(got) {
					t.Errorf("EffectiveClientIP() = %q classifies as PRIVATE; must fail closed", got)
				}
			}
		})
	}
}

// TestIPGateHonorsTrustedProxy: with a trusted proxy declared, the lan gate
// judges the FORWARDED client address, not the proxy's private address.
func TestIPGateHonorsTrustedProxy(t *testing.T) {
	cfg := config.Defaults()
	cfg.Network.NetworkAccess = "lan"
	cfg.Network.TrustedProxies = []string{"172.18.0.2"}
	store := config.NewStore(cfg, "")

	handler := IPGateMiddleware(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		wantStatus int
		// xffLines sets X-Forwarded-For as separate header field lines
		// (Header.Add per entry) rather than one comma-joined line.
		xffLines []string
	}{
		{"wan client via proxy blocked", "172.18.0.2:41000", "203.0.113.9", http.StatusForbidden, nil},
		{"lan client via proxy allowed", "172.18.0.2:41000", "192.168.1.50", http.StatusOK, nil},
		{"proxy itself (no xff) allowed", "172.18.0.2:41000", "", http.StatusOK, nil},
		{"direct wan client still blocked", "203.0.113.9:5000", "", http.StatusForbidden, nil},
		{"direct wan client, forged xff, still blocked", "203.0.113.9:5000", "192.168.1.50", http.StatusForbidden, nil},
		{"trusted proxy, forged private prefix, still blocked", "172.18.0.2:41000", "192.168.1.50, 203.0.113.9", http.StatusForbidden, nil},
		// The forgery arrives on its own header field line and the proxy adds
		// a second one (HAProxy-style append). Reading only the first line
		// would let the forged 192.168.1.50 pass the lan gate.
		{"trusted proxy, forged private line, still blocked", "172.18.0.2:41000", "", http.StatusForbidden,
			[]string{"192.168.1.50", "203.0.113.9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/jobs", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			for _, line := range tt.xffLines {
				r.Header.Add("X-Forwarded-For", line)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

// TestCompressionReusesGzipWriters: gzip.NewWriter allocates its deflate
// window and hash tables on every call — measured at 1,080,105 B/op across 15
// allocations, against 4,099 B/op across 1 for a pooled writer. On a dashboard
// polling /api/jobs that is ~1 MB of garbage per response (sweep T4-35).
//
// THE MUTANT: replace the pool Get/Reset in startGzip with gzip.NewWriter —
// AllocedBytesPerOp jumps past 200 KB and this fails.
func TestCompressionReusesGzipWriters(t *testing.T) {
	body := strings.Repeat("compressible-json-byte-", 200) // ~4.6 KB, over gzipMinSize
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	})
	wrapped := CompressionMiddleware(handler)

	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			wrapped.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
	// The race detector charges its shadow memory to the allocating goroutine,
	// which lifts the SAME pooled workload from ~4 KB/op to ~291 KB/op. Raise
	// the ceiling rather than skip the test: the unpooled baseline is ~1.09 MB,
	// so the mutant below still fails under both builds (sweep R5/T3).
	ceiling := 200 * 1024
	if raceEnabled {
		ceiling = 500 * 1024
	}
	if got := res.AllocedBytesPerOp(); got > int64(ceiling) {
		t.Errorf("a gzipped response allocates %d B/op against a %d ceiling; a fresh gzip.Writer alone "+
			"is ~1.08 MB and a pooled one ~4 KB, so anything over that means the writer is not being "+
			"reused", got, ceiling)
	}

	// Correctness, not just cost: a reused writer that is not Reset onto the
	// new ResponseWriter writes into the previous response.
	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)

		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		got, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if string(got) != body {
			t.Fatalf("decoded body = %d bytes, want the original %d — the pooled writer was not "+
				"Reset onto this response", len(got), len(body))
		}
	}
}

// recordingLogger captures Warn lines so a test can assert the refusal line
// exists and names the pair that was compared. Debug/Info/Error are no-ops so
// the same fixture also satisfies WebSocketHub's four-method logger
// (websocket_origin_test.go's refusal-log test).
type recordingLogger struct{ warns []string }

func (l *recordingLogger) Debug(msg string, args ...any) {}
func (l *recordingLogger) Info(msg string, args ...any)  {}

func (l *recordingLogger) Warn(msg string, args ...any) {
	line := msg
	for i := 0; i+1 < len(args); i += 2 {
		line += " " + fmt.Sprint(args[i]) + "=" + fmt.Sprint(args[i+1])
	}
	l.warns = append(l.warns, line)
}

func (l *recordingLogger) Error(msg string, args ...any) {}

// TestCSRFOriginComparison pins WHICH authority the Origin is compared
// against, through the real middleware.
//
// THE MUTANTS: making originAllowed read r.Host instead of
// effectiveRequestHost fails the trusted-proxy row (403 instead of 200);
// trusting X-Forwarded-Host without the trusted_proxies test fails the
// untrusted row (200 instead of 403); deleting the Warn call fails the log
// assertion.
func TestCSRFOriginComparison(t *testing.T) {
	newStore := func(trusted []string) *config.Store {
		return config.NewStore(&config.MoomboxConfig{
			Network: config.NetworkConfig{
				NetworkAccess:  "public",
				TrustedProxies: trusted,
			},
		}, "")
	}
	pass := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	newRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(""))
		r.RemoteAddr = "10.1.2.3:44444"
		r.Host = "internal:774"
		r.Header.Set("X-Forwarded-Host", "dash.example")
		r.Header.Set("Origin", "http://dash.example")
		return r
	}

	t.Run("trusted proxy: the forwarded host is the one compared", func(t *testing.T) {
		log := &recordingLogger{}
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore([]string{"10.1.2.3"}), "tok", log)(pass).ServeHTTP(rr, newRequest())
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 — the Origin names the forwarded host", rr.Code)
		}
		if len(log.warns) != 0 {
			t.Fatalf("logged %v on an accepted request, want nothing", log.warns)
		}
	})

	t.Run("untrusted peer: the forwarded host is ignored and the refusal names the pair", func(t *testing.T) {
		log := &recordingLogger{}
		rr := httptest.NewRecorder()
		CSRFMiddleware(newStore(nil), "tok", log)(pass).ServeHTTP(rr, newRequest())
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — X-Forwarded-Host from an untrusted peer must not count", rr.Code)
		}
		if len(log.warns) != 1 {
			t.Fatalf("logged %v, want exactly one refusal line", log.warns)
		}
		line := log.warns[0]
		for _, want := range []string{"CSRF: origin refused", "http://dash.example", "internal:774"} {
			if !strings.Contains(line, want) {
				t.Fatalf("refusal line %q does not name %q", line, want)
			}
		}
	})
}

// TestHostGateRefusesARebindingHost: on localhost/lan a GET carries no Origin
// check, so a page on attacker.example rebound to 127.0.0.1 read the whole
// API same-origin — the peer is loopback, so no auth applied. The Host it was
// addressed by is held to the Origin rule instead. external/public are meant
// to be reached by DNS names and stay open here.
//
// Mutant: drop HostGateMiddleware from the chain (or let a DNS name through on
// lan) — the attacker.example rows pass.
func TestHostGateRefusesARebindingHost(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, tc := range []struct {
		access, host string
		want         int
	}{
		{"localhost", "localhost:774", http.StatusOK},
		{"localhost", "127.0.0.1:774", http.StatusOK},
		{"localhost", "[::1]:774", http.StatusOK},
		{"localhost", "attacker.example:774", http.StatusForbidden},
		{"localhost", "192.168.1.10:774", http.StatusForbidden},
		{"", "attacker.example", http.StatusForbidden},
		{"lan", "192.168.1.10:774", http.StatusOK},
		{"lan", "localhost:774", http.StatusOK},
		{"lan", "attacker.example:774", http.StatusForbidden},
		{"external", "attacker.example:774", http.StatusOK},
		{"public", "moombox.example.com", http.StatusOK},
	} {
		cfg := config.Defaults()
		cfg.Network.NetworkAccess = tc.access
		h := HostGateMiddleware(config.NewStore(cfg, ""))(ok)
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		req.RemoteAddr = "127.0.0.1:50000"
		req.Host = tc.host
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("access=%q Host=%q: status %d, want %d", tc.access, tc.host, rr.Code, tc.want)
		}
	}
}
