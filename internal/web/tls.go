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
	"time"
)

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

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
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
		for _, ip := range getLANIPs() {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
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
	sans := "localhost, 127.0.0.1, ::1"
	for _, ip := range tmpl.IPAddresses {
		if !ip.IsLoopback() {
			sans += ", " + ip.String()
		}
	}
	logger.Info("[TLS] Certificate SANs: " + sans)

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
