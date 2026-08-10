// Command goflow-server runs pkg/httpapi's Server: the first HTTP entrypoint
// for the goflow engine. Configuration is environment-only (no flags) and
// deliberately minimal — addr, catalog directory, and a mandatory API token.
package main

import (
	"context"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"goflow/pkg/catalog"
	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/httpapi"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
	"goflow/pkg/scheduler"
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

	runsDir := os.Getenv("GOFLOW_RUNS_DIR")
	if runsDir == "" {
		runsDir = "./data/runs"
	}

	// GOFLOW_SCHEDULER_TICK is how often pkg/scheduler checks every saved
	// flow for a due schedule trigger — NOT any individual flow's own
	// intervalSeconds (that's per-flow, configured on the flow itself). A
	// tick longer than some flow's intervalSeconds delays that flow firing
	// on schedule; see pkg/scheduler's doc comment. Default 1s: fine-grained
	// enough for a schedule.PieceName flow configured with a 1s interval to
	// actually fire close to on time, cheap enough (one flowStore.List() per
	// tick) not to matter at this project's scale.
	tickStr := os.Getenv("GOFLOW_SCHEDULER_TICK")
	tick := time.Second
	if tickStr != "" {
		secs, err := strconv.Atoi(tickStr)
		if err != nil || secs <= 0 {
			log.Printf("GOFLOW_SCHEDULER_TICK=%q is not a positive integer (seconds) — refusing to start", tickStr)
			os.Exit(1)
		}
		tick = time.Duration(secs) * time.Second
	}

	// GOFLOW_OAUTH_ISSUER is this server's externally-reachable base URL —
	// required for the OAuth 2.1 metadata pkg/oauth serves to point a client
	// at URLs it can actually reach (e.g. the real https://host behind a
	// reverse proxy, not the loopback address this process binds to).
	// Defaults to a plain http://<addr> for local/direct use (e.g. against
	// the MCP Inspector on localhost), where that default IS reachable.
	issuer := os.Getenv("GOFLOW_OAUTH_ISSUER")
	if issuer == "" {
		issuer = "http://" + addr
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

	runStore, err := runstore.NewFileStore(runsDir)
	if err != nil {
		log.Fatalf("opening runs store at %q: %v", runsDir, err)
	}

	srv := httpapi.NewServer(gated, credStore, flowStore, runStore, token, issuer)

	// The scheduler needs its own *flowstore.GatedStore over the same
	// flowsDir httpapi.NewServer just wrapped internally — a second Go
	// object, but not a second source of truth: both ultimately validate
	// against catalog.BuildRegistry(gated), the same shared assembly
	// httpapi.Server.buildRegistry now also just calls (see
	// pkg/catalog/registry.go), so the two can't drift on what pieces exist.
	schedFlowStore := &flowstore.GatedStore{
		Underlying:    flowStore,
		BuildRegistry: func() (*piece.Registry, error) { return catalog.BuildRegistry(gated) },
	}
	sched := scheduler.New(schedFlowStore, schedFlowStore.BuildRegistry, credStore, runStore, tick)
	// No graceful shutdown here, deliberately matching http.ListenAndServe
	// below: this process has none today (a kill/systemd-stop ends both
	// together), so context.Background() rather than a cancellable context
	// wired to a signal handler doesn't introduce a NEW inconsistency —
	// it matches the HTTP server's own existing behavior.
	go sched.Run(context.Background())

	log.Printf("goflow-server listening on %s (catalog: %s, credentials: %s, flows: %s, runs: %s, oauth issuer: %s, scheduler tick: %s)", addr, catalogDir, credentialsDir, flowsDir, runsDir, issuer, tick)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
