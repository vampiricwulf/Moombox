// Command sign is a CI tool for Ed25519 binary signing.
//
// Usage:
//
//	go run ./cmd/sign -genkey         Generate a new Ed25519 key pair
//	go run ./cmd/sign <file>          Sign <file>, writes <file>.sig
//
// Signing reads the private key from the SIGNING_KEY environment variable
// (hex-encoded Ed25519 private key, 128 hex chars / 64 bytes).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/vampiricwulf/Moombox/internal/updater"
)

func main() {
	genkey := flag.Bool("genkey", false, "generate a new Ed25519 key pair")
	flag.Parse()

	if *genkey {
		generateKeyPair()
		return
	}

	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: sign [-genkey] <file>")
		os.Exit(1)
	}

	signFile(args[0])
}

func generateKeyPair() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating key pair: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Ed25519 key pair generated.")
	fmt.Println()
	fmt.Printf("Public key (embed in signing.go):  %s\n", hex.EncodeToString(pub))
	fmt.Printf("Private key (GitHub Actions secret): %s\n", hex.EncodeToString(priv))
	fmt.Println()
	fmt.Println("IMPORTANT: Store the private key as a GitHub Actions secret named SIGNING_KEY.")
	fmt.Println("           The public key goes into internal/updater/signing.go as updatePublicKeyHex.")
}

func signFile(path string) {
	keyHex := os.Getenv("SIGNING_KEY")
	if keyHex == "" {
		fmt.Fprintln(os.Stderr, "error: SIGNING_KEY environment variable not set")
		os.Exit(1)
	}

	sig, err := updater.SignBinary(keyHex, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error signing %s: %v\n", path, err)
		os.Exit(1)
	}

	sigPath := path + ".sig"
	if err := os.WriteFile(sigPath, sig, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", sigPath, err)
		os.Exit(1)
	}

	fmt.Printf("Signed %s → %s (%d bytes)\n", path, sigPath, len(sig))
}
