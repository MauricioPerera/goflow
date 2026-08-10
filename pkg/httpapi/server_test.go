package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/model"
)

// newTestServer wires a real FileStore in a temp dir (no interface mocks)
// behind a GatedStore, exactly like cmd/server does in production.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	fs, err := catalog.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return NewServer(&catalog.GatedStore{Underlying: fs}, "secret-token")
}

func do(t *testing.T, srv *Server, method, path string, body any, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	}
	if auth {
		r.Header.Set("Authorization", "Bearer secret-token")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestHealth_NoAuth(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/health", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	m := decode(t, rec)
	if m["status"] != "ok" {
		t.Fatalf("body = %v, want status=ok", m)
	}
}

func TestUnauthorized_NoHeader(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/catalog", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	m := decode(t, rec)
	if m["error"] != "unauthorized" {
		t.Fatalf("body = %v, want error=unauthorized", m)
	}
	// The catalog handler reads the store via DescribeCombined; on an empty
	// store it would return 200 {"catalog":"(catalog is empty...)"}. A 401
	// here proves the handler never ran — i.e. auth short-circuited first.
	if strings.Contains(rec.Body.String(), "catalog is empty") {
		t.Fatalf("handler ran despite failed auth: %s", rec.Body.String())
	}
}

func TestUnauthorized_WrongToken(t *testing.T) {
	srv := newTestServer(t)
	r := httptest.NewRequest("GET", "/catalog", nil)
	r.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCatalog_WithAuth_ContainsBuiltinPiece(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/catalog", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	text, ok := m["catalog"].(string)
	if !ok {
		t.Fatalf("body = %v, want a catalog string", m)
	}
	// pieces.All() includes the "http" and "delay" pieces; DescribeCombined
	// prints them as "- <name>: <displayName>".
	if !strings.Contains(text, "http") && !strings.Contains(text, "delay") {
		t.Fatalf("catalog text does not mention a known built-in piece: %s", text)
	}
}

func TestPostPieces_RejectsDefinitionWithoutExamples(t *testing.T) {
	srv := newTestServer(t)
	// No Examples on the action -> the gate (Validate) rejects it.
	def := catalog.Definition{
		Name: "noexamples", DisplayName: "No Examples",
		Actions: []catalog.ActionDefinition{{
			Name: "do", DisplayName: "Do", Source: "(params) => ({ ok: true })",
		}},
	}
	rec := do(t, srv, "POST", "/pieces", def, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// Confirm it was NOT persisted: a direct Get on the underlying store.
	got, ok, err := srv.store.Get("noexamples")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatalf("rejected definition was persisted: %#v", got)
	}
}

func TestPostPieces_AcceptsValidDefinition(t *testing.T) {
	srv := newTestServer(t)
	def := catalog.Definition{
		Name: "validpiece", DisplayName: "Valid Piece", Description: "a test piece",
		Actions: []catalog.ActionDefinition{{
			Name: "ok", DisplayName: "OK", Description: "returns ok",
			Source: "(params) => ({ ok: true })",
			Examples: []catalog.Example{{
				Input:       map[string]any{},
				CheckOutput: true,
				WantOutput:  map[string]any{"ok": true},
			}},
		}},
	}
	rec := do(t, srv, "POST", "/pieces", def, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["saved"] != true || m["name"] != "validpiece" {
		t.Fatalf("body = %v, want saved=true name=validpiece", m)
	}

	// It must now appear in the combined catalog text.
	rec = do(t, srv, "GET", "/catalog", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "validpiece") {
		t.Fatalf("saved piece not in catalog: %s", rec.Body.String())
	}
}

func TestFlowsRun_SimpleCodeAction_Succeeds(t *testing.T) {
	srv := newTestServer(t)
	fv := model.FlowVersion{
		ID: "fv-test",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "double", DisplayName: "Double", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Input:  map[string]any{"n": 21},
					Source: `(params) => ({ doubled: params.n * 2 })`,
				},
			},
		},
	}
	rec := do(t, srv, "POST", "/flows/run", runRequest{Flow: fv}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v; body=%s", err, rec.Body.String())
	}
	// Verdict.Status marshals as "SUCCEEDED" (model.FlowRunSucceeded).
	verdict, _ := state["Verdict"].(map[string]any)
	if verdict["Status"] != string(model.FlowRunSucceeded) {
		t.Fatalf("Verdict.Status = %v, want SUCCEEDED; body=%s", verdict["Status"], rec.Body.String())
	}
	steps, _ := state["Steps"].(map[string]any)
	double, _ := steps["double"].(map[string]any)
	if double["Status"] != string(model.StepSucceeded) {
		t.Fatalf("double step status = %v, want SUCCEEDED", double["Status"])
	}
	output, _ := double["Output"].(map[string]any)
	doubled, _ := output["doubled"].(float64)
	if doubled != 42 {
		t.Fatalf("doubled = %v, want 42; output=%#v", output["doubled"], output)
	}
}

func TestFlowsRun_ReferencesNonexistentPiece_400(t *testing.T) {
	srv := newTestServer(t)
	fv := model.FlowVersion{
		ID: "fv-bad",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "badstep", DisplayName: "Bad", Type: model.ActionPiece,
				Piece: &model.PieceSettings{PieceName: "no_such_piece", ActionName: "no_such_action"},
			},
		},
	}
	rec := do(t, srv, "POST", "/flows/run", runRequest{Flow: fv}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	errs, ok := m["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("body = %v, want a non-empty errors array", m)
	}
}

func TestFlowsRun_MalformedJSON_400(t *testing.T) {
	srv := newTestServer(t)
	r := httptest.NewRequest("POST", "/flows/run", bytes.NewReader([]byte(`{"flow": {bad json`)))
	r.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestRecoverMiddleware_PanicBecomes500 drives the actual recover middleware
// the server uses, wrapping a handler that panics, and asserts it produces a
// 500 JSON body instead of propagating the panic.
func TestRecoverMiddleware_PanicBecomes500(t *testing.T) {
	srv := newTestServer(t)
	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("deliberate")
	})
	rec := httptest.NewRecorder()
	srv.recover(boom).ServeHTTP(rec, httptest.NewRequest("GET", "/anything", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	m := decode(t, rec)
	if m["error"] != "internal error" {
		t.Fatalf("body = %v, want error=internal error", m)
	}
}
