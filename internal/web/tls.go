package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// certWatcher holds an atomically-swappable *tls.Certificate so the TLS
// stack can pick up a freshly written cert/key pair without restarting the
// process. Audit reports/web.md S-20 — "user manually replacing the
// self-signed with a real LE cert had to restart Moombox to get the
// rotation".
type certWatcher struct {
	certPath, keyPath string
	cert              atomic.Pointer[tls.Certificate]
	mu                sync.Mutex
	lastModTime       time.Time
	logger            interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// reloadIfChanged stat's the cert file; on a newer mtime it parses the new
// pair and atomically swaps it into the watcher. Falls back to the old
// cert on parse failure (warning logged) so a partially-written replacement
// can't bring down TLS.
func (w *certWatcher) reloadIfChanged() {
	w.mu.Lock()
	defer w.mu.Unlock()

	info, err := os.Stat(w.certPath)
	if err != nil {
		return
	}
	if !info.ModTime().After(w.lastModTime) {
		return
	}
	cert, err := tls.LoadX509KeyPair(w.certPath, w.keyPath)
	if err != nil {
		w.logger.Warn("[TLS] reload skipped — cert/key pair invalid", "err", err)
		return
	}
	w.lastModTime = info.ModTime()
	w.cert.Store(&cert)
	w.logger.Info("[TLS] reloaded certificate after on-disk change", "cert", w.certPath)
}

// getCertificate is the tls.Config.GetCertificate hook. Called once per
// handshake; the reload check itself is gated by mtime so the cost is a
// stat + atomic load when nothing has changed.
func (w *certWatcher) getCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	w.reloadIfChanged()
	c := w.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("no certificate loaded")
	}
	return c, nil
}

// SANs returns the DNS names + IP addresses that appear in the loaded
// certificate, normalised to lowercase strings (IPs as their canonical
// form). Used by the WebSocket origin allowlist (audit reports/web.md
// S-17) to replace the r.Host-derived host check that was vulnerable to
// Host-header spoofing.
func (w *certWatcher) SANs() []string {
	c := w.cert.Load()
	if c == nil || len(c.Certificate) == 0 {
		return nil
	}
	parsed, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(parsed.DNSNames)+len(parsed.IPAddresses))
	for _, dns := range parsed.DNSNames {
		out = append(out, strings.ToLower(dns))
	}
	for _, ip := range parsed.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

// CurrentCertSANs is the package-level singleton populated by
// LoadOrGenerateTLSConfig. Nil before the TLS config is built; consumers
// (websocket.go) call .SANs() defensively.
var CurrentCertSANs *certWatcher

// LoadOrGenerateTLSConfig returns a TLS configuration using the given cert/key
// files. If the files don't exist, a self-signed certificate is generated and
// written to disk first.
func LoadOrGenerateTLSConfig(certPath, keyPath, networkAccess string, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) (*tls.Config, error) {
	// Generate if either file is missing
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if os.IsNotExist(certErr) || os.IsNotExist(keyErr) {
		if err := generateSelfSignedCert(certPath, keyPath, networkAccess, logger); err != nil {
			return nil, fmt.Errorf("generate self-signed cert: %w", err)
		}
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}

	logger.Info("[TLS] Loaded certificate", "cert", certPath, "key", keyPath)

	// Watcher allows hot-rotation: replacing cert.pem + key.pem on disk is
	// picked up on the next handshake without a process restart. Audit
	// reports/web.md S-20.
	watcher := &certWatcher{
		certPath: certPath,
		keyPath:  keyPath,
		logger:   logger,
	}
	if info, statErr := os.Stat(certPath); statErr == nil {
		watcher.lastModTime = info.ModTime()
	}
	watcher.cert.Store(&cert)
	CurrentCertSANs = watcher

	return &tls.Config{
		Certificates:   []tls.Certificate{cert},
		GetCertificate: watcher.getCertificate,
		// TLS 1.2 minimum — TLS 1.3 ciphers can't be pinned via
		// CipherSuites by design (the TLS 1.3 RFC restricts the suite
		// list to a fixed set), and TLS 1.2 keeps non-browser clients
		// (older curl / embedded scripts) working. The audit's S-19
		// alternative — bumping MinVersion to TLS 1.3 — would lock out
		// any client that hasn't been patched in the last few years.
		MinVersion: tls.VersionTLS12,
		// Curated cipher list for TLS 1.2 only (TLS 1.3 ignores this
		// field). All ciphers here are ECDHE-based for forward secrecy
		// and AEAD-mode (GCM / ChaCha20-Poly1305) so a long-lived
		// session can't be retroactively decrypted if the server key
		// leaks. Drops Go's default TLS_RSA_* suites (no forward
		// secrecy) and any 3DES / CBC suites that historically had
		// padding-oracle issues. Audit reports/web.md S-19.
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
		// Server picks the cipher to enforce the curated order rather
		// than letting the client downgrade us to a weaker entry.
		PreferServerCipherSuites: true,
	}, nil
}

// generateSelfSignedCert creates an ECDSA P-256 self-signed certificate and
// writes the PEM-encoded cert and key to disk.
func generateSelfSignedCert(certPath, keyPath, networkAccess string, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ECDSA key: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Moombox"},
		NotBefore:    now,
		NotAfter:     now.Add(10 * 365 * 24 * time.Hour), // ~10 years
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},

		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	// Add LAN IPs when listening beyond localhost
	if networkAccess == "lan" || networkAccess == "external" || networkAccess == "public" {
		tmpl.IPAddresses = append(tmpl.IPAddresses, getLANIPs()...)
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	// Write certificate (world-readable)
	certFile, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("write cert file: %w", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		certFile.Close()
		return fmt.Errorf("encode cert PEM: %w", err)
	}
	certFile.Close()

	// Write private key (owner-only)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal EC key: %w", err)
	}
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		keyFile.Close()
		return fmt.Errorf("encode key PEM: %w", err)
	}
	keyFile.Close()

	logger.Info("[TLS] Generated self-signed certificate", "cert", certPath, "key", keyPath)

	// Log SANs for debugging
	var sans strings.Builder
	sans.WriteString("localhost, 127.0.0.1, ::1")
	for _, ip := range tmpl.IPAddresses {
		if !ip.IsLoopback() {
			sans.WriteString(", " + ip.String())
		}
	}
	logger.Info("[TLS] Certificate SANs: " + sans.String())

	return nil
}

// getLANIPs returns all non-loopback unicast IPs from local network interfaces.
func getLANIPs() []net.IP {
	var ips []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}
