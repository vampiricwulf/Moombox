package updater

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
)

// Hex-encoded Ed25519 public key (32 bytes = 64 hex chars).
// The corresponding private key is stored as a GitHub Actions secret (SIGNING_KEY).
const updatePublicKeyHex = "71ce2f926296a552950faa1fd7d3e89574e14ec353aa253f2577f6883fdf51eb"

// VerifySignature verifies that sigPath contains a valid Ed25519 signature
// for the file at binaryPath, using the embedded public key.
func VerifySignature(binaryPath, sigPath string) error {
	pubKey, err := hex.DecodeString(updatePublicKeyHex)
	if err != nil {
		return fmt.Errorf("invalid embedded public key: %w", err)
	}
	if len(pubKey) != ed25519.PublicKeySize {
		return fmt.Errorf("embedded public key is %d bytes, expected %d", len(pubKey), ed25519.PublicKeySize)
	}
	return verifySignatureWithKey(pubKey, binaryPath, sigPath)
}

// verifySignatureWithKey verifies a signature using an explicit public key.
// Used by tests to verify with generated keys rather than the embedded key.
func verifySignatureWithKey(pubKey ed25519.PublicKey, binaryPath, sigPath string) error {
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("reading binary: %w", err)
	}

	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("reading signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, expected %d", len(sig), ed25519.SignatureSize)
	}

	if !ed25519.Verify(pubKey, binary, sig) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

// SignBinary signs the file at binaryPath with the given hex-encoded private key.
// Returns the 64-byte Ed25519 signature.
func SignBinary(privateKeyHex string, binaryPath string) ([]byte, error) {
	privKey, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid private key hex: %w", err)
	}
	if len(privKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, expected %d", len(privKey), ed25519.PrivateKeySize)
	}

	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("reading binary: %w", err)
	}

	sig := ed25519.Sign(privKey, binary)
	return sig, nil
}
