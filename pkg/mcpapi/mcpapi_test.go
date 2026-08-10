package mcpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
	"goflow/pkg/piece"
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
	return NewHandler(fs, emptyRegistryBuilder, credStore)
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
	return NewHandler(fs, emptyRegistryBuilder, credStore), credStore
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
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools))
	}
	for _, ti := range tools {
		tl, _ := ti.(map[string]any)
		if tl["name"] == nil {
			t.Fatalf("tool missing name: %v", tl)
		}
		if tl["description"] == nil {
			t.Fatalf("tool %v missing description", tl["name"])
		}
		schema, _ := tl["inputSchema"].(map[string]any)
		if schema == nil || schema["type"] != "object" {
			t.Fatalf("tool %v inputSchema.type = %v, want object", tl["name"], schema)
		}
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
