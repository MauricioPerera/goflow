package mcpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
)

// testCredKey is a fixed 32-byte AES-256 key for the credentials store in
// tests — constant so the on-disk shape is deterministic.
var testCredKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

// emptyRegistryBuilder is a BuildRegistry that returns an empty registry —
// enough for the CODE-only flows these tests use, which validate and run
// without any registered piece. Mirrors what a real server does for a flow
// that touches no pieces, without dragging in a catalog store.
func emptyRegistryBuilder() (*piece.Registry, error) {
	return piece.NewRegistry(), nil
}

// newHandlerWithFlows builds a Handler backed by a real flowstore.FileStore in
// a temp dir, saves the given flows directly (no gate needed: FileStore.Save
// does not validate, and these flows are valid anyway), and returns the
// handler plus the store dir. A real credentials.FileStore is wired too; nil
// would be valid only if no flow references a credential, but a real store is
// harmless and keeps the handler exercised the way production wires it.
func newHandlerWithFlows(t *testing.T, defs ...flowstore.FlowDefinition) *Handler {
	t.Helper()
	fs, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	credStore, err := credentials.NewFileStore(t.TempDir(), testCredKey)
	if err != nil {
		t.Fatalf("credentials.NewFileStore: %v", err)
	}
	for _, def := range defs {
		if err := fs.Save(def); err != nil {
			t.Fatalf("Save %q: %v", def.Name, err)
		}
	}
	return NewHandler(fs, emptyRegistryBuilder, credStore, runstore.NewMemoryStore(), catalog.NewMemoryStore())
}

// newHandlerWithFlowsAndCreds is newHandlerWithFlows but also returns the
// credentials store so a test can seed a real credential for a $credential
// end-to-end run.
func newHandlerWithFlowsAndCreds(t *testing.T, defs ...flowstore.FlowDefinition) (*Handler, *credentials.FileStore) {
	t.Helper()
	fs, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	credStore, err := credentials.NewFileStore(t.TempDir(), testCredKey)
	if err != nil {
		t.Fatalf("credentials.NewFileStore: %v", err)
	}
	for _, def := range defs {
		if err := fs.Save(def); err != nil {
			t.Fatalf("Save %q: %v", def.Name, err)
		}
	}
	return NewHandler(fs, emptyRegistryBuilder, credStore, runstore.NewMemoryStore(), catalog.NewMemoryStore()), credStore
}

// newHandlerWithFlowsAndHistory is newHandlerWithFlows but also returns the
// runstore.Store, so a test can confirm a tools/call was actually recorded.
func newHandlerWithFlowsAndHistory(t *testing.T, defs ...flowstore.FlowDefinition) (*Handler, runstore.Store) {
	t.Helper()
	fs, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	credStore, err := credentials.NewFileStore(t.TempDir(), testCredKey)
	if err != nil {
		t.Fatalf("credentials.NewFileStore: %v", err)
	}
	for _, def := range defs {
		if err := fs.Save(def); err != nil {
			t.Fatalf("Save %q: %v", def.Name, err)
		}
	}
	historyStore := runstore.NewMemoryStore()
	return NewHandler(fs, emptyRegistryBuilder, credStore, historyStore, catalog.NewMemoryStore()), historyStore
}

// newHandlerWithFlowsAndCatalog is newHandlerWithFlows but also returns the
// catalog.Store, so a test can seed a JS-authored piece and confirm
// goflow_describe_catalog actually surfaces it.
func newHandlerWithFlowsAndCatalog(t *testing.T, defs ...flowstore.FlowDefinition) (*Handler, catalog.Store) {
	t.Helper()
	fs, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	credStore, err := credentials.NewFileStore(t.TempDir(), testCredKey)
	if err != nil {
		t.Fatalf("credentials.NewFileStore: %v", err)
	}
	for _, def := range defs {
		if err := fs.Save(def); err != nil {
			t.Fatalf("Save %q: %v", def.Name, err)
		}
	}
	catalogStore := catalog.NewMemoryStore()
	return NewHandler(fs, emptyRegistryBuilder, credStore, runstore.NewMemoryStore(), catalogStore), catalogStore
}

// doublesArgFlow is a "double it" flow that reads n from the TRIGGER payload
// (not a hardcoded step input), so calling it with arguments {"n": 7} yields
// doubled: 14. The CODE step's input references the trigger step's output via
// {{ trigger_1.output.n }} — the same {{ stepName.output.field }} convention
// the engine's templating uses (see pkg/expr). This is the EMPTY-trigger ->
// CODE shape the /flows/run tests use, just wired to read the caller's input.
func doublesArgFlow() flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name:        "double-it",
		DisplayName: "Double It",
		Description: "doubles n from the trigger payload",
		InputSchema: "n (number, required) — doubled by the step",
		Flow: model.FlowVersion{
			ID: "fv-mcp",
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

// throwsFlow is a valid (compilable) CODE flow whose step throws at run time —
// a tool that exists and validates but fails when called. The MCP handler must
// surface this as a tool RESULT with isError:true, not a JSON-RPC protocol
// error.
func throwsFlow() flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name:        "throws",
		DisplayName: "Throws",
		Description: "always throws",
		Flow: model.FlowVersion{
			ID: "fv-throw",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "boom", DisplayName: "Boom", Type: model.ActionCode,
					Code: &model.CodeSettings{
						Source: `(params) => { throw new Error('boom') }`,
					},
				},
			},
		},
	}
}

// call sends a JSON-RPC body to the handler and returns the recorder. The body
// is built from a map so the tests can omit "id" entirely (to send a
// notification) or set it to any JSON type.
func call(t *testing.T, h *Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// callRaw sends an already-serialized body (used for invalid-JSON and batch
// cases where the body isn't a Go value we'd marshal).
func callRaw(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// decodeResponse unmarshals a JSON-RPC response into a map. Fails the test if
// the body isn't valid JSON.
func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestInitialize_EchoesProtocolVersionAndServerInfo(t *testing.T) {
	h := newHandlerWithFlows(t)
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2030-01-01"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResponse(t, rec)
	if m["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want 2.0", m["jsonrpc"])
	}
	if m["id"] != float64(1) {
		t.Fatalf("id = %v, want 1 (echoed)", m["id"])
	}
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want an object", m["result"])
	}
	if result["protocolVersion"] != "2030-01-01" {
		t.Fatalf("protocolVersion = %v, want 2030-01-01 (echoed)", result["protocolVersion"])
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities = %v, want an object", result["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities.tools missing: %v", caps)
	}
	info, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo = %v, want an object", result["serverInfo"])
	}
	if info["name"] != "goflow-mcp" {
		t.Fatalf("serverInfo.name = %v, want goflow-mcp", info["name"])
	}
}

func TestInitialize_FallsBackWhenProtocolVersionMissing(t *testing.T) {
	h := newHandlerWithFlows(t)
	// No protocolVersion in params -> the literal fallback applies.
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": "abc", "method": "initialize", "params": map[string]any{},
	})
	m := decodeResponse(t, rec)
	result, _ := m["result"].(map[string]any)
	if result["protocolVersion"] != "2026-06-18" {
		t.Fatalf("protocolVersion = %v, want fallback 2026-06-18", result["protocolVersion"])
	}
	if m["id"] != "abc" {
		t.Fatalf("id = %v, want \"abc\" (string echoed)", m["id"])
	}
}

func TestNotificationsInitialized_202EmptyBody(t *testing.T) {
	h := newHandlerWithFlows(t)
	// No "id" key at all -> this is a notification -> 202, empty body.
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty for a notification", rec.Body.String())
	}
}

func TestToolsList_TwoFlowsWithSchema(t *testing.T) {
	h := newHandlerWithFlows(t, doublesArgFlow(), throwsFlow())
	rec := call(t, h, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	result, _ := decodeResponse(t, rec)["result"].(map[string]any)
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("result.tools = %v, want an array", result["tools"])
	}
	// len(reservedToolNames) fixed meta tools (goflow_describe_catalog etc. —
	// always present, see handleToolsList) plus one per saved flow.
	if want := len(reservedToolNames) + 2; len(tools) != want {
		t.Fatalf("len(tools) = %d, want %d (meta tools + 2 flows)", len(tools), want)
	}
	names := map[string]bool{}
	for _, ti := range tools {
		tl, _ := ti.(map[string]any)
		name, _ := tl["name"].(string)
		if name == "" {
			t.Fatalf("tool missing name: %v", tl)
		}
		names[name] = true
		if tl["description"] == nil {
			t.Fatalf("tool %v missing description", tl["name"])
		}
		schema, _ := tl["inputSchema"].(map[string]any)
		if schema == nil || schema["type"] != "object" {
			t.Fatalf("tool %v inputSchema.type = %v, want object", tl["name"], schema)
		}
	}
	for reserved := range reservedToolNames {
		if !names[reserved] {
			t.Fatalf("tools/list is missing the fixed meta tool %q", reserved)
		}
	}
	if !names["double-it"] || !names["throws"] {
		t.Fatalf("tools/list is missing a per-flow tool: %v", names)
	}
}

func TestToolsCall_DoubleIt_Doubled14NotError(t *testing.T) {
	h := newHandlerWithFlows(t, doublesArgFlow())
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "double-it", "arguments": map[string]any{"n": 7}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	result, _ := decodeResponse(t, rec)["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError = %v, want false for a successful run", result["isError"])
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v, want a one-element array", result["content"])
	}
	item, _ := content[0].(map[string]any)
	if item["type"] != "text" {
		t.Fatalf("content[0].type = %v, want text", item["type"])
	}
	text, _ := item["text"].(string)
	// The text is the full ExecutionState as indented JSON; it must parse and
	// carry the doubled value the CODE step produced.
	var state map[string]any
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		t.Fatalf("content text is not JSON: %v; text=%s", err, text)
	}
	steps, _ := state["Steps"].(map[string]any)
	double, _ := steps["double"].(map[string]any)
	output, _ := double["Output"].(map[string]any)
	doubled, _ := output["doubled"].(float64)
	if doubled != 14 {
		t.Fatalf("doubled = %v, want 14; output=%#v", output["doubled"], output)
	}
}

func TestToolsCall_UnknownName_Error32602(t *testing.T) {
	h := newHandlerWithFlows(t, doublesArgFlow())
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "no-such-flow"},
	})
	m := decodeResponse(t, rec)
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("response = %v, want an error object", m)
	}
	if code, _ := errObj["code"].(float64); code != -32602 {
		t.Fatalf("error.code = %v, want -32602", errObj["code"])
	}
	if m["id"] != float64(2) {
		t.Fatalf("id = %v, want 2 (echoed even on error)", m["id"])
	}
}

func TestToolsCall_MissingName_Error32602(t *testing.T) {
	h := newHandlerWithFlows(t, doublesArgFlow())
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{},
	})
	errObj, _ := decodeResponse(t, rec)["error"].(map[string]any)
	if code, _ := errObj["code"].(float64); code != -32602 {
		t.Fatalf("error.code = %v, want -32602 (name required)", errObj["code"])
	}
}

func TestToolsCall_Throws_IsErrorTrueInResult(t *testing.T) {
	h := newHandlerWithFlows(t, throwsFlow())
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "throws"},
	})
	m := decodeResponse(t, rec)
	// A run-time failure is a tool RESULT with isError:true, NOT a JSON-RPC
	// error object — the tool exists, it just failed.
	if _, isErr := m["error"]; isErr {
		t.Fatalf("response is a protocol error %v, want a tool result", m)
	}
	result, _ := m["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true for a thrown step", result["isError"])
	}
	content, _ := result["content"].([]any)
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	// The state JSON records the failure; the error message must be in there.
	if !strings.Contains(text, "boom") {
		t.Fatalf("content text does not describe the failure (want 'boom'): %s", text)
	}
}

func TestInvalidJSON_Error32700NullID(t *testing.T) {
	h := newHandlerWithFlows(t)
	rec := callRaw(t, h, "this is not json at all")
	m := decodeResponse(t, rec)
	errObj, _ := m["error"].(map[string]any)
	if code, _ := errObj["code"].(float64); code != -32700 {
		t.Fatalf("error.code = %v, want -32700 (parse error)", errObj["code"])
	}
	if m["id"] != nil {
		t.Fatalf("id = %v, want null (id could not be read)", m["id"])
	}
}

func TestBatchArray_Error32600(t *testing.T) {
	h := newHandlerWithFlows(t)
	rec := callRaw(t, h, `[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`)
	m := decodeResponse(t, rec)
	errObj, _ := m["error"].(map[string]any)
	if code, _ := errObj["code"].(float64); code != -32600 {
		t.Fatalf("error.code = %v, want -32600 (batch not supported)", errObj["code"])
	}
	if m["id"] != nil {
		t.Fatalf("id = %v, want null for a batch", m["id"])
	}
}

func TestUnknownMethod_Error32601(t *testing.T) {
	h := newHandlerWithFlows(t)
	rec := call(t, h, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "foo/bar"})
	errObj, _ := decodeResponse(t, rec)["error"].(map[string]any)
	if code, _ := errObj["code"].(float64); code != -32601 {
		t.Fatalf("error.code = %v, want -32601 (method not found)", errObj["code"])
	}
	if !strings.Contains(errObj["message"].(string), "foo/bar") {
		t.Fatalf("error.message = %v, want it to mention foo/bar", errObj["message"])
	}
}

// usesCredentialFlow is a CODE-only flow whose step input references a stored
// credential by name via the $credential marker. The CODE step returns the
// resolved auth's length, so a successful run proves the real secret reached
// the piece — without ever putting the secret itself in the output.
func usesCredentialFlow() flowstore.FlowDefinition {
	return flowstore.FlowDefinition{
		Name:        "uses-cred",
		DisplayName: "Uses Credential",
		Description: "uses a $credential marker",
		Flow: model.FlowVersion{
			ID: "fv-cred",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "use_auth", DisplayName: "Use Cred", Type: model.ActionCode,
					Code: &model.CodeSettings{
						Input:  map[string]any{"auth": map[string]any{"$credential": "relay"}},
						Source: `(params) => ({ authLen: params.auth.length })`,
					},
				},
			},
		},
	}
}

// TestToolsCall_CredentialMarker_SecretNotInBody_RealValueUsed is the
// end-to-end MCP proof: a real credential is stored, a flow references it by
// name, a tools/call runs that flow over the real Handler (no direct
// flowstore call), and the raw HTTP response body must NOT contain the
// secret anywhere — while the step's output authLen == len(secret) proves the
// real value did reach the piece. This is the test that proves the redaction
// works over the wire, not just at the library level.
func TestToolsCall_CredentialMarker_SecretNotInBody_RealValueUsed(t *testing.T) {
	h, credStore := newHandlerWithFlowsAndCreds(t, usesCredentialFlow())
	const secret = "el-secreto-xyz-789"
	if err := credStore.Save("relay", secret); err != nil {
		t.Fatalf("Save credential: %v", err)
	}

	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 11, "method": "tools/call",
		"params": map[string]any{"name": "uses-cred", "arguments": map[string]any{}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Read the RAW HTTP body — the exact bytes a client would receive — and
	// assert the secret is nowhere in it.
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("real secret leaked into the tools/call response body:\n%s", body)
	}

	// The run succeeded and the placeholder is present (so redaction is
	// observable). JSON escapes "<"/">", so check the decoded value.
	m := decodeResponse(t, rec)
	result, _ := m["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError = %v, want false for a successful run; body=%s", result["isError"], body)
	}
	content, _ := result["content"].([]any)
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	var state map[string]any
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		t.Fatalf("content text is not JSON: %v; text=%s", err, text)
	}
	steps, _ := state["Steps"].(map[string]any)
	useAuth, _ := steps["use_auth"].(map[string]any)
	input, _ := useAuth["Input"].(map[string]any)
	if input["auth"] != "<credential:relay>" {
		t.Fatalf("Input[auth] = %v, want placeholder <credential:relay>", input["auth"])
	}
	// authLen == len(secret) proves the REAL secret reached the piece (the
	// output is computed from it); only the Input was redacted. After a JSON
	// roundtrip the number is a float64.
	output, _ := useAuth["Output"].(map[string]any)
	authLen, _ := output["authLen"].(float64)
	if int(authLen) != len(secret) {
		t.Fatalf("authLen = %v, want %d (proves the real secret reached the piece)", authLen, len(secret))
	}
}

// TestToolsCall_MissingCredential_IsErrorTrueMentionsName: a flow that
// references a credential that isn't stored comes back as a tool RESULT with
// isError:true (a configuration problem with this tool, not a protocol
// error), and the message names the missing credential.
func TestToolsCall_MissingCredential_IsErrorTrueMentionsName(t *testing.T) {
	h := newHandlerWithFlows(t, usesCredentialFlow()) // "relay" never saved
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 12, "method": "tools/call",
		"params": map[string]any{"name": "uses-cred", "arguments": map[string]any{}},
	})
	m := decodeResponse(t, rec)
	if _, isErr := m["error"]; isErr {
		t.Fatalf("response is a protocol error %v, want a tool result", m)
	}
	result, _ := m["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true for a missing credential", result["isError"])
	}
	content, _ := result["content"].([]any)
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	if !strings.Contains(text, "relay") {
		t.Fatalf("error text does not name the missing credential: %s", text)
	}
}

// TestToolsCall_RecordedInHistory proves a tools/call run goes through
// flowstore.RunWithHistory exactly like an HTTP-triggered run does —
// recorded with the tool's name as FlowName, discoverable via the same
// runstore.Store an HTTP GET /runs would read from.
func TestToolsCall_RecordedInHistory(t *testing.T) {
	h, hist := newHandlerWithFlowsAndHistory(t, doublesArgFlow())
	rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "double-it", "arguments": map[string]any{"n": 7}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	summaries, err := hist.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("List() = %+v, want exactly 1 recorded run", summaries)
	}
	if summaries[0].FlowName != "double-it" {
		t.Fatalf("recorded FlowName = %q, want %q", summaries[0].FlowName, "double-it")
	}
	if summaries[0].Status != model.FlowRunSucceeded {
		t.Fatalf("recorded Status = %v, want SUCCEEDED", summaries[0].Status)
	}
}

// --- read-only meta tools -----------------------------------------------

func callTool(t *testing.T, h *Handler, name string, args map[string]any) map[string]any {
	t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	rec := call(t, h, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call %s: status = %d, want 200; body=%s", name, rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if errObj, isErr := resp["error"]; isErr {
		t.Fatalf("tools/call %s returned a JSON-RPC protocol error, want a tool result: %v", name, errObj)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s: result = %v, want an object", name, resp["result"])
	}
	return result
}

// toolText extracts the single text content item every meta tool's result
// carries and decodes it as JSON into v.
func toolText(t *testing.T, result map[string]any, v any) {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v, want a one-element array", result["content"])
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	if text == "" {
		t.Fatalf("content[0].text is empty: %v", item)
	}
	if v == nil {
		return
	}
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("content text is not JSON: %v; text=%s", err, text)
	}
}

func TestDescribeCatalog_SurfacesBuiltinAndJSPieces(t *testing.T) {
	h, catStore := newHandlerWithFlowsAndCatalog(t)
	if err := catStore.Save(catalog.Definition{
		Name: "risk_score", DisplayName: "Risk Score", Description: "classifies risk",
		Actions: []catalog.ActionDefinition{{
			Name: "run", DisplayName: "Run", Description: "runs it",
			InputSchema: "x (number, required)",
			Source:      `(ctx) => ({ doubled: Number(ctx.input.x) * 2 })`,
		}},
	}); err != nil {
		t.Fatalf("catStore.Save: %v", err)
	}

	result := callTool(t, h, toolDescribeCatalog, nil)
	if result["isError"] != false {
		t.Fatalf("isError = %v, want false", result["isError"])
	}
	content, _ := result["content"].([]any)
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	if !strings.Contains(text, "risk_score") {
		t.Fatalf("describe_catalog text missing the JS-authored piece: %s", text)
	}
	if !strings.Contains(text, "http") { // a built-in Go piece
		t.Fatalf("describe_catalog text missing a built-in Go piece: %s", text)
	}
}

func TestListFlows_ReturnsMetadataForEveryFlowExceptReservedNames(t *testing.T) {
	h := newHandlerWithFlows(t, doublesArgFlow(), throwsFlow())
	result := callTool(t, h, toolListFlows, nil)
	var body struct {
		Flows []struct {
			Name           string `json:"name"`
			DisplayName    string `json:"displayName"`
			WebhookEnabled bool   `json:"webhookEnabled"`
		} `json:"flows"`
	}
	toolText(t, result, &body)
	if len(body.Flows) != 2 {
		t.Fatalf("flows = %+v, want exactly 2", body.Flows)
	}
	byName := map[string]bool{}
	for _, f := range body.Flows {
		byName[f.Name] = true
	}
	if !byName["double-it"] || !byName["throws"] {
		t.Fatalf("flows = %+v, missing an expected flow", body.Flows)
	}
}

func TestGetFlow_ReturnsFullDefinition(t *testing.T) {
	h := newHandlerWithFlows(t, doublesArgFlow())
	result := callTool(t, h, toolGetFlow, map[string]any{"name": "double-it"})
	if result["isError"] != false {
		t.Fatalf("isError = %v, want false", result["isError"])
	}
	var def flowstore.FlowDefinition
	toolText(t, result, &def)
	if def.Name != "double-it" || def.Flow.Trigger == nil {
		t.Fatalf("get_flow result = %+v, want the full FlowDefinition with its Flow", def)
	}
}

func TestGetFlow_UnknownName_IsErrorTrueNotProtocolError(t *testing.T) {
	h := newHandlerWithFlows(t)
	result := callTool(t, h, toolGetFlow, map[string]any{"name": "never-saved"})
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true for an unknown flow name", result["isError"])
	}
}

func TestGetFlow_MissingNameArgument_IsErrorTrue(t *testing.T) {
	h := newHandlerWithFlows(t)
	result := callTool(t, h, toolGetFlow, map[string]any{})
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true when the name argument is missing", result["isError"])
	}
}

func TestListRunsAndGetRun_RoundTripThroughToolsCall(t *testing.T) {
	h, _ := newHandlerWithFlowsAndHistory(t, doublesArgFlow())
	if rec := call(t, h, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "double-it", "arguments": map[string]any{"n": 7}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("seeding run: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	listResult := callTool(t, h, toolListRuns, nil)
	var listBody struct {
		Runs []struct {
			ID       string `json:"ID"`
			FlowName string `json:"FlowName"`
			Status   string `json:"Status"`
		} `json:"runs"`
	}
	toolText(t, listResult, &listBody)
	if len(listBody.Runs) != 1 || listBody.Runs[0].FlowName != "double-it" {
		t.Fatalf("list_runs = %+v, want exactly 1 run for double-it", listBody.Runs)
	}

	getResult := callTool(t, h, toolGetRun, map[string]any{"id": listBody.Runs[0].ID})
	if getResult["isError"] != false {
		t.Fatalf("get_run isError = %v, want false", getResult["isError"])
	}
	var rec runstore.Record
	toolText(t, getResult, &rec)
	if rec.FlowName != "double-it" || rec.State == nil {
		t.Fatalf("get_run result = %+v, want the full Record with its State", rec)
	}
}

func TestGetRun_UnknownID_IsErrorTrue(t *testing.T) {
	h, _ := newHandlerWithFlowsAndHistory(t)
	result := callTool(t, h, toolGetRun, map[string]any{"id": "never-existed"})
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true for an unknown run id", result["isError"])
	}
}

func TestListRuns_HistoryStoreNil_IsErrorTrueNotPanic(t *testing.T) {
	// Built directly (not via newHandlerWithFlows, which always wires a real
	// MemoryStore) specifically to leave HistoryStore nil — the
	// nil-means-off case callListRuns/callGetRun must handle without a panic.
	fs, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	h := &Handler{
		FlowStore:     fs,
		BuildRegistry: emptyRegistryBuilder,
		CatalogStore:  catalog.NewMemoryStore(),
	}
	result := callTool(t, h, toolListRuns, nil)
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true when HistoryStore is nil", result["isError"])
	}
}

func TestReservedFlowName_ExcludedFromListButStillResolvesToMetaTool(t *testing.T) {
	// A flow that happens to be named exactly like a reserved meta tool —
	// still saved and runnable by name over HTTP, but tools/list must not
	// advertise it as a distinct tool, and tools/call on that name must still
	// resolve to the FIXED tool, not the flow.
	shadowFlow := doublesArgFlow()
	shadowFlow.Name = toolListFlows
	h := newHandlerWithFlows(t, shadowFlow, doublesArgFlow())

	rec := call(t, h, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	result, _ := decodeResponse(t, rec)["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	count := 0
	for _, ti := range tools {
		tl, _ := ti.(map[string]any)
		if tl["name"] == toolListFlows {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("tools/list has %d entries named %q, want exactly 1 (the fixed meta tool, not the shadowed flow)", count, toolListFlows)
	}

	// tools/call on that name must run the META TOOL (a list-flows result),
	// not the shadowed flow (which would return an ExecutionState instead).
	callResult := callTool(t, h, toolListFlows, nil)
	var body struct {
		Flows []map[string]any `json:"flows"`
	}
	toolText(t, callResult, &body)
	if body.Flows == nil {
		t.Fatalf("tools/call %q returned something other than a list_flows result: %+v", toolListFlows, callResult)
	}
}
