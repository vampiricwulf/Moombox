package web

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// fakeRestrictFileToOwner swaps the restrictFileToOwner seam for one test.
func fakeRestrictFileToOwner(t *testing.T, fn func(path string) error) {
	t.Helper()
	real := restrictFileToOwner
	t.Cleanup(func() { restrictFileToOwner = real })
	restrictFileToOwner = fn
}

// TestGeneratedKeyIsRestrictedWhileStillEmpty pins which file the generator
// restricts and when: the private key, before the key is written into it. On
// Windows the 0600 the file is created with means nothing for access, so a key
// written first sits under its folder's inherited permissions.
//
// Mutants: drop the call -> nothing is seen; move it after pem.Encode -> the
// file already holds the key; pass certPath -> the wrong file is seen.
func TestGeneratedKeyIsRestrictedWhileStillEmpty(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "moombox.crt"), filepath.Join(dir, "moombox.key")
	var seen []string
	atCall, readErr := []byte("never read"), error(nil)
	fakeRestrictFileToOwner(t, func(path string) error {
		seen = append(seen, path)
		atCall, readErr = os.ReadFile(path)
		return nil
	})

	if err := generateSelfSignedCert(certPath, keyPath, "localhost", &countingWSLogger{}); err != nil {
		t.Fatalf("generateSelfSignedCert: %v", err)
	}

	if len(seen) != 1 || seen[0] != keyPath {
		t.Fatalf("the file restriction ran on %v, want only the key %q", seen, keyPath)
	}
	if readErr != nil || len(atCall) != 0 {
		t.Errorf("the key file held %d bytes (read error %v) when the restriction ran, want it empty", len(atCall), readErr)
	}
}

// TestGeneratedKeySurvivesAFailedRestriction: the restriction is hardening. A
// volume that refuses the DACL (FAT, some network shares) still gets a working
// certificate pair, and the operator is told once.
//
// Mutants: return the restriction's error -> generation fails; drop the Warn
// -> nothing is logged.
func TestGeneratedKeySurvivesAFailedRestriction(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "moombox.crt"), filepath.Join(dir, "moombox.key")
	fakeRestrictFileToOwner(t, func(string) error { return errors.New("this volume has no ACLs") })
	log := &countingWSLogger{}

	if err := generateSelfSignedCert(certPath, keyPath, "localhost", log); err != nil {
		t.Fatalf("generation failed because the file restriction did: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("the generated pair does not load: %v", err)
	}
	if _, warns := log.counts(); warns != 1 {
		t.Errorf("%d warnings, want 1 naming the key that could not be restricted", warns)
	}
}

// generatedPair runs the real generator into a temp folder and loads what it
// wrote, with the restriction seam silenced.
func generatedPair(t *testing.T) (keyPath string, cert *tls.Certificate) {
	t.Helper()
	fakeRestrictFileToOwner(t, func(string) error { return nil })
	dir := t.TempDir()
	certPath := filepath.Join(dir, "moombox.crt")
	keyPath = filepath.Join(dir, "moombox.key")
	if err := generateSelfSignedCert(certPath, keyPath, "localhost", &countingWSLogger{}); err != nil {
		t.Fatalf("generateSelfSignedCert: %v", err)
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load generated pair: %v", err)
	}
	return keyPath, &c
}

// TestExistingKeyIsRestrictedOnlyAtTheDefaultPaths: at startup Moombox
// restricts an existing key only when the pair is its own generated one at the
// default paths. A path the operator configured is the operator's file, even
// when the certificate there is one Moombox generated.
//
// Mutants: drop either path check -> the configured-path rows restrict the
// key; drop the isOwnGeneratedCert check -> the operator's certificate row
// does; drop the call -> the first row restricts nothing.
func TestExistingKeyIsRestrictedOnlyAtTheDefaultPaths(t *testing.T) {
	keyPath, own := generatedPair(t)
	operators := certWatcherFor(t, "dashboard.example.org", []string{"dashboard.example.org"}, nil).cert.Load()

	cases := []struct {
		name          string
		certCfg, key  string
		cert          *tls.Certificate
		wantRestricts bool
	}{
		{"own certificate, both paths left empty", "", "", own, true},
		{"own certificate, key path configured", "", "./moombox.key", own, false},
		{"own certificate, certificate path configured", "./moombox.crt", "", own, false},
		{"an operator's certificate at the default paths", "", "", operators, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			fakeRestrictFileToOwner(t, func(path string) error {
				seen = append(seen, path)
				return nil
			})
			cfg := config.Defaults()
			cfg.Network.TLSCertPath, cfg.Network.TLSKeyPath = tc.certCfg, tc.key

			restrictOwnGeneratedKey(cfg, keyPath, tc.cert, &countingWSLogger{})

			if tc.wantRestricts && (len(seen) != 1 || seen[0] != keyPath) {
				t.Errorf("the restriction ran on %v, want only %q", seen, keyPath)
			}
			if !tc.wantRestricts && len(seen) != 0 {
				t.Errorf("the restriction ran on %v, want the file left alone", seen)
			}
		})
	}
}

// lookalikeCert builds a certificate from the generator's own template, lets
// mutate change one thing about it, and signs it with signer (its own key when
// nil).
func lookalikeCert(t *testing.T, key crypto.Signer, signer crypto.Signer, mutate func(*x509.Certificate)) *tls.Certificate {
	t.Helper()
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: placeholderCertCN},
		NotBefore:    now,
		NotAfter:     now.Add(generatedCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	if mutate != nil {
		mutate(tmpl)
	}
	if signer == nil {
		signer = key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), signer)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestIsOwnGeneratedCertRequiresEveryMarkOfTheTemplate: the answer lets
// Moombox change a file's permissions, so a certificate that merely carries
// the Common Name "Moombox" must not pass. Each row differs from the
// generator's template in one mark; the first row is the generator's own
// output, which ties the predicate to the generator.
//
// Mutants: drop any one check in isOwnGeneratedCert -> its row answers true;
// change the generator's template without the predicate -> the first row
// answers false.
func TestIsOwnGeneratedCertRequiresEveryMarkOfTheTemplate(t *testing.T) {
	_, own := generatedPair(t)
	ec := func() crypto.Signer {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	withChain := lookalikeCert(t, ec(), nil, nil)
	withChain.Certificate = append(withChain.Certificate, withChain.Certificate[0])

	cases := []struct {
		name string
		cert *tls.Certificate
		want bool
	}{
		{"the generator's own output", own, true},
		{"the template rebuilt by hand", lookalikeCert(t, ec(), nil, nil), true},
		{"no certificate", nil, false},
		{"a chain of two", withChain, false},
		{"another Common Name", lookalikeCert(t, ec(), nil, func(c *x509.Certificate) { c.Subject.CommonName = "moombox.example.org" }), false},
		{"an organisation beside the Common Name", lookalikeCert(t, ec(), nil, func(c *x509.Certificate) { c.Subject.Organization = []string{"Example"} }), false},
		{"signed by another key under the same name", lookalikeCert(t, ec(), ec(), nil), false},
		{"an RSA key", lookalikeCert(t, rsaKey, nil, nil), false},
		{"a P-384 key", lookalikeCert(t, p384, nil, nil), false},
		{"a CA certificate", lookalikeCert(t, ec(), nil, func(c *x509.Certificate) { c.IsCA = true; c.BasicConstraintsValid = true }), false},
		{"another key usage", lookalikeCert(t, ec(), nil, func(c *x509.Certificate) { c.KeyUsage |= x509.KeyUsageKeyEncipherment }), false},
		{"client auth as well", lookalikeCert(t, ec(), nil, func(c *x509.Certificate) {
			c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		}), false},
		{"another DNS name", lookalikeCert(t, ec(), nil, func(c *x509.Certificate) { c.DNSNames = []string{"localhost", "dashboard.example.org"} }), false},
		{"another lifetime", lookalikeCert(t, ec(), nil, func(c *x509.Certificate) { c.NotAfter = c.NotBefore.Add(365 * 24 * time.Hour) }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOwnGeneratedCert(tc.cert); got != tc.want {
				t.Errorf("isOwnGeneratedCert = %v, want %v", got, tc.want)
			}
		})
	}
}
