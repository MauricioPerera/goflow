// Command licensegen issues signed license files for distributed goflow
// binaries (see pkg/license) — a private operator tool, never linked
// into cmd/server, and never itself distributed to a licensee: it's the
// thing that HOLDS the private key.
//
// Two modes:
//
//	licensegen -genkey -out mykey.hex
//	  Generates a fresh Ed25519 keypair, writes the PRIVATE key (hex) to
//	  -out, and prints the PUBLIC key (hex) to stdout to paste into
//	  pkg/license's publicKeyHex constant. Run once per key rotation;
//	  the output file is a secret — see the printed warning.
//
//	licensegen -key mykey.hex -licensee "Acme Corp" -days 365 -out acme.license.json
//	  Signs a new License for licensee, valid from now for -days days,
//	  and writes it to -out — the file a licensee's cmd/server reads via
//	  GOFLOW_LICENSE_FILE.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"goflow/pkg/license"
)

func main() {
	genkey := flag.Bool("genkey", false, "generate a new Ed25519 keypair instead of signing a license")
	keyPath := flag.String("key", "", "path to a private key file produced by -genkey (required unless -genkey)")
	licensee := flag.String("licensee", "", "name of the license holder (required unless -genkey)")
	days := flag.Int("days", 365, "number of days the license is valid for, starting now")
	out := flag.String("out", "", "output file path (required)")
	flag.Parse()

	if *out == "" {
		log.Fatal("-out is required")
	}

	if *genkey {
		runGenKey(*out)
		return
	}

	if *keyPath == "" || *licensee == "" {
		log.Fatal("-key and -licensee are required when not using -genkey")
	}
	runSign(*keyPath, *licensee, *days, *out)
}

func runGenKey(out string) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatalf("generating keypair: %v", err)
	}
	if err := os.WriteFile(out, []byte(hex.EncodeToString(priv)), 0o600); err != nil {
		log.Fatalf("writing private key to %q: %v", out, err)
	}
	fmt.Printf("Private key written to %s (mode 0600) — KEEP THIS OFFLINE.\n", out)
	fmt.Println("Never commit it to any repository. Losing it means no new licenses can ever be issued under this key; leaking it means anyone can forge one.")
	fmt.Println()
	fmt.Println("Paste this into pkg/license/license.go's publicKeyHex constant:")
	fmt.Println(hex.EncodeToString(pub))
}

func runSign(keyPath, licensee string, days int, out string) {
	keyHex, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatalf("reading private key %q: %v", keyPath, err)
	}
	keyBytes, err := hex.DecodeString(string(keyHex))
	if err != nil {
		log.Fatalf("private key %q is not valid hex: %v", keyPath, err)
	}
	if len(keyBytes) != ed25519.PrivateKeySize {
		log.Fatalf("private key %q decodes to %d bytes, want %d", keyPath, len(keyBytes), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(keyBytes)

	now := time.Now().UTC()
	claims := license.Claims{
		Licensee:  licensee,
		IssuedAt:  now,
		ExpiresAt: now.AddDate(0, 0, days),
	}
	lic, err := license.Sign(priv, claims)
	if err != nil {
		log.Fatalf("signing license: %v", err)
	}
	if err := license.Save(out, lic); err != nil {
		log.Fatalf("saving license to %q: %v", out, err)
	}
	fmt.Printf("License issued to %q, valid %s -> %s, written to %s\n", licensee, claims.IssuedAt.Format(time.RFC3339), claims.ExpiresAt.Format(time.RFC3339), out)
}
