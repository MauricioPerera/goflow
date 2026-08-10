// Package mcpapi exposes goflow's named flows as MCP (Model Context Protocol)
// tools over a hand-written JSON-RPC 2.0 transport — no MCP SDK, no streaming
// SSE, no sessions. One POST endpoint speaks just enough of the protocol for
// an LLM client to discover and run the flows persisted in a flowstore.Store:
// initialize, notifications/initialized, tools/list, and tools/call. The
// shared "validate then run" logic is flowstore.RunWithHistory, the same
// path POST /flows/run and POST /flows/{name}/run use, so a tool call and an
// HTTP run execute identically and are recorded identically.
//
// Everything here is encoding/json + net/http — JSON-RPC 2.0 is simple enough
// that pulling in a library would add a dependency for nothing, matching the
// rest of this project's stdlib-only stance.
package mcpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/runstore"
)

// Handler is an http.Handler that speaks MCP (JSON-RPC 2.0) over a single POST
// endpoint. FlowStore supplies the named flows surfaced as tools; BuildRegistry
// is the same fresh-per-call registry assembler the HTTP server uses, so a
// tool call validates and runs against the exact same piece registry as every
// other route. Both are injected so the handler owns no global state.
type Handler struct {
	FlowStore     flowstore.Store
	BuildRegistry func() (*piece.Registry, error)
	// CredStore resolves $credential markers in a flow's Input before it runs
	// and redacts the substituted values from the returned ExecutionState, so
	// a tool call that uses a stored credential never leaks the secret back in
	// the JSON-RPC result. May be nil only when no saved flow references a
	// credential.
	CredStore credentials.Store
	// HistoryStore records every tools/call run — the same run-history
	// mechanism POST /flows/run and POST /flows/{name}/run use, via
	// flowstore.RunWithHistory, so a tool call recorded in GET /runs looks
	// identical to an HTTP-triggered run. May be nil (recording disabled),
	// matching CredStore's nil-means-off convention.
	HistoryStore runstore.Store
}

// NewHandler returns a Handler wired to flowStore, buildRegistry, credStore,
// and historyStore. It is the caller's job to gate it with auth
// (httpapi.Server mounts it behind its bearer-token middleware, same as
// every other route); this package does not know about auth on purpose —
// pkg/httpapi owns that concern.
func NewHandler(flowStore flowstore.Store, buildRegistry func() (*piece.Registry, error), credStore credentials.Store, historyStore runstore.Store) *Handler {
	return &Handler{FlowStore: flowStore, BuildRegistry: buildRegistry, CredStore: credStore, HistoryStore: historyStore}
}

// nullID is the JSON-RPC id used when no id could be read from the request
// (parse error, batch, non-object) — the spec says the error response's id is
// null in that case. Kept as a json.RawMessage so it marshals verbatim.
var nullID = json.RawMessage("null")

// rawRequest is the first typed decode of a request body. ID is a pointer to
// json.RawMessage so PRESENCE of the "id" key (not its value) distinguishes a
// request from a notification: a pointer is nil only when the key is absent,
// never when the value is null — exactly the distinction the spec asks for
// ("mirá si la clave id existe en el JSON crudo, no solo castear a un
// puntero"). Keeping ID as raw bytes also preserves whether the client sent a
// number or a string, echoed back untouched in the response.
type rawRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params"`
	ID      *json.RawMessage `json:"id"`
}

// ServeHTTP implements http.Handler. It reads one JSON-RPC object from the
// body, classifies it (parse error / batch / non-object / notification /
// request), dispatches the four supported methods, and writes the JSON-RPC
// response — or a bare 202 for a notification. A panic in a method handler
// is left for an outer recover() middleware (pkg/httpapi's, when mounted
// there) to turn into a 500; this handler does not add its own recover, but
// also does not rely on one being absent.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, nil, -32700, fmt.Sprintf("parse error: %v", err))
		return
	}
	_ = r.Body.Close()

	// First pass into any: separates "not valid JSON" (-32700) from "valid
	// JSON but not a request object" (-32600 for arrays/batch, -32600 for
	// scalars). Only after this do we decode into rawRequest, which needs a
	// JSON object to populate its fields.
	var first any
	if err := json.Unmarshal(body, &first); err != nil {
		writeError(w, nil, -32700, fmt.Sprintf("parse error: %v", err))
		return
	}
	if _, isArr := first.([]any); isArr {
		writeError(w, nil, -32600, "batch requests are not supported")
		return
	}
	if _, isObj := first.(map[string]any); !isObj {
		writeError(w, nil, -32600, "invalid request: expected a JSON object")
		return
	}

	var req rawRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, nil, -32700, fmt.Sprintf("parse error: %v", err))
		return
	}

	// A notification has no "id" key present. Notifications never get a
	// JSON-RPC response — only HTTP 202 with an empty body — so this is
	// checked before method/jsonrpc validation: even a malformed
	// notification stays a notification.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// It's a request (id present). Validate the JSON-RPC envelope before
	// dispatching: missing jsonrpc or method is -32600, with the id echoed.
	if req.JSONRPC == "" || req.Method == "" {
		writeError(w, req.ID, -32600, "invalid request: missing jsonrpc or method")
		return
	}

	switch req.Method {
	case "initialize":
		h.handleInitialize(w, req)
	case "tools/list":
		h.handleToolsList(w, req)
	case "tools/call":
		h.handleToolsCall(w, req)
	default:
		writeError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// handleInitialize echoes the client's protocolVersion (falling back to the
// literal "2026-06-18" when it's absent or not a string), advertises only the
// tools capability, and identifies the server. Capabilities/clientInfo are
// accepted but not validated in depth — this is a single-transport server
// with no feature negotiation to enforce.
func (h *Handler) handleInitialize(w http.ResponseWriter, req rawRequest) {
	var params struct {
		ProtocolVersion any `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		// Ignore the error: a non-string protocolVersion (or any shape we
		// don't recognize) falls through to the default below, which is
		// exactly the "si no vino o no es string" case the spec defines.
		_ = json.Unmarshal(req.Params, &params)
	}
	pv := "2026-06-18"
	if s, ok := params.ProtocolVersion.(string); ok && s != "" {
		pv = s
	}
	writeResult(w, req.ID, map[string]any{
		"protocolVersion": pv,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "goflow-mcp",
			"version": "0.1.0",
		},
	})
}

// handleToolsList surfaces every saved flow as one tool. The description falls
// back through Description -> DisplayName -> Name so a caller always sees
// something useful. inputSchema is deliberately permissive (an object that
// accepts any properties): a flow's trigger payload is free-text-described
// (FlowDefinition.InputSchema), not a formal JSON Schema, so the description
// carries the real contract and additionalProperties:true lets any arguments
// through to flowstore.Run.
func (h *Handler) handleToolsList(w http.ResponseWriter, req rawRequest) {
	defs, err := h.FlowStore.List()
	if err != nil {
		writeError(w, req.ID, -32603, "internal error: "+err.Error())
		return
	}
	tools := make([]map[string]any, 0, len(defs))
	for _, def := range defs {
		desc := def.Description
		if desc == "" {
			desc = def.DisplayName
		}
		if desc == "" {
			desc = def.Name
		}
		tools = append(tools, map[string]any{
			"name":        def.Name,
			"description": desc,
			"inputSchema": map[string]any{
				"type":                 "object",
				"description":          def.InputSchema,
				"additionalProperties": true,
			},
		})
	}
	writeResult(w, req.ID, map[string]any{"tools": tools})
}

// handleToolsCall runs a saved flow by name. arguments is the trigger payload;
// executeTrigger is always false (the same default the rest of the API uses).
// The three Run outcomes map to three distinct wire shapes:
//   - runErr != nil (buildRegistry failed): a JSON-RPC -32603 internal error —
//     a server fault, not a tool failure.
//   - validationErrs non-empty: a tool RESULT with isError:true — the tool
//     exists but its definition is broken; that's a tool-level outcome, not a
//     protocol error.
//   - a state: a tool RESULT whose content is the full ExecutionState as
//     indented JSON, with isError set by whether the run succeeded.
//
// An unknown name is a -32602 protocol error (the client asked for a tool
// that doesn't exist), distinct from a known-but-broken tool above.
func (h *Handler) handleToolsCall(w http.ResponseWriter, req rawRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeError(w, req.ID, -32602, "invalid params: "+err.Error())
			return
		}
	}
	if params.Name == "" {
		writeError(w, req.ID, -32602, "invalid params: name is required")
		return
	}

	def, ok, err := h.FlowStore.Get(params.Name)
	if err != nil {
		writeError(w, req.ID, -32603, "internal error: "+err.Error())
		return
	}
	if !ok {
		writeError(w, req.ID, -32602, "unknown tool: "+params.Name)
		return
	}

	state, validationErrs, runErr := flowstore.RunWithHistory(&def.Flow, h.BuildRegistry, h.CredStore, h.HistoryStore, def.Name, params.Arguments, false)
	if runErr != nil {
		writeError(w, req.ID, -32603, "internal error: "+runErr.Error())
		return
	}
	if len(validationErrs) > 0 {
		writeResult(w, req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": flowstore.FormatValidationErrors(validationErrs),
			}},
			"isError": true,
		})
		return
	}

	stateJSON, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		writeError(w, req.ID, -32603, "internal error: "+err.Error())
		return
	}
	writeResult(w, req.ID, map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(stateJSON),
		}},
		"isError": state.Verdict.Status != model.FlowRunSucceeded,
	})
}

// writeResult sends a JSON-RPC success response: {"jsonrpc":"2.0","id":<id>,
// "result":<result>}. id is the request's raw id, echoed verbatim so a number
// stays a number and a string stays a string.
func writeResult(w http.ResponseWriter, id *json.RawMessage, result any) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      idOrZero(id),
		"result":  result,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeError sends a JSON-RPC error response: {"jsonrpc":"2.0","id":<id or
// null>,"error":{"code":<code>,"message":<msg>}}. JSON-RPC errors are
// payload-level, so the HTTP status is 200 (the only non-200 case this handler
// emits is the 202 for notifications).
func writeError(w http.ResponseWriter, id *json.RawMessage, code int, message string) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      idOrZero(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// idOrZero returns id unchanged when the request carried one, or the null id
// when it didn't (parse error / batch / non-object, where the id couldn't be
// read at all). A non-nil *json.RawMessage marshals to its raw bytes —
// preserving number-vs-string — so callers pass the request's id straight
// through.
func idOrZero(id *json.RawMessage) *json.RawMessage {
	if id == nil {
		return &nullID
	}
	return id
}
