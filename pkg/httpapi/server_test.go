package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
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
	return NewServer(&catalog.GatedStore{Underlying: fs}, credStore, flowStore, "secret-token")
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
