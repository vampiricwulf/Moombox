package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"slices"
	"testing"
	"time"
)

// certWatcherFor builds a self-issued certificate with the given Common Name
// and SANs and returns a watcher holding it — the same shape
// LoadOrGenerateTLSConfig publishes as CurrentCertSANs.
func certWatcherFor(t *testing.T, commonName string, dnsNames []string, ips []net.IP) *certWatcher {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	w := &certWatcher{}
	w.cert.Store(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	return w
}

// useIdentityCert installs w as the process-wide certificate for one test.
// internal/web's tests never call t.Parallel, so a plain swap with a restore
// is safe here and needs no lock on CurrentCertSANs.
func useIdentityCert(t *testing.T, w *certWatcher) {
	t.Helper()
	prev := CurrentCertSANs
	CurrentCertSANs = w
	t.Cleanup(func() { CurrentCertSANs = prev })
}

// THE MUTANT: drop the placeholderCertCN check from IdentitySANs. The
// placeholder's localhost/interface SANs become an allowlist, and every
// external install reached by a DNS name or a NATed public address is refused
// on its own dashboard.
func TestIdentitySANsIgnoresTheMoomboxPlaceholder(t *testing.T) {
	w := certWatcherFor(t, placeholderCertCN, []string{"localhost"},
		[]net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback})

	if got := w.IdentitySANs(); got != nil {
		t.Fatalf("IdentitySANs() = %v, want nil for Moombox's own certificate", got)
	}
	// SANs() keeps reporting everything — IdentitySANs is a second, narrower
	// reader, not a replacement.
	if got := w.SANs(); len(got) != 3 {
		t.Fatalf("SANs() = %v, want the certificate's three entries", got)
	}
}

// THE MUTANT: return nil unconditionally from IdentitySANs — the rebinding row
// in TestIsAllowedOrigin stops denying, which is the whole point of the change.
func TestIdentitySANsReportsAnOperatorCertificate(t *testing.T) {
	w := certWatcherFor(t, "dash.example", []string{"DASH.example"},
		[]net.IP{net.IPv4(203, 0, 113, 7)})

	got := w.IdentitySANs()
	want := []string{"dash.example", "203.0.113.7"}
	if !slices.Equal(got, want) {
		t.Fatalf("IdentitySANs() = %v, want %v (DNS names lowercased, IPs canonical)", got, want)
	}
}

// TestIdentitySANsRecomputesAfterAHotSwap pins fix-round-1 item 2 (review
// Minor #2 / probe P5b): IdentitySANs/SANs now cache their result per loaded
// *tls.Certificate rather than re-parsing the DER on every call, so a
// certificate rotation (reloadIfChanged's plain w.cert.Store — reproduced
// directly here) must still be visible on the very next read.
//
// THE MUTANT: cache the first computed result forever, ignoring whether
// w.cert has since been swapped (e.g. a sync.Once, or comparing against
// nothing) — this test would keep seeing "dash.example" after the swap to
// "rotated.example".
func TestIdentitySANsRecomputesAfterAHotSwap(t *testing.T) {
	w := certWatcherFor(t, "dash.example", []string{"dash.example"}, nil)
	if got := w.IdentitySANs(); !slices.Equal(got, []string{"dash.example"}) {
		t.Fatalf("IdentitySANs() before swap = %v, want [dash.example]", got)
	}

	// Reproduce reloadIfChanged's swap directly: a plain cert.Store with no
	// separate "invalidate the cache" call — that absence is exactly what a
	// pointer-keyed cache must not need.
	rotated := certWatcherFor(t, "rotated.example", []string{"rotated.example"}, nil)
	w.cert.Store(rotated.cert.Load())

	if got := w.IdentitySANs(); !slices.Equal(got, []string{"rotated.example"}) {
		t.Fatalf("IdentitySANs() after swap = %v, want [rotated.example] — stale cache after a reload", got)
	}
}

// THE MUTANT: have identityHosts call CurrentCertSANs.SANs() instead of
// IdentitySANs() — the placeholder narrows the origin check again.
func TestIdentityHostsReadsTheNarrowReader(t *testing.T) {
	useIdentityCert(t, nil)
	if got := identityHosts(); got != nil {
		t.Fatalf("identityHosts() with no certificate = %v, want nil", got)
	}

	useIdentityCert(t, certWatcherFor(t, placeholderCertCN, []string{"localhost"}, nil))
	if got := identityHosts(); got != nil {
		t.Fatalf("identityHosts() with the placeholder = %v, want nil", got)
	}

	useIdentityCert(t, certWatcherFor(t, "dash.example", []string{"dash.example"}, nil))
	if got := identityHosts(); !slices.Equal(got, []string{"dash.example"}) {
		t.Fatalf("identityHosts() with an operator certificate = %v, want [dash.example]", got)
	}
}
