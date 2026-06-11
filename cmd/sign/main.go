// Command sign is a CI tool for Ed25519 binary signing.
//
// Usage:
//
//	go run ./cmd/sign -genkey                  Generate a new Ed25519 key pair (prints to stdout)
//	go run ./cmd/sign -genkey -out keys.txt    Generate key pair and write to keys.txt (mode 0o600)
//	go run ./cmd/sign <file>                   Sign <file>, writes <file>.sig
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
	"strings"

	"github.com/vampiricwulf/Moombox/internal/updater"
)

func main() {
	genkey := flag.Bool("genkey", false, "generate a new Ed25519 key pair")
	outPath := flag.String("out", "", "with -genkey, write key pair to this file (mode 0o600) instead of stdout")
	flag.Parse()

	if *genkey {
		generateKeyPair(*outPath)
		return
	}

	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: sign [-genkey [-out <file>]] <file>")
		os.Exit(1)
	}

	signFile(args[0])
}

func generateKeyPair(outPath string) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating key pair: %v\n", err)
		os.Exit(1)
	}

	pubHex := hex.EncodeToString(pub)
	privHex := hex.EncodeToString(priv)

	// When running in CI or anywhere that records transcripts, printing the
	// private key to stdout risks leaking it into log artifacts. The -out
	// flag writes the pair to a 0o600 file instead; stdout then only
	// reports the public key and the output path.
	if outPath != "" {
		contents := fmt.Sprintf("public_key=%s\nprivate_key=%s\n", pubHex, privHex)
		if err := os.WriteFile(outPath, []byte(contents), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "error writing key pair to %s: %v\n", outPath, err)
			os.Exit(1)
		}
		fmt.Println("Ed25519 key pair generated.")
		fmt.Println()
		fmt.Printf("Public key (embed in signing.go): %s\n", pubHex)
		fmt.Printf("Key pair written to: %s (mode 0o600)\n", outPath)
		fmt.Println()
		fmt.Println("IMPORTANT: Store the private_key line as a GitHub Actions secret named SIGNING_KEY.")
		fmt.Println("           Delete the output file after copying the secret.")
		return
	}

	fmt.Println("Ed25519 key pair generated.")
	fmt.Println()
	fmt.Printf("Public key (embed in signing.go):  %s\n", pubHex)
	fmt.Printf("Private key (GitHub Actions secret): %s\n", privHex)
	fmt.Println()
	fmt.Println("IMPORTANT: Store the private key as a GitHub Actions secret named SIGNING_KEY.")
	fmt.Println("           The public key goes into internal/updater/signing.go as updatePublicKeyHex.")
	fmt.Println("           Tip: rerun with -out <file> to write the key pair to a 0o600 file")
	fmt.Println("           instead of stdout if your shell/CI records transcripts.")
}

func signFile(path string) {
	// TrimSpace: CI secrets routinely carry a trailing newline, which would
	// fail hex decoding with a confusing "invalid private key hex".
	keyHex := strings.TrimSpace(os.Getenv("SIGNING_KEY"))
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
