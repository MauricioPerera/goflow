package okf

import (
	"strings"
	"testing"
	"time"

	"goflow/pkg/catalog"
	"goflow/pkg/credentials"
	"goflow/pkg/flowstore"
	"goflow/pkg/model"
)

var testCredKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

func testFlowStore(t *testing.T) *flowstore.FileStore {
	t.Helper()
	fs, err := flowstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("flowstore.NewFileStore: %v", err)
	}
	return fs
}

func testCredStore(t *testing.T) *credentials.FileStore {
	t.Helper()
	cs, err := credentials.NewFileStore(t.TempDir(), testCredKey)
	if err != nil {
		t.Fatalf("credentials.NewFileStore: %v", err)
	}
	return cs
}

func codeFlowUsingPiece(pieceName string) model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-okf-test",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "call", DisplayName: "Call", Type: model.ActionPiece,
				Piece: &model.PieceSettings{PieceName: pieceName, ActionName: "run", Input: map[string]any{}},
			},
		},
	}
}

func TestExportBundle_IncludesGoPieces_AllConformant(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

	bundle, err := ExportBundle(catalogStore, fs, nil, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	if _, ok := bundle["pieces/webhook.md"]; !ok {
		t.Fatalf("bundle missing pieces/webhook.md (a built-in Go piece); bundle keys=%v", keysOf(bundle))
	}
	if err := ValidateConformance(bundle["pieces/webhook.md"]); err != nil {
		t.Fatalf("pieces/webhook.md not conformant: %v", err)
	}
}

func TestExportBundle_IncludesJSPiece_WithDescription(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	if err := catalogStore.Save(catalog.Definition{
		Name: "stripe", DisplayName: "Stripe", Description: "Create charges and refunds.",
		Actions: []catalog.ActionDefinition{{
			Name: "run", DisplayName: "Run", Description: "Runs it.",
			InputSchema: "amount (integer, required)", RequiresAuth: "Stripe secret key",
			Source: "(ctx) => ({})",
		}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fs := testFlowStore(t)
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, nil, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	doc, ok := bundle["pieces/stripe.md"]
	if !ok {
		t.Fatalf("bundle missing pieces/stripe.md; bundle keys=%v", keysOf(bundle))
	}
	if err := ValidateConformance(doc); err != nil {
		t.Fatalf("pieces/stripe.md not conformant: %v", err)
	}
	if !strings.Contains(doc, "Create charges and refunds.") {
		t.Fatalf("doc missing piece description: %s", doc)
	}
	if !strings.Contains(doc, "Requires auth: Stripe secret key") {
		t.Fatalf("doc missing requiresAuth: %s", doc)
	}
	if !strings.Contains(doc, "amount (integer, required)") {
		t.Fatalf("doc missing inputSchema: %s", doc)
	}
}

func TestExportBundle_IncludesFlow(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	if err := fs.Save(flowstore.FlowDefinition{
		Name: "my-flow", DisplayName: "My Flow", Description: "does a thing",
		Flow: codeFlowUsingPiece("webhook"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, nil, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	doc, ok := bundle["flows/my-flow.md"]
	if !ok {
		t.Fatalf("bundle missing flows/my-flow.md; bundle keys=%v", keysOf(bundle))
	}
	if err := ValidateConformance(doc); err != nil {
		t.Fatalf("flows/my-flow.md not conformant: %v", err)
	}
	if !strings.Contains(doc, "does a thing") {
		t.Fatalf("doc missing flow description: %s", doc)
	}
	if !strings.Contains(doc, "EMPTY") {
		t.Fatalf("doc missing trigger type: %s", doc)
	}
}

func TestExportBundle_PieceUsedBySection_ListsReferencingFlows(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	if err := fs.Save(flowstore.FlowDefinition{Name: "caller-flow", Flow: codeFlowUsingPiece("webhook")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, nil, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	doc := bundle["pieces/webhook.md"]
	if !strings.Contains(doc, "[caller-flow](/flows/caller-flow.md)") {
		t.Fatalf("pieces/webhook.md missing a used-by link to caller-flow: %s", doc)
	}
}

func TestExportBundle_UnreferencedPiece_SaysNone(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, nil, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	doc := bundle["pieces/webhook.md"]
	if !strings.Contains(doc, "(no saved flow currently references this)") {
		t.Fatalf("pieces/webhook.md = %s, want the no-references placeholder", doc)
	}
}

func TestExportBundle_CredentialConcept_NeverLeaksValue(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	cs := testCredStore(t)
	const secret = "sk_live_totally_secret_value_never_leak_this"
	if err := cs.Save("stripe-key", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, cs, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	doc, ok := bundle["credentials/stripe-key.md"]
	if !ok {
		t.Fatalf("bundle missing credentials/stripe-key.md; bundle keys=%v", keysOf(bundle))
	}
	if err := ValidateConformance(doc); err != nil {
		t.Fatalf("credentials/stripe-key.md not conformant: %v", err)
	}
	for path, content := range bundle {
		if strings.Contains(content, secret) {
			t.Fatalf("bundle path %q leaks the credential's decrypted value", path)
		}
	}
}

func TestExportBundle_CredentialUsedBySection_ListsReferencingFlows(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	cs := testCredStore(t)
	if err := cs.Save("my-secret", "value"); err != nil {
		t.Fatalf("Save credential: %v", err)
	}
	flow := model.FlowVersion{
		ID: "fv-cred-test",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "step", DisplayName: "Step", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Input:  map[string]any{"secret": map[string]any{"$credential": "my-secret"}},
					Source: "(params) => params",
				},
			},
		},
	}
	if err := fs.Save(flowstore.FlowDefinition{Name: "cred-user-flow", Flow: flow}); err != nil {
		t.Fatalf("Save flow: %v", err)
	}
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, cs, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	doc := bundle["credentials/my-secret.md"]
	if !strings.Contains(doc, "[cred-user-flow](/flows/cred-user-flow.md)") {
		t.Fatalf("credentials/my-secret.md missing a used-by link to cred-user-flow: %s", doc)
	}
}

func TestExportBundle_NilCredStore_EmptySectionNoError(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, nil, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	doc, ok := bundle["credentials/index.md"]
	if !ok {
		t.Fatal("bundle missing credentials/index.md even with a nil credStore")
	}
	if !strings.Contains(doc, "(none)") {
		t.Fatalf("credentials/index.md = %s, want the empty-section placeholder", doc)
	}
}

func TestExportBundle_RootIndex_ListsAllThreeSections(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	fs := testFlowStore(t)
	if err := fs.Save(flowstore.FlowDefinition{Name: "a-flow", Flow: codeFlowUsingPiece("webhook")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, nil, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	root, ok := bundle["index.md"]
	if !ok {
		t.Fatal("bundle missing root index.md")
	}
	for _, want := range []string{"# Pieces", "# Flows", "# Credentials", "[a-flow](/flows/a-flow.md)"} {
		if !strings.Contains(root, want) {
			t.Fatalf("root index.md missing %q: %s", want, root)
		}
	}
}

func TestExportBundle_EveryConceptDocument_IsConformant(t *testing.T) {
	catalogStore := catalog.NewMemoryStore()
	if err := catalogStore.Save(catalog.Definition{
		Name: "shout", DisplayName: "Shout",
		Actions: []catalog.ActionDefinition{{Name: "run", DisplayName: "Run", Source: "(ctx) => ({})"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fs := testFlowStore(t)
	if err := fs.Save(flowstore.FlowDefinition{Name: "flow-a", Flow: codeFlowUsingPiece("webhook")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cs := testCredStore(t)
	if err := cs.Save("cred-a", "value"); err != nil {
		t.Fatalf("Save credential: %v", err)
	}
	now := time.Now()

	bundle, err := ExportBundle(catalogStore, fs, cs, now)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	for path, doc := range bundle {
		if strings.HasSuffix(path, "index.md") {
			// §8: index files carry no frontmatter — nothing to validate
			// with ValidateConformance, which requires one.
			if strings.HasPrefix(doc, "---") {
				t.Fatalf("%s: index files must not carry frontmatter", path)
			}
			continue
		}
		if err := ValidateConformance(doc); err != nil {
			t.Fatalf("%s not conformant: %v; doc=%s", path, err, doc)
		}
	}
}

func keysOf(b Bundle) []string {
	out := make([]string, 0, len(b))
	for k := range b {
		out = append(out, k)
	}
	return out
}
