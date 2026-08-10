package pieces_test

// TestAIFirst_AllFourPhasesTogether is the capstone proof for the whole
// "AI-first" direction: every phase built for it, combined in one flow,
// none of them hand-waved.
//
// Phase 2 (pkg/jspiece): an agent authors a new piece as JS source — no
// Go equivalent exists for it.
// Phase 3 (pkg/catalog): that piece is saved through a GatedStore, which
// actually runs its Example before allowing the save, onto a FileStore —
// real disk persistence, not just in-process memory.
// Phase 1 (model.ParseFlowVersion): a flow chaining that persisted piece
// with three real Go catalog pieces (http, json, text, storage) exists
// ONLY as a JSON string below — never built as a Go struct literal.
// Phase 4 (pkg/flowvalidate): the parsed flow is validated structurally
// against the full registry BEFORE it's ever executed.
//
// And the registry itself is loaded from a completely separate
// catalog.FileStore instance pointed at the same directory — simulating a
// later process/session that has none of this test's in-memory state,
// only what an earlier session persisted to disk.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/engine"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

func TestAIFirst_AllFourPhasesTogether(t *testing.T) {
	// --- An earlier session: author a JS piece, gate it, persist it. ---
	dir := t.TempDir()
	authoringStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gated := &catalog.GatedStore{Underlying: authoringStore}

	def := catalog.Definition{
		Name: "order_utils", DisplayName: "Order Utils",
		Description: "formats a human-readable order summary — no Go catalog equivalent",
		Actions: []catalog.ActionDefinition{
			{
				Name: "summarize", DisplayName: "Summarize",
				Description: "returns \"Order <id>: $<amount>\" for an order",
				InputSchema: "orderId (string, required), amount (number, required)",
				Source: `(ctx) => ({
					summary: "Order " + ctx.input.orderId + ": $" + Number(ctx.input.amount).toFixed(2)
				})`,
				Examples: []catalog.Example{
					{
						Description: "formats id and amount",
						Input:       map[string]any{"orderId": "X1", "amount": 9.5},
						CheckOutput: true,
						WantOutput:  map[string]any{"summary": "Order X1: $9.50"},
					},
				},
			},
		},
	}
	if err := gated.Save(def); err != nil {
		t.Fatalf("Save() error = %v, want nil — this Definition's Example should pass", err)
	}

	// --- A later session: fresh Store, fresh registry, only the directory
	// on disk carried over. Loads the persisted piece alongside the real
	// Go catalog. ---
	laterStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore (later session): %v", err)
	}
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := catalog.RegisterFromStore(registry, laterStore); err != nil {
		t.Fatalf("RegisterFromStore: %v", err)
	}

	// --- The order API this flow will call. ---
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"orderId":"ORD-99","amount":42.5}`))
	}))
	defer server.Close()

	// --- The flow itself: pure JSON text, never a Go struct literal. ---
	flowJSON := `{
		"id": "fv-ai-first",
		"trigger": {
			"name": "trigger_1",
			"displayName": "Trigger",
			"type": "EMPTY",
			"nextAction": {
				"name": "fetch",
				"displayName": "Fetch",
				"type": "PIECE",
				"piece": {
					"pieceName": "http",
					"actionName": "request",
					"input": { "url": "` + server.URL + `" }
				},
				"nextAction": {
					"name": "parsed",
					"displayName": "Parse",
					"type": "PIECE",
					"piece": {
						"pieceName": "json",
						"actionName": "parse",
						"input": { "text": "{{ fetch.output.body }}" }
					},
					"nextAction": {
						"name": "summarized",
						"displayName": "Summarize",
						"type": "PIECE",
						"piece": {
							"pieceName": "order_utils",
							"actionName": "summarize",
							"input": {
								"orderId": "{{ parsed.output.data.orderId }}",
								"amount": "{{ parsed.output.data.amount }}"
							}
						},
						"nextAction": {
							"name": "shouted",
							"displayName": "Shout",
							"type": "PIECE",
							"piece": {
								"pieceName": "text",
								"actionName": "case",
								"input": {
									"text": "{{ summarized.output.summary }}",
									"mode": "upper"
								}
							},
							"nextAction": {
								"name": "persisted",
								"displayName": "Persist",
								"type": "PIECE",
								"piece": {
									"pieceName": "storage",
									"actionName": "write",
									"input": {
										"fileName": "order.txt",
										"content": "{{ shouted.output.text }}",
										"format": "text"
									}
								}
							}
						}
					}
				}
			}
		}
	}`

	fv, err := model.ParseFlowVersion([]byte(flowJSON))
	if err != nil {
		t.Fatalf("ParseFlowVersion: %v", err)
	}

	// --- Validate the parsed flow BEFORE ever running it. ---
	if errs := flowvalidate.Validate(fv, registry); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors for a well-formed flow", errs)
	}

	// --- Now actually run it. ---
	e := engine.New(registry)
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	summarizedOut := state.Steps["summarized"].Output.(map[string]any)
	if summarizedOut["summary"] != "Order ORD-99: $42.50" {
		t.Fatalf("summary = %v, want %q", summarizedOut["summary"], "Order ORD-99: $42.50")
	}

	shoutedOut := state.Steps["shouted"].Output.(map[string]any)
	if shoutedOut["text"] != "ORDER ORD-99: $42.50" {
		t.Fatalf("shouted text = %v", shoutedOut["text"])
	}

	persistedOut := state.Steps["persisted"].Output.(map[string]any)
	fileURL, _ := persistedOut["fileURL"].(string)
	if fileURL == "" {
		t.Fatal("fileURL is empty")
	}
	writer := e.Files.(*piece.MemoryFileWriter)
	stored, ok := writer.Get(fileURL)
	if !ok || string(stored) != "ORDER ORD-99: $42.50" {
		t.Fatalf("stored file = %q, ok=%v", stored, ok)
	}
}
