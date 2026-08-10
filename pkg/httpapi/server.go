// Package httpapi exposes goflow's engine over a minimal HTTP API — the
// first server in the project (everything below pkg/engine is a pure
// library until now). It deliberately adds no new dependencies: net/http,
// crypto/subtle, encoding/json, log, os, and io are all it uses.
//
// The server is a thin transport over the existing library: it assembles a
// piece.Registry fresh per request (built-in Go pieces plus whatever JS
// pieces are persisted in the catalog Store), runs flowvalidate before
// touching the engine, and serializes the returned *model.ExecutionState
// straight to JSON. Auth is a single shared bearer token compared in
// constant time; a missing/wrong token never reaches the handler.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"

	"goflow/pkg/catalog"
	"goflow/pkg/engine"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

// Server holds the shared, read-mostly state for every request: the catalog
// Store (a *catalog.GatedStore wrapping a *catalog.FileStore in practice,
// but typed as the interface so tests can pass any Store) and the bearer
// token every non-/health route is gated on.
type Server struct {
	store catalog.Store
	token string
}

// NewServer returns a Server that authorizes non-/health routes with token
// and reads/writes catalog Definitions through store.
func NewServer(store catalog.Store, token string) *Server {
	return &Server{store: store, token: token}
}

// Handler assembles the route table and wraps it with the middleware stack:
// outermost is logging (so every request — authorized or not — is logged),
// then recover (so a panicking handler becomes a 500 instead of killing the
// process), then per-route auth for everything except /health.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.Handle("/catalog", s.auth(http.HandlerFunc(s.handleCatalog)))
	mux.Handle("/pieces", s.auth(http.HandlerFunc(s.handlePieces)))
	mux.Handle("/flows/run", s.auth(http.HandlerFunc(s.handleFlowsRun)))
	return s.logging(s.recover(mux))
}

// buildRegistry assembles a fresh *piece.Registry on every call (no caching:
// a piece saved between two requests must be visible to the second one
// without a server restart). Built-in Go pieces (pieces.All) are registered
// first, then every Definition the store returns is converted via ToPiece
// and registered. A store.List failure is propagated, not silenced.
func (s *Server) buildRegistry() (*piece.Registry, error) {
	reg := piece.NewRegistry()
	for _, p := range pieces.All() {
		reg.Register(p)
	}
	defs, err := s.store.List()
	if err != nil {
		return nil, err
	}
	for _, def := range defs {
		reg.Register(def.ToPiece())
	}
	return reg, nil
}

// goCatalogMap builds the name -> DisplayName map DescribeCombined expects
// for the built-in Go-pieces section of the catalog text.
func goCatalogMap() map[string]string {
	m := make(map[string]string, len(pieces.All()))
	for _, p := range pieces.All() {
		m[p.Name] = p.DisplayName
	}
	return m
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	text, err := catalog.DescribeCombined(s.store, goCatalogMap())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"catalog": text})
}

func (s *Server) handlePieces(w http.ResponseWriter, r *http.Request) {
	var def catalog.Definition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// store is a GatedStore in production: Save runs Validate (which runs
	// every Example) and rejects a Definition without worked examples. Its
	// returned error is already descriptive — do not rewrite it.
	if err := s.store.Save(def); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"saved": true, "name": def.Name})
}

// runRequest is the body shape POST /flows/run expects.
type runRequest struct {
	Flow           model.FlowVersion `json:"flow"`
	Trigger        any               `json:"trigger"`
	ExecuteTrigger bool              `json:"executeTrigger"`
}

func (s *Server) handleFlowsRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	reg, err := s.buildRegistry()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if vErrs := flowvalidate.Validate(&req.Flow, reg); len(vErrs) > 0 {
		out := make([]map[string]string, len(vErrs))
		for i, e := range vErrs {
			out[i] = map[string]string{"path": e.Path, "message": e.Message}
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": out})
		return
	}

	eng := engine.New(reg)
	state := eng.ExecuteBegin(&req.Flow, engine.BeginInput{
		TriggerPayload: req.Trigger,
		ExecuteTrigger: req.ExecuteTrigger,
	})

	// The ExecutionState is the whole body — not wrapped in another object.
	// Its runtime types have no json tags, so fields marshal as their Go
	// names (Steps, Verdict, LogSize, ...), matching the rest of the repo.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(state)
}

// --- middleware -------------------------------------------------------------

// auth rejects any request whose Authorization header is not exactly
// "Bearer <token>", compared in constant time so a timing side channel
// can't leak how much of the token matched. On mismatch it returns 401 and
// never calls the wrapped handler.
func (s *Server) auth(next http.Handler) http.Handler {
	expected := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recover turns a panicking handler into a 500 instead of tearing down the
// serving goroutine (and, with the default server, the process).
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder wraps http.ResponseWriter to capture the status code for
// the access log. WriteHeader is recorded once; the first code wins (a
// handler that never calls WriteHeader gets 200, matching net/http).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// logging writes one line per request to the standard logger: method, path,
// and the recorded status. It sits outside auth so unauthorized requests
// are still visible in the log.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d", r.Method, r.URL.Path, rec.status)
	})
}

// writeJSON sets the JSON content type, writes the status, and encodes body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
