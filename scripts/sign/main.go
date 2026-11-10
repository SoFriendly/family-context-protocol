// Command sign manages the FCP release-signing key.
//
//	go run ./scripts/sign keygen        # once: writes the private key, prints the public key
//	go run ./scripts/sign sign FILE     # writes FILE.sig (base64 Ed25519)
//
// The private key lives at $FCP_SIGNING_KEY (default ~/.config/fcp/release-signing.key),
// never in the repo. Back it up in a password manager: without it, future
// releases can't be installed by `fcp update` on existing servers.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func keyPath() string {
	if p := os.Getenv("FCP_SIGNING_KEY"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "fcp", "release-signing.key")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sign keygen | sign sign FILE")
	}
	path := keyPath()
	switch args[0] {
	case "keygen":
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite the release key", path)
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("Private key: %s (back it up; never commit it)\nPublic key:  %s\n", path, base64.StdEncoding.EncodeToString(pub))
	case "sign":
		if len(args) != 2 {
			return errors.New("usage: sign sign FILE")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("no signing key (run keygen first): %w", err)
		}
		priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(priv) != ed25519.PrivateKeySize {
			return fmt.Errorf("%s is not an Ed25519 private key", path)
		}
		msg, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		sig := ed25519.Sign(ed25519.PrivateKey(priv), msg)
		return os.WriteFile(args[1]+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644)
	default:
		return errors.New("usage: sign keygen | sign sign FILE")
	}
	return nil
}
