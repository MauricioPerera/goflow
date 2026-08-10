// Command goflow-server runs pkg/httpapi's Server: the first HTTP entrypoint
// for the goflow engine. Configuration is environment-only (no flags) and
// deliberately minimal — addr, catalog directory, and a mandatory API token.
package main

import (
	"encoding/hex"
	"log"
	"net/http"
	"os"

	"goflow/pkg/catalog"
	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/httpapi"
)

func main() {
	addr := os.Getenv("GOFLOW_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	catalogDir := os.Getenv("GOFLOW_CATALOG_DIR")
	if catalogDir == "" {
		catalogDir = "./data/catalog"
	}

	token := os.Getenv("GOFLOW_API_TOKEN")
	if token == "" {
		// Never start without auth configured: an open engine endpoint that
		// runs arbitrary JS and persists pieces is not something to boot
		// unauthenticated by accident.
		log.Println("GOFLOW_API_TOKEN is not set or is empty — refusing to start without auth configured")
		os.Exit(1)
	}

	// GOFLOW_CREDENTIALS_KEY is a 64-char hex string (32 bytes) — the AES-256
	// key for the credential vault. Never start with the cipher misconfigured:
	// a missing, non-hex, or wrong-length key is a hard failure, same as the
	// API token. No truncation, no padding, no silent default.
	keyHex := os.Getenv("GOFLOW_CREDENTIALS_KEY")
	if keyHex == "" {
		log.Println("GOFLOW_CREDENTIALS_KEY is not set or is empty — refusing to start without credential encryption configured")
		os.Exit(1)
	}
	credKey, err := hex.DecodeString(keyHex)
	if err != nil {
		log.Printf("GOFLOW_CREDENTIALS_KEY is not valid hex: %v — refusing to start", err)
		os.Exit(1)
	}
	if len(credKey) != 32 {
		log.Printf("GOFLOW_CREDENTIALS_KEY decodes to %d bytes, want 32 — refusing to start", len(credKey))
		os.Exit(1)
	}

	credentialsDir := os.Getenv("GOFLOW_CREDENTIALS_DIR")
	if credentialsDir == "" {
		credentialsDir = "./data/credentials"
	}

	flowsDir := os.Getenv("GOFLOW_FLOWS_DIR")
	if flowsDir == "" {
		flowsDir = "./data/flows"
	}

	// NewFileStore creates the directory if missing (os.MkdirAll inside),
	// so no separate mkdir here.
	fileStore, err := catalog.NewFileStore(catalogDir)
	if err != nil {
		log.Fatalf("opening catalog store at %q: %v", catalogDir, err)
	}
	gated := &catalog.GatedStore{Underlying: fileStore}

	credStore, err := credentials.NewFileStore(credentialsDir, credKey)
	if err != nil {
		log.Fatalf("opening credentials store at %q: %v", credentialsDir, err)
	}

	// flowStore is the RAW store — NewServer wraps it in a GatedStore
	// internally, wiring the gate's BuildRegistry to the server's own
	// buildRegistry so /flows validates against the same piece registry every
	// other route assembles. Pass it raw, same as catalog above.
	flowStore, err := flowstore.NewFileStore(flowsDir)
	if err != nil {
		log.Fatalf("opening flows store at %q: %v", flowsDir, err)
	}

	srv := httpapi.NewServer(gated, credStore, flowStore, token)
	log.Printf("goflow-server listening on %s (catalog: %s, credentials: %s, flows: %s)", addr, catalogDir, credentialsDir, flowsDir)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
