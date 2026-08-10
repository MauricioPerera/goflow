// Command goflow-server runs pkg/httpapi's Server: the first HTTP entrypoint
// for the goflow engine. Configuration is environment-only (no flags) and
// deliberately minimal — addr, catalog directory, and a mandatory API token.
package main

import (
	"log"
	"net/http"
	"os"

	"goflow/pkg/catalog"
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

	// NewFileStore creates the directory if missing (os.MkdirAll inside),
	// so no separate mkdir here.
	fileStore, err := catalog.NewFileStore(catalogDir)
	if err != nil {
		log.Fatalf("opening catalog store at %q: %v", catalogDir, err)
	}
	gated := &catalog.GatedStore{Underlying: fileStore}

	srv := httpapi.NewServer(gated, token)
	log.Printf("goflow-server listening on %s (catalog: %s)", addr, catalogDir)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
