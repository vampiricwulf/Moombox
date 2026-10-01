package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// The release job signs with whatever the SIGNING_KEY secret holds. If that is
// not the private half of the public key the updater embeds, the signatures
// are well-formed and worthless: the release goes green and every installed
// Moombox refuses the update. Signing has to refuse instead, and leave no
// .sig behind for the publish step to upload.
func TestSignFileRefusesAKeyTheUpdaterDoesNotTrust(t *testing.T) {
	_, strangerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "Moombox.exe")
	if err := os.WriteFile(binary, []byte("a release binary"), 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	sigPath, err := signFile(hex.EncodeToString(strangerKey), binary)
	if err == nil {
		t.Errorf("signFile accepted a key the updater's public key does not verify; wrote %s", sigPath)
	}
	if _, statErr := os.Stat(binary + ".sig"); !os.IsNotExist(statErr) {
		t.Errorf("a .sig was left beside the binary (stat err = %v)", statErr)
	}
}
