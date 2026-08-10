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
	"goflow/pkg/credentials"
	"goflow/pkg/engine"
	"goflow/pkg/flowstore"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

// Server holds the shared, read-mostly state for every request: the catalog
// Store (a *catalog.GatedStore wrapping a *catalog.FileStore in practice,
// but typed as the interface so tests can pass any Store), the named-flow
// store (a *flowstore.GatedStore wrapping whatever flowstore.Store the
// caller passed, built inside NewServer so the gate reuses this server's
// own buildRegistry), and the bearer token every non-/health route is gated
// on.
type Server struct {
	store     catalog.Store
	credStore credentials.Store
	flowStore *flowstore.GatedStore
	token     string
}

// NewServer returns a Server that authorizes non-/health routes with token,
// reads/writes catalog Definitions through store, manages encrypted
// credentials through credStore, and persists named flows through flowStore.
// flowStore is the RAW store (no gate): NewServer wraps it in a
// *flowstore.GatedStore itself, wiring the gate's BuildRegistry to this
// server's own buildRegistry — so /flows (save) validates a flow against the
// exact same piece registry /flows/run and every other route assembles,
// without duplicating that assembly in cmd/server/main.go. credStore is
// never asked to decrypt over HTTP — only Save/List/Delete are exposed; Get
// is for trusted Go callers.
func NewServer(store catalog.Store, credStore credentials.Store, flowStore flowstore.Store, token string) *Server {
	s := &Server{store: store, credStore: credStore, token: token}
	s.flowStore = &flowstore.GatedStore{Underlying: flowStore, BuildRegistry: s.buildRegistry}
	return s
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
	mux.Handle("POST /flows/run", s.auth(http.HandlerFunc(s.handleFlowsRun)))
	mux.Handle("/flows", s.auth(http.HandlerFunc(s.handleFlows)))
	mux.Handle("GET /flows/{name}", s.auth(http.HandlerFunc(s.handleFlowGet)))
	mux.Handle("DELETE /flows/{name}", s.auth(http.HandlerFunc(s.handleFlowDelete)))
	mux.Handle("POST /flows/{name}/run", s.auth(http.HandlerFunc(s.handleFlowRun)))
	mux.Handle("/credentials", s.auth(http.HandlerFunc(s.handleCredentials)))
	mux.Handle("DELETE /credentials/{name}", s.auth(http.HandlerFunc(s.handleCredentialDelete)))
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
	s.runFlowVersion(w, &req.Flow, req.Trigger, req.ExecuteTrigger)
}

// runFlowVersion is the shared run path for both POST /flows/run (an ad-hoc
// FlowVersion in the body) and POST /flows/{name}/run (a FlowVersion fetched
// from the store by name). It assembles a fresh registry, re-validates the
// flow against it (a flow saved long ago may reference a piece since removed
// from the catalog — caught here, not mid-run), and — if it passes — runs it
// through the engine and writes the *model.ExecutionState as the body. A
// registry or validation failure is a 400; the ExecutionState is the whole
// body on success, never wrapped (matching the ad-hoc route's shape).
func (s *Server) runFlowVersion(w http.ResponseWriter, fv *model.FlowVersion, trigger any, executeTrigger bool) {
	reg, err := s.buildRegistry()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if vErrs := flowvalidate.Validate(fv, reg); len(vErrs) > 0 {
		out := make([]map[string]string, len(vErrs))
		for i, e := range vErrs {
			out[i] = map[string]string{"path": e.Path, "message": e.Message}
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": out})
		return
	}

	eng := engine.New(reg)
	state := eng.ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: trigger,
		ExecuteTrigger: executeTrigger,
	})

	// The ExecutionState is the whole body — not wrapped in another object.
	// Its runtime types have no json tags, so fields marshal as their Go
	// names (Steps, Verdict, LogSize, ...), matching the rest of the repo.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(state)
}

// flowSummary is the metadata-only projection of a FlowDefinition returned
// by GET /flows — Name/DisplayName/Description only, never the full
// FlowVersion. Deliberately light: the listing is meant to be cheap, like a
// future tools/list of MCP, so a caller can pick a flow without every saved
// flow's whole definition crossing the wire.
type flowSummary struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// handleFlows routes POST (save a named flow) and GET (list flows,
// metadata-only) at /flows. POST runs the gate (flowvalidate against the
// current piece registry) inside flowStore.Save; a validation failure is a
// 400 with the store's already-descriptive error. GET never returns the
// FlowVersion — only the three metadata fields.
func (s *Server) handleFlows(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var def flowstore.FlowDefinition
		if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// flowStore is a GatedStore: Save runs flowvalidate.Validate against a
		// fresh registry and rejects a flow that doesn't pass. Its returned
		// error is already descriptive — do not rewrite it.
		if err := s.flowStore.Save(def); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"saved": true, "name": def.Name})

	case http.MethodGet:
		defs, err := s.flowStore.List()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		summaries := make([]flowSummary, 0, len(defs))
		for _, d := range defs {
			summaries = append(summaries, flowSummary{
				Name: d.Name, DisplayName: d.DisplayName, Description: d.Description,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"flows": summaries})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleFlowGet handles GET /flows/{name} — returns the full FlowDefinition
// (with the FlowVersion inside). A missing name is 404, matching the
// store's (zero, false, nil) contract.
func (s *Server) handleFlowGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	def, ok, err := s.flowStore.Get(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, def)
}

// handleFlowDelete handles DELETE /flows/{name}. The store's ErrNotFound maps
// to 404; any other error to 400. Success returns the name.
func (s *Server) handleFlowDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.flowStore.Delete(name); err != nil {
		if err == flowstore.ErrNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
}

// flowRunRequest is the body shape POST /flows/{name}/run expects. Both
// fields are optional: an absent trigger is nil (fine for an EMPTY trigger
// flow), and an absent executeTrigger defaults to false (pass the payload
// straight through, matching the ad-hoc route's default).
type flowRunRequest struct {
	Trigger        any  `json:"trigger"`
	ExecuteTrigger bool `json:"executeTrigger"`
}

// handleFlowRun handles POST /flows/{name}/run — fetch the saved flow by
// name (404 if missing), then run exactly the same path as POST /flows/run:
// re-validate against the current registry, and on success execute and
// return the *model.ExecutionState.
func (s *Server) handleFlowRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	def, ok, err := s.flowStore.Get(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	var req flowRunRequest
	// A missing/empty body is fine — both fields are optional. Only a
	// malformed JSON body is a 400.
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	s.runFlowVersion(w, &def.Flow, req.Trigger, req.ExecuteTrigger)
}

// credRequest is the body POST /credentials expects. Value is any JSON — a
// string, object, whatever the caller wants sealed. A null/absent value is
// rejected as 400 below.
type credRequest struct {
	Name  string `json:"name"`
	Value *any   `json:"value"`
}

// handleCredentials routes POST (save) and GET (list names) at /credentials.
// POST never echoes the value back — only {"saved":true,"name":...}. GET
// never returns values, only the sorted name list. There is intentionally no
// GET /credentials/{name} here: the decrypted value is never exposed over
// HTTP (see the security note on credentials.Store.Get).
func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req credRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		if req.Value == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "value is required"})
			return
		}
		if err := s.credStore.Save(req.Name, *req.Value); err != nil {
			// Includes invalid-name (path traversal) errors from the store's
			// own validation — surfaced verbatim, not rewritten.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"saved": true, "name": req.Name})

	case http.MethodGet:
		names, err := s.credStore.List()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"credentials": names})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleCredentialDelete handles DELETE /credentials/{name} using the Go
// 1.22+ ServeMux pattern routing (r.PathValue). The store's ErrNotFound maps
// to 404; any other error to 400. Success returns the name, never a value.
func (s *Server) handleCredentialDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.credStore.Delete(name); err != nil {
		if err == credentials.ErrNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
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
