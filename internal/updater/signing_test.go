package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func generateTestKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
	return path
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	privHex := hex.EncodeToString(priv)

	dir := t.TempDir()
	binaryPath := writeTempFile(t, dir, "test.exe", []byte("hello moombox binary"))

	sig, err := SignBinary(privHex, binaryPath)
	if err != nil {
		t.Fatalf("SignBinary: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature length = %d, want %d", len(sig), ed25519.SignatureSize)
	}

	sigPath := writeTempFile(t, dir, "test.exe.sig", sig)

	if err := verifySignatureWithKey(pub, binaryPath, sigPath); err != nil {
		t.Fatalf("VerifySignature failed on valid signature: %v", err)
	}
}

func TestVerifyRejectsTamperedBinary(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	privHex := hex.EncodeToString(priv)

	dir := t.TempDir()
	binaryPath := writeTempFile(t, dir, "test.exe", []byte("original binary"))

	sig, err := SignBinary(privHex, binaryPath)
	if err != nil {
		t.Fatalf("SignBinary: %v", err)
	}

	// Tamper with the binary after signing
	tamperedPath := writeTempFile(t, dir, "tampered.exe", []byte("tampered binary"))
	sigPath := writeTempFile(t, dir, "tampered.exe.sig", sig)

	err = verifySignatureWithKey(pub, tamperedPath, sigPath)
	if err == nil {
		t.Fatal("expected verification to fail for tampered binary, but it succeeded")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv := generateTestKeyPair(t)
	wrongPub, _ := generateTestKeyPair(t)
	privHex := hex.EncodeToString(priv)

	dir := t.TempDir()
	binaryPath := writeTempFile(t, dir, "test.exe", []byte("some binary"))

	sig, err := SignBinary(privHex, binaryPath)
	if err != nil {
		t.Fatalf("SignBinary: %v", err)
	}

	sigPath := writeTempFile(t, dir, "test.exe.sig", sig)

	// Verify with a different public key
	err = verifySignatureWithKey(wrongPub, binaryPath, sigPath)
	if err == nil {
		t.Fatal("expected verification to fail with wrong key, but it succeeded")
	}
}

func TestVerifyRejectsWrongSignatureLength(t *testing.T) {
	pub, _ := generateTestKeyPair(t)

	dir := t.TempDir()
	binaryPath := writeTempFile(t, dir, "test.exe", []byte("binary"))
	sigPath := writeTempFile(t, dir, "test.exe.sig", []byte("too short"))

	err := verifySignatureWithKey(pub, binaryPath, sigPath)
	if err == nil {
		t.Fatal("expected error for wrong signature length, but got nil")
	}
}

func TestSignBinaryRejectsInvalidKey(t *testing.T) {
	dir := t.TempDir()
	binaryPath := writeTempFile(t, dir, "test.exe", []byte("binary"))

	// Not valid hex
	_, err := SignBinary("not-hex", binaryPath)
	if err == nil {
		t.Fatal("expected error for invalid hex key")
	}

	// Valid hex but wrong length
	_, err = SignBinary("abcd", binaryPath)
	if err == nil {
		t.Fatal("expected error for wrong key length")
	}
}
