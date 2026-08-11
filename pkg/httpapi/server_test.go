package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
	"goflow/pkg/runstore"
)

// testCredKey is a fixed 32-byte AES-256 key for the credentials store in
// tests — constant so the raw-file checks are deterministic.
var testCredKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

// newTestServer wires a real FileStore in a temp dir (no interface mocks)
// behind a GatedStore, exactly like cmd/server does in production. It also
// wires a real credentials.FileStore in its own temp dir and a real
// flowstore.FileStore in its own temp dir (passed raw — NewServer gates it
// internally, same as cmd/server).
func newTestServer(t *testing.T) *Server {
	t.Helper()
	fs, err := catalog.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	credStore, err := credentials.NewFileStore(t.TempDir(), testCredKey)
	if err != nil {
		t.Fatalf("credentials.NewFileStore: %v", err)
	}
	flowStore, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("flowstore.NewFileStore: %v", err)
	}
	runStore, err := runstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("runstore.NewFileStore: %v", err)
	}
	return NewServer(&catalog.GatedStore{Underlying: fs}, credStore, flowStore, runStore, "secret-token", "http://testserver")
}

// credDir returns the on-disk directory backing the server's credentials
// store, so tests can read raw .enc files directly. The store is a
// *credentials.FileStore in tests; reach in via the interface to its dir.
func credDir(t *testing.T, srv *Server) string {
	t.Helper()
	fs, ok := srv.credStore.(*credentials.FileStore)
	if !ok {
		t.Fatalf("credStore is %T, want *credentials.FileStore", srv.credStore)
	}
	return fs.Dir()
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

func TestPiecesExport_ReturnsFullDefinitionIncludingSourceAndExamples(t *testing.T) {
	srv := newTestServer(t)
	def := catalog.Definition{
		Name: "exportable", DisplayName: "Exportable", Description: "a test piece",
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
	if rec := do(t, srv, "POST", "/pieces", def, true); rec.Code != http.StatusCreated {
		t.Fatalf("save status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, srv, "GET", "/pieces/export", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Pieces []catalog.Definition `json:"pieces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Pieces) != 1 || body.Pieces[0].Name != "exportable" {
		t.Fatalf("pieces = %+v, want exactly one named \"exportable\"", body.Pieces)
	}
	got := body.Pieces[0]
	if len(got.Actions) != 1 || got.Actions[0].Source != "(params) => ({ ok: true })" {
		t.Fatalf("exported Definition is missing its action Source, want the DescribeCombined-lossy fields to be REAL data here: %+v", got)
	}
	if len(got.Actions[0].Examples) != 1 {
		t.Fatalf("exported Definition is missing its action Examples: %+v", got.Actions[0])
	}
}

func TestPiecesExport_IncludesRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	def := catalog.Definition{
		Name: "slack-poster", DisplayName: "Slack Poster",
		Actions: []catalog.ActionDefinition{{
			Name: "post", DisplayName: "Post",
			Source:       "(ctx) => ({ ok: true })",
			RequiresAuth: "Slack Bot Token (string, starts with xoxb-)",
			Examples: []catalog.Example{{
				Input: map[string]any{}, CheckOutput: true, WantOutput: map[string]any{"ok": true},
			}},
		}},
	}
	if rec := do(t, srv, "POST", "/pieces", def, true); rec.Code != http.StatusCreated {
		t.Fatalf("save status = %d, want 201", rec.Code)
	}

	rec := do(t, srv, "GET", "/pieces/export", nil, true)
	var body struct {
		Pieces []catalog.Definition `json:"pieces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Pieces) != 1 || body.Pieces[0].Actions[0].RequiresAuth != "Slack Bot Token (string, starts with xoxb-)" {
		t.Fatalf("pieces = %+v, want RequiresAuth to round-trip", body.Pieces)
	}
}

func TestPiecesExport_EmptyCatalog_ReturnsEmptyArray(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/pieces/export", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	pieces, ok := m["pieces"].([]any)
	if !ok || len(pieces) != 0 {
		t.Fatalf("pieces = %v, want an empty array", m["pieces"])
	}
}

func TestPiecesExport_NoAuth_401(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/pieces/export", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestFlowRun_CallFlow_InvokesNamedFlow_RecordedInHistory(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", notifyFlowDef("leaf"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save leaf: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	root := flowstore.FlowDefinition{
		Name: "root", DisplayName: "Root",
		Flow: model.FlowVersion{
			ID: "fv-root",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "call", DisplayName: "Call", Type: model.ActionCallFlow,
					CallFlow: &model.CallFlowSettings{FlowName: "leaf"},
				},
			},
		},
	}
	if rec := do(t, srv, "POST", "/flows", root, true); rec.Code != http.StatusCreated {
		t.Fatalf("save root: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, srv, "POST", "/flows/root/run", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if verdict, _ := state["Verdict"].(map[string]any); verdict["Status"] != string(model.FlowRunSucceeded) {
		t.Fatalf("Verdict = %v, want SUCCEEDED", state["Verdict"])
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	names := map[string]int{}
	for _, r := range runs {
		rm, _ := r.(map[string]any)
		names[rm["FlowName"].(string)]++
	}
	if names["root"] != 1 || names["leaf"] != 1 {
		t.Fatalf("recorded runs = %v, want exactly one for \"root\" and one for \"leaf\"", names)
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

func simpleLinearFlow() model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-export-test",
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
}

func TestFlowsExportJS_SimpleCodeChain_Succeeds(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/flows/export/js", exportRequest{Flow: simpleLinearFlow()}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("Content-Type = %q, want application/javascript", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "async function runFlow(") {
		t.Fatalf("body missing runFlow, want the generated exporter module:\n%s", body)
	}
	if !strings.Contains(body, `"id": "fv-export-test"`) {
		t.Fatalf("body missing the embedded flow JSON:\n%s", body)
	}
}

func TestFlowsExportJS_UnsupportedFlow_400(t *testing.T) {
	srv := newTestServer(t)
	fv := model.FlowVersion{
		ID: "fv-bad-export",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "route", DisplayName: "Route", Type: model.ActionRouter,
				Router: &model.RouterSettings{},
			},
		},
	}
	rec := do(t, srv, "POST", "/flows/export/js", exportRequest{Flow: fv}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	errStr, _ := m["error"].(string)
	if !strings.Contains(errStr, "route") || !strings.Contains(errStr, "ROUTER") {
		t.Fatalf("error = %q, want it to name the unsupported ROUTER action", errStr)
	}
}

func TestFlowsExportJS_NoAuth_401(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/flows/export/js", exportRequest{Flow: simpleLinearFlow()}, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestFlowExportJS_NamedFlow_Succeeds(t *testing.T) {
	srv := newTestServer(t)
	fv := simpleLinearFlow()
	def := flowstore.FlowDefinition{Name: "my-flow", DisplayName: "My Flow", Flow: fv}
	rec := do(t, srv, "POST", "/flows", def, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, srv, "POST", "/flows/my-flow/export/js", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "async function runFlow(") {
		t.Fatalf("body missing runFlow:\n%s", rec.Body.String())
	}
}

func TestFlowExportJS_UnknownName_404(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/flows/no-such-flow/export/js", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
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

// --- credentials -------------------------------------------------------------

func TestPostCredentials_NoAuth_401_NoFileWritten(t *testing.T) {
	srv := newTestServer(t)
	dir := credDir(t, srv)
	rec := do(t, srv, "POST", "/credentials", map[string]any{"name": "x", "value": "y"}, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// Auth short-circuits before the handler runs, so nothing is written.
	if _, err := os.Stat(filepath.Join(dir, "x.enc")); err == nil {
		t.Fatalf("credential file written despite failed auth")
	}
}

func TestPostCredentials_Valid_SecretNotInRawFile(t *testing.T) {
	srv := newTestServer(t)
	dir := credDir(t, srv)
	secret := "el-secreto-super-unico-xyz123"
	rec := do(t, srv, "POST", "/credentials", map[string]any{"name": "vault", "value": secret}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["saved"] != true || m["name"] != "vault" {
		t.Fatalf("body = %v, want saved=true name=vault", m)
	}
	// The response must not leak the value.
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("response body leaks the secret: %s", rec.Body.String())
	}
	// The on-disk file must not contain the plaintext secret.
	raw, err := os.ReadFile(filepath.Join(dir, "vault.enc"))
	if err != nil {
		t.Fatalf("read raw file: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("plaintext secret in raw file:\n%s", raw)
	}
}

func TestGetCredentials_ListsNamesSortedNoValues(t *testing.T) {
	srv := newTestServer(t)
	// Save three with recognizable secret values, in non-sorted order.
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		rec := do(t, srv, "POST", "/credentials", map[string]any{"name": n, "value": "secret-value-" + n}, true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("save %q: status = %d, want 201; body=%s", n, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, srv, "GET", "/credentials", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	m := decode(t, rec)
	names, ok := m["credentials"].([]any)
	if !ok {
		t.Fatalf("body = %v, want credentials array", m)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("names = %v, want %v (sorted)", names, want)
		}
	}
	// The body must not contain any of the secret values.
	body := rec.Body.String()
	for _, n := range want {
		if strings.Contains(body, "secret-value-"+n) {
			t.Fatalf("GET /credentials body leaks a value: %s", body)
		}
	}
}

func TestDeleteCredential_ExistingThenGone(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/credentials", map[string]any{"name": "killme", "value": "v"}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d, want 201", rec.Code)
	}
	rec = do(t, srv, "DELETE", "/credentials/killme", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["deleted"] != true || m["name"] != "killme" {
		t.Fatalf("body = %v, want deleted=true name=killme", m)
	}
	// A subsequent GET /credentials no longer lists it.
	rec = do(t, srv, "GET", "/credentials", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "killme") {
		t.Fatalf("deleted name still listed: %s", rec.Body.String())
	}
}

func TestDeleteCredential_Missing_404(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "DELETE", "/credentials/nope", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["error"] != "not found" {
		t.Fatalf("body = %v, want error=not found", m)
	}
}

func TestPostCredentials_InvalidName_400NothingWritten(t *testing.T) {
	srv := newTestServer(t)
	dir := credDir(t, srv)
	for _, name := range []string{"", "../escape"} {
		body := map[string]any{"name": name, "value": "v"}
		rec := do(t, srv, "POST", "/credentials", body, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name=%q: status = %d, want 400; body=%s", name, rec.Code, rec.Body.String())
		}
	}
	// Nothing landed outside the credentials dir (the ../escape attempt).
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.enc")); err == nil {
		t.Fatalf("traversal wrote outside credentials dir")
	}
	// And the empty-name case wrote nothing inside either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".enc") {
			t.Fatalf("unexpected credential file written: %s", e.Name())
		}
	}
}

// --- named flows (pkg/flowstore over HTTP) ---------------------------------

// validFlowDef is a FlowDefinition wrapping the no-piece double-it flow —
// the same shape POST /flows/run already accepts inline, now with a name.
func validFlowDef(name string) flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name:        name,
		DisplayName: "Double It",
		Description: "doubles n",
		InputSchema: "n (number, required)",
		Flow: model.FlowVersion{
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
		},
	}
}

func TestPostFlows_NoAuth_401(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/flows", validFlowDef("x"), false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPostFlows_ReferencesNonexistentPiece_400NotPersisted(t *testing.T) {
	srv := newTestServer(t)
	def := flowstore.FlowDefinition{
		Name: "badflow", DisplayName: "Bad",
		Flow: model.FlowVersion{
			ID: "fv-bad",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "badstep", DisplayName: "Bad", Type: model.ActionPiece,
					Piece: &model.PieceSettings{PieceName: "no_such_piece", ActionName: "no_such_action"},
				},
			},
		},
	}
	rec := do(t, srv, "POST", "/flows", def, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// The rejection error mentions validation failure.
	if !strings.Contains(rec.Body.String(), "failed validation") {
		t.Fatalf("body = %s, want a 'failed validation' error", rec.Body.String())
	}
	// Confirm it was NOT persisted: GET /flows/badflow -> 404.
	rec = do(t, srv, "GET", "/flows/badflow", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after rejected save: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPostFlows_Valid_201ListedMetadataOnly_GetFull(t *testing.T) {
	srv := newTestServer(t)
	def := validFlowDef("double-it")
	rec := do(t, srv, "POST", "/flows", def, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["saved"] != true || m["name"] != "double-it" {
		t.Fatalf("body = %v, want saved=true name=double-it", m)
	}

	// GET /flows lists it metadata-only: name/displayName/description, and
	// MUST NOT carry the full FlowVersion (no "Flow", no action name
	// "double", no "Source").
	rec = do(t, srv, "GET", "/flows", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"double-it"`) {
		t.Fatalf("list body missing name: %s", body)
	}
	if !strings.Contains(body, `"displayName":"Double It"`) {
		t.Fatalf("list body missing displayName: %s", body)
	}
	if !strings.Contains(body, `"description":"doubles n"`) {
		t.Fatalf("list body missing description: %s", body)
	}
	for _, leak := range []string{`"Flow"`, `"double"`, `"Source"`, `"doubled"`} {
		if strings.Contains(body, leak) {
			t.Fatalf("list body leaks the full FlowVersion (%s): %s", leak, body)
		}
	}

	// GET /flows/double-it returns the FULL definition, Flow included.
	rec = do(t, srv, "GET", "/flows/double-it", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	full := decode(t, rec)
	flow, ok := full["Flow"].(map[string]any)
	if !ok {
		t.Fatalf("get body missing Flow: %v", full)
	}
	trigger, _ := flow["trigger"].(map[string]any)
	if trigger["type"] != string(model.TriggerEmpty) {
		t.Fatalf("Flow.trigger.type = %v, want EMPTY", trigger["type"])
	}
}

func TestDeleteFlow_ExistingThenGone_Missing404(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", validFlowDef("killme"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	rec := do(t, srv, "DELETE", "/flows/killme", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["deleted"] != true || m["name"] != "killme" {
		t.Fatalf("body = %v, want deleted=true name=killme", m)
	}
	// A subsequent GET is 404.
	rec = do(t, srv, "GET", "/flows/killme", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: status = %d, want 404", rec.Code)
	}
	// Deleting a name that was never saved is 404, not 400.
	rec = do(t, srv, "DELETE", "/flows/nope", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	m = decode(t, rec)
	if m["error"] != "not found" {
		t.Fatalf("body = %v, want error=not found", m)
	}
}

func TestPostFlowRun_ByName_Succeeds(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", validFlowDef("double-it"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	rec := do(t, srv, "POST", "/flows/double-it/run", flowRunRequest{}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v; body=%s", err, rec.Body.String())
	}
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

func TestPostFlowRun_UnknownName_404(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/flows/never-saved/run", flowRunRequest{}, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["error"] != "not found" {
		t.Fatalf("body = %v, want error=not found", m)
	}
}

// credentialFlowDef is a named flow whose CODE step references a stored
// credential by name via the $credential marker. The step returns the
// resolved auth's length, so a successful run proves the real secret reached
// the piece — without ever putting the secret itself in the output.
func credentialFlowDef(name, credName string) flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name:        name,
		DisplayName: "Uses Credential",
		Description: "uses a $credential marker",
		InputSchema: "none",
		Flow: model.FlowVersion{
			ID: "fv-cred",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "use_auth", DisplayName: "Use Cred", Type: model.ActionCode,
					Code: &model.CodeSettings{
						Input:  map[string]any{"auth": map[string]any{"$credential": credName}},
						Source: `(params) => ({ authLen: params.auth.length })`,
					},
				},
			},
		},
	}
}

// TestPostFlowRun_ByName_WithCredential_SecretNotInBody_RealValueUsed is the
// end-to-end HTTP proof for the named-flow run path: a real credential is
// stored over HTTP, a flow referencing it by name is saved over HTTP, the
// flow is run via POST /flows/{name}/run, and the RAW response body must not
// contain the secret anywhere — while the step's output authLen == len(secret)
// proves the real value reached the piece. This is the test that proves the
// redaction works over the wire on the HTTP path, not just at the library
// level.
func TestPostFlowRun_ByName_WithCredential_SecretNotInBody_RealValueUsed(t *testing.T) {
	srv := newTestServer(t)
	const secret = "el-secreto-xyz-789"
	// Store the real credential over HTTP.
	rec := do(t, srv, "POST", "/credentials", map[string]any{"name": "relay", "value": secret}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save credential: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// Save the flow that references it by name over HTTP.
	rec = do(t, srv, "POST", "/flows", credentialFlowDef("uses-cred", "relay"), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save flow: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// Run it by name over HTTP.
	rec = do(t, srv, "POST", "/flows/uses-cred/run", flowRunRequest{}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Read the RAW HTTP body — the exact bytes a client would receive — and
	// assert the secret is nowhere in it.
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("real secret leaked into the POST /flows/{name}/run response body:\n%s", body)
	}

	// The run succeeded; the step's Input holds the placeholder, and its
	// Output authLen == len(secret) proves the real secret reached the piece.
	var state map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v; body=%s", err, body)
	}
	verdict, _ := state["Verdict"].(map[string]any)
	if verdict["Status"] != string(model.FlowRunSucceeded) {
		t.Fatalf("Verdict.Status = %v, want SUCCEEDED; body=%s", verdict["Status"], body)
	}
	steps, _ := state["Steps"].(map[string]any)
	useAuth, _ := steps["use_auth"].(map[string]any)
	input, _ := useAuth["Input"].(map[string]any)
	if input["auth"] != "<credential:relay>" {
		t.Fatalf("Input[auth] = %v, want placeholder <credential:relay>", input["auth"])
	}
	output, _ := useAuth["Output"].(map[string]any)
	authLen, _ := output["authLen"].(float64)
	if int(authLen) != len(secret) {
		t.Fatalf("authLen = %v, want %d (proves the real secret reached the piece)", authLen, len(secret))
	}
}

// --- MCP (/mcp) over HTTP ---------------------------------------------------

// TestPostMcp_NoAuth_401 confirms /mcp sits behind the same bearer-token
// middleware as every other route — an unauthenticated request never reaches
// the MCP handler.
func TestPostMcp_NoAuth_401(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	}, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["error"] != "unauthorized" {
		t.Fatalf("body = %v, want error=unauthorized", m)
	}
}

// TestPostMcp_WithAuth_Initialize_OK drives the full stack end-to-end — auth
// middleware, mux routing, and the mounted mcpapi.Handler — with a real
// initialize request, asserting a valid JSON-RPC success response.
func TestPostMcp_WithAuth_Initialize_OK(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 42, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2026-06-18"},
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want 2.0", m["jsonrpc"])
	}
	if m["id"] != float64(42) {
		t.Fatalf("id = %v, want 42 (echoed)", m["id"])
	}
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want an object; body=%s", m["result"], rec.Body.String())
	}
	if result["protocolVersion"] != "2026-06-18" {
		t.Fatalf("protocolVersion = %v, want 2026-06-18", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "goflow-mcp" {
		t.Fatalf("serverInfo.name = %v, want goflow-mcp", info["name"])
	}
}

// TestPostMcp_NoAuth_401_AdvertisesOAuthResourceMetadata proves the exact
// gap README.md documented before pkg/oauth existed is now closed at the
// wire level: a 401 on /mcp specifically (not on routes with no OAuth story
// of their own) carries a WWW-Authenticate hint pointing a spec-compliant
// client at this server's protected-resource metadata (RFC 9728), instead of
// leaving it to retry the same bare token forever.
func TestPostMcp_NoAuth_401_AdvertisesOAuthResourceMetadata(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer resource_metadata="http://testserver/.well-known/oauth-protected-resource"`
	if got != want {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
	}

	// A route with no OAuth resource story of its own (/catalog) must NOT
	// carry the same hint — it's specific to /mcp, the one OAuth-protected
	// resource this server has.
	rec2 := do(t, srv, "GET", "/catalog", nil, false)
	if h := rec2.Header().Get("WWW-Authenticate"); h != "" {
		t.Fatalf("/catalog's 401 carries WWW-Authenticate = %q, want none", h)
	}
}

// TestOAuthWellKnown_ReachableWithoutAuth proves the OAuth discovery/
// registration surface is reachable by a client that has no token yet — the
// entire point of implementing OAuth in the first place. If these required
// auth, a client would be stuck exactly where it started.
func TestOAuthWellKnown_ReachableWithoutAuth(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-protected-resource",
	} {
		rec := do(t, srv, "GET", path, nil, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200, body=%s", path, rec.Code, rec.Body.String())
		}
	}

	registerBody, _ := json.Marshal(map[string]any{
		"client_name":   "test client",
		"redirect_uris": []string{"http://cb.example/callback"},
	})
	r := httptest.NewRequest("POST", "/oauth/register", bytes.NewReader(registerBody))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /oauth/register (no auth): status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
}

// TestOAuthEndToEnd_AccessTokenGrantsTheSameAccessAsStaticToken drives the
// full OAuth 2.1 dance through the REAL server — registration, authorization
// (via the Bearer-header path, proving the static token is what backs the
// whole flow), and a code-for-token exchange — then uses the resulting
// access token exactly like the static token on two different routes
// (/catalog and /mcp), proving the auth middleware genuinely accepts either
// credential form for anything it already gates, not just for /mcp.
func TestOAuthEndToEnd_AccessTokenGrantsTheSameAccessAsStaticToken(t *testing.T) {
	srv := newTestServer(t)

	registerBody, _ := json.Marshal(map[string]any{
		"client_name":   "e2e client",
		"redirect_uris": []string{"http://cb.example/callback"},
	})
	rReg := httptest.NewRequest("POST", "/oauth/register", bytes.NewReader(registerBody))
	recReg := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recReg, rReg)
	if recReg.Code != http.StatusCreated {
		t.Fatalf("register: status = %d, body=%s", recReg.Code, recReg.Body.String())
	}
	clientID := decode(t, recReg)["client_id"].(string)

	verifier := "e2e-test-verifier-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authQ := url.Values{
		"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri": {"http://cb.example/callback"}, "state": {"s"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	rAuth := httptest.NewRequest("GET", "/oauth/authorize?"+authQ.Encode(), nil)
	rAuth.Header.Set("Authorization", "Bearer secret-token")
	recAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recAuth, rAuth)
	if recAuth.Code != http.StatusFound {
		t.Fatalf("authorize: status = %d, want 302, body=%s", recAuth.Code, recAuth.Body.String())
	}
	loc, err := url.Parse(recAuth.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("Location %q carries no code", loc)
	}

	tokenForm := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {"http://cb.example/callback"}, "client_id": {clientID},
		"code_verifier": {verifier},
	}
	rTok := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tokenForm.Encode()))
	rTok.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recTok := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recTok, rTok)
	if recTok.Code != http.StatusOK {
		t.Fatalf("token exchange: status = %d, body=%s", recTok.Code, recTok.Body.String())
	}
	accessToken := decode(t, recTok)["access_token"].(string)
	if accessToken == "" {
		t.Fatal("token exchange returned an empty access_token")
	}
	if accessToken == "secret-token" {
		t.Fatal("access_token must never equal the static token")
	}

	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/catalog", nil},
		{"POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}},
	} {
		var r *http.Request
		if req.body == nil {
			r = httptest.NewRequest(req.method, req.path, nil)
		} else {
			raw, _ := json.Marshal(req.body)
			r = httptest.NewRequest(req.method, req.path, bytes.NewReader(raw))
		}
		r.Header.Set("Authorization", "Bearer "+accessToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s with OAuth access token: status = %d, want 200, body=%s", req.method, req.path, rec.Code, rec.Body.String())
		}
	}
}

func TestGetRuns_NoAuth_401(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/runs", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGetRuns_Empty_EmptyList(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/runs", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	runs, ok := m["runs"].([]any)
	if !ok || len(runs) != 0 {
		t.Fatalf("runs = %v, want an empty list", m["runs"])
	}
}

// TestPostFlowsRun_RecordedInHistory proves an AD-HOC run (POST /flows/run,
// no persisted flow involved) shows up in GET /runs with an empty flowName —
// runstore.RunWithHistory records it just like a named run.
func TestPostFlowsRun_RecordedInHistory(t *testing.T) {
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
	if rec := do(t, srv, "POST", "/flows/run", runRequest{Flow: fv}, true); rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200; body=%s", listRec.Code, listRec.Body.String())
	}
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want exactly 1 recorded run", m["runs"])
	}
	summary, _ := runs[0].(map[string]any)
	if summary["FlowName"] != "" {
		t.Fatalf("FlowName = %v, want \"\" for an ad-hoc run", summary["FlowName"])
	}
	if summary["Status"] != string(model.FlowRunSucceeded) {
		t.Fatalf("Status = %v, want SUCCEEDED", summary["Status"])
	}
	id, _ := summary["ID"].(string)
	if id == "" {
		t.Fatal("summary has no ID")
	}

	getRec := do(t, srv, "GET", "/runs/"+id, nil, true)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200; body=%s", getRec.Code, getRec.Body.String())
	}
	full := decode(t, getRec)
	state, _ := full["State"].(map[string]any)
	steps, _ := state["Steps"].(map[string]any)
	if _, ok := steps["double"]; !ok {
		t.Fatalf("recorded run's State.Steps missing \"double\": %v", state)
	}
}

// TestPostFlowRun_ByName_RecordedWithFlowName proves a NAMED run
// (POST /flows/{name}/run) is recorded with that name, distinguishing it
// from an ad-hoc run in the same history.
func TestPostFlowRun_ByName_RecordedWithFlowName(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", validFlowDef("double-it"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, "POST", "/flows/double-it/run", flowRunRequest{}, true); rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want exactly 1 recorded run", m["runs"])
	}
	summary, _ := runs[0].(map[string]any)
	if summary["FlowName"] != "double-it" {
		t.Fatalf("FlowName = %v, want %q", summary["FlowName"], "double-it")
	}
}

func TestGetRun_UnknownID_404(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "GET", "/runs/never-existed", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPostFlowsRun_ValidationFailure_NotRecordedInHistory proves a flow that
// never actually ran (rejected by flowvalidate before execution) does NOT
// pollute run history — mirrors
// flowstore.TestRunWithHistory_ValidationFailure_NotRecorded through the
// real HTTP layer.
func TestPostFlowsRun_ValidationFailure_NotRecordedInHistory(t *testing.T) {
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
	if rec := do(t, srv, "POST", "/flows/run", runRequest{Flow: fv}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	if len(runs) != 0 {
		t.Fatalf("runs = %v, want nothing recorded for a flow that never ran", m["runs"])
	}
}

// --- POST /webhooks/{name} ---------------------------------------------------

// webhookEchoFlowDef reads n from the TRIGGER payload (not a hardcoded step
// input, unlike validFlowDef) via {{ trigger_1.output.n }} — the shape a
// real webhook flow needs, since the payload is whatever the external
// sender's POST body carries.
func webhookEchoFlowDef(name string, enabled bool, secretCred string) flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name: name, DisplayName: "Webhook Echo", Description: "doubles n from the webhook payload",
		WebhookEnabled: enabled, WebhookSecretCredential: secretCred,
		Flow: model.FlowVersion{
			ID: "fv-webhook-echo",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "double", DisplayName: "Double", Type: model.ActionCode,
					Code: &model.CodeSettings{
						Input:  map[string]any{"n": "{{ trigger_1.output.n }}"},
						Source: `(params) => ({ doubled: params.n * 2 })`,
					},
				},
			},
		},
	}
}

// webhookReplyFlowDef calls the webhook_reply piece's actionName ("stop" or
// "respond") with a custom status/body/header, so a test can confirm
// handleWebhook delivers it verbatim as the real HTTP response.
func webhookReplyFlowDef(name, actionName string) flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name: name, DisplayName: "Webhook Reply", WebhookEnabled: true,
		Flow: model.FlowVersion{
			ID: "fv-webhook-reply",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "reply", DisplayName: "Reply", Type: model.ActionPiece,
					Piece: &model.PieceSettings{
						PieceName: "webhook_reply", ActionName: actionName,
						Input: map[string]any{
							"status":  201,
							"body":    map[string]any{"received": true},
							"headers": map[string]any{"X-Custom": "yes"},
						},
					},
				},
			},
		},
	}
}

// webhookFailFlowDef is WebhookEnabled and throws — no explicit
// Stop/Respond, so handleWebhook's fallback ack must reflect the failure.
func webhookFailFlowDef(name string) flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name: name, DisplayName: "Webhook Fail", WebhookEnabled: true,
		Flow: model.FlowVersion{
			ID: "fv-webhook-fail",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "boom", DisplayName: "Boom", Type: model.ActionCode,
					Code: &model.CodeSettings{Source: `(params) => { throw new Error("boom"); }`},
				},
			},
		},
	}
}

// notifyFlowDef is a valid, always-succeeding CODE flow used as the
// OnFailureFlow target in the tests below — its own success/failure isn't
// the point, just that it actually RAN and got recorded.
func notifyFlowDef(name string) flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name: name, DisplayName: "Notify",
		Flow: model.FlowVersion{
			ID: "fv-notify",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "ack", DisplayName: "Ack", Type: model.ActionCode,
					Code: &model.CodeSettings{Source: `(params) => ({ acked: true })`},
				},
			},
		},
	}
}

func TestWebhook_OnFailure_TriggersNamedFlow_RecordedInHistory(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", notifyFlowDef("notify"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save notify: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	failing := webhookFailFlowDef("failing-webhook")
	failing.OnFailureFlow = "notify"
	if rec := do(t, srv, "POST", "/flows", failing, true); rec.Code != http.StatusCreated {
		t.Fatalf("save failing: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	if rec := postWebhook(srv, "failing-webhook", "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("webhook: status = %d, want 500 (the flow itself failed); body=%s", rec.Code, rec.Body.String())
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	names := map[string]int{}
	for _, r := range runs {
		rm, _ := r.(map[string]any)
		names[rm["FlowName"].(string)]++
	}
	if names["failing-webhook"] != 1 || names["notify"] != 1 {
		t.Fatalf("recorded runs = %v, want exactly one for \"failing-webhook\" and one for \"notify\"", names)
	}
}

func TestFlowRun_OnFailureConfigured_TriggersNamedFlow_RecordedInHistory(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", notifyFlowDef("notify"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save notify: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	failing := webhookFailFlowDef("failing-named")
	failing.OnFailureFlow = "notify"
	if rec := do(t, srv, "POST", "/flows", failing, true); rec.Code != http.StatusCreated {
		t.Fatalf("save failing: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, "POST", "/flows/failing-named/run", nil, true); rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	names := map[string]int{}
	for _, r := range runs {
		rm, _ := r.(map[string]any)
		names[rm["FlowName"].(string)]++
	}
	if names["failing-named"] != 1 || names["notify"] != 1 {
		t.Fatalf("recorded runs = %v, want exactly one for \"failing-named\" and one for \"notify\"", names)
	}
}

func TestFlowRun_OnFailureNotConfigured_DoesNotTriggerAnything(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", notifyFlowDef("notify"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save notify: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	failing := webhookFailFlowDef("failing-no-hook") // WebhookEnabled true, OnFailureFlow left empty
	if rec := do(t, srv, "POST", "/flows", failing, true); rec.Code != http.StatusCreated {
		t.Fatalf("save failing: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, "POST", "/flows/failing-no-hook/run", nil, true); rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200 (ExecuteBegin never errors the HTTP call); body=%s", rec.Code, rec.Body.String())
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("recorded runs = %+v, want exactly one — \"notify\" must not have fired", runs)
	}
}

func postWebhook(srv *Server, name, body, secretHeader string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest("POST", "/webhooks/"+name, nil)
	} else {
		r = httptest.NewRequest("POST", "/webhooks/"+name, strings.NewReader(body))
	}
	if secretHeader != "" {
		r.Header.Set("X-Webhook-Secret", secretHeader)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func TestWebhook_UnknownFlow_404(t *testing.T) {
	srv := newTestServer(t)
	rec := postWebhook(srv, "never-saved", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWebhook_NotEnabled_404SameAsUnknown proves a saved-but-not-enabled
// flow gets the IDENTICAL response an unknown flow gets — an outside caller
// can't distinguish "doesn't exist" from "exists but not webhook-enabled".
func TestWebhook_NotEnabled_404SameAsUnknown(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("disabled-flow", false, ""), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := postWebhook(srv, "disabled-flow", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebhook_NoAuthRequired_PayloadBecomesTriggerAndRuns(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("echo", true, ""), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// Deliberately NO Authorization header — a third-party sender has no
	// way to know GOFLOW_API_TOKEN.
	r := httptest.NewRequest("POST", "/webhooks/echo", strings.NewReader(`{"n": 7}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["status"] != "ok" {
		t.Fatalf("body = %v, want the generic ack {status: ok} — not the full ExecutionState", m)
	}
}

func TestWebhook_MalformedJSONBody_400(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("echo", true, ""), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := postWebhook(srv, "echo", "{not valid json", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebhook_SecretConfigured_MissingOrWrongHeaderRejected(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.credStore.Save("wh-secret", "correct-horse-battery-staple"); err != nil {
		t.Fatalf("credStore.Save: %v", err)
	}
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("secured", true, "wh-secret"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	if rec := postWebhook(srv, "secured", `{"n":1}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no header: status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postWebhook(srv, "secured", `{"n":1}`, "wrong-secret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong header: status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebhook_SecretConfigured_CorrectHeaderSucceeds(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.credStore.Save("wh-secret", "correct-horse-battery-staple"); err != nil {
		t.Fatalf("credStore.Save: %v", err)
	}
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("secured", true, "wh-secret"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := postWebhook(srv, "secured", `{"n":1}`, "correct-horse-battery-staple")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWebhook_SecretCredentialUnresolvable_FailsClosed proves a
// WebhookSecretCredential naming a credential that was never actually
// saved denies every request — it must never silently behave as "no
// secret required".
func TestWebhook_SecretCredentialUnresolvable_FailsClosed(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("dangling", true, "never-saved-cred"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := postWebhook(srv, "dangling", `{"n":1}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWebhook_StopResponse_DeliveredVerbatim proves ctx.Run.Stop's
// status/body/headers become the REAL HTTP response — the first route in
// this project where that's actually delivered instead of just recorded in
// the ExecutionState.
func TestWebhook_StopResponse_DeliveredVerbatim(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookReplyFlowDef("stopper", "stop"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := postWebhook(srv, "stopper", "", "")
	if rec.Code != http.StatusCreated { // 201, as set by webhookReplyFlowDef's Input
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Custom"); got != "yes" {
		t.Fatalf("X-Custom header = %q, want %q", got, "yes")
	}
	m := decode(t, rec)
	if m["received"] != true {
		t.Fatalf("body = %v, want {received: true} verbatim, not the ExecutionState", m)
	}
}

func TestWebhook_RespondEarly_DeliveredVerbatim(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookReplyFlowDef("responder", "respond"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := postWebhook(srv, "responder", "", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["received"] != true {
		t.Fatalf("body = %v, want {received: true} verbatim", m)
	}
}

func TestWebhook_NoExplicitResponse_FailedFlowGetsGenericAck500(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookFailFlowDef("failer"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := postWebhook(srv, "failer", "", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["status"] != "failed" {
		t.Fatalf("body = %v, want the generic {status: failed} ack — not step-level error detail", m)
	}
}

// TestWebhook_RecordedInHistory proves a webhook-triggered run goes through
// flowstore.RunWithHistory exactly like every other transport — GET /runs
// sees it with the flow's name.
func TestWebhook_RecordedInHistory(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("echo", true, ""), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postWebhook(srv, "echo", `{"n":3}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("webhook: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	listRec := do(t, srv, "GET", "/runs", nil, true)
	m := decode(t, listRec)
	runs, _ := m["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want exactly 1 recorded run", m["runs"])
	}
	summary, _ := runs[0].(map[string]any)
	if summary["FlowName"] != "echo" {
		t.Fatalf("FlowName = %v, want %q", summary["FlowName"], "echo")
	}
}

// TestGetFlows_ListsWebhookEnabled proves GET /flows' metadata-only listing
// includes WebhookEnabled, so an agent can discover which flows accept
// POST /webhooks/{name} without fetching each one's full definition.
func TestGetFlows_ListsWebhookEnabled(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/flows", webhookEchoFlowDef("echo", true, ""), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, "POST", "/flows", validFlowDef("plain"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, srv, "GET", "/flows", nil, true)
	m := decode(t, rec)
	flows, _ := m["flows"].([]any)
	byName := map[string]map[string]any{}
	for _, f := range flows {
		fm, _ := f.(map[string]any)
		byName[fm["name"].(string)] = fm
	}
	if byName["echo"]["webhookEnabled"] != true {
		t.Fatalf("echo.webhookEnabled = %v, want true", byName["echo"]["webhookEnabled"])
	}
	if byName["plain"]["webhookEnabled"] != false {
		t.Fatalf("plain.webhookEnabled = %v, want false", byName["plain"]["webhookEnabled"])
	}
}

func validPieceDef(name string) catalog.Definition {
	return catalog.Definition{
		Name: name, DisplayName: "Valid Piece", Description: "a test piece",
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
}

func TestDeletePiece_NoAuth_401(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "DELETE", "/pieces/whatever", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestDeletePiece_ExistingThenGone(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, "POST", "/pieces", validPieceDef("killme"), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := do(t, srv, "DELETE", "/pieces/killme", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["deleted"] != true || m["name"] != "killme" {
		t.Fatalf("body = %v, want deleted=true name=killme", m)
	}
	// A subsequent GET /catalog no longer lists it.
	rec = do(t, srv, "GET", "/catalog", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "killme") {
		t.Fatalf("deleted piece still listed in catalog: %s", rec.Body.String())
	}
}

func TestDeletePiece_Missing_404(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv, "DELETE", "/pieces/never-existed", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
