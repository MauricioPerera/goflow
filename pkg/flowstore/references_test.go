package flowstore

import (
	"testing"

	"goflow/pkg/model"
)

func flowWithCredentialInCodeInput(name, credentialName string) FlowDefinition {
	return FlowDefinition{
		Name: name,
		Flow: model.FlowVersion{
			ID: "fv-" + name,
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "use", DisplayName: "Use", Type: model.ActionCode,
					Code: &model.CodeSettings{
						Input:  map[string]any{"auth": map[string]any{"$credential": credentialName}},
						Source: `(params) => params`,
					},
				},
			},
		},
	}
}

func TestFindFlowsReferencingCredential_MatchesTopLevelInputMarker(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(flowWithCredentialInCodeInput("uses-it", "api-key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "unrelated", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save unrelated: %v", err)
	}

	names, err := FindFlowsReferencingCredential(store, "api-key")
	if err != nil {
		t.Fatalf("FindFlowsReferencingCredential: %v", err)
	}
	if len(names) != 1 || names[0] != "uses-it" {
		t.Fatalf("names = %v, want exactly [\"uses-it\"]", names)
	}
}

func TestFindFlowsReferencingCredential_MatchesNestedInputMarker(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	def := FlowDefinition{
		Name: "nested",
		Flow: model.FlowVersion{
			ID: "fv-nested",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "use", DisplayName: "Use", Type: model.ActionCode,
					Code: &model.CodeSettings{
						Input: map[string]any{
							"config": map[string]any{
								"headers": map[string]any{"Authorization": map[string]any{"$credential": "deep-key"}},
							},
						},
						Source: `(params) => params`,
					},
				},
			},
		},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingCredential(store, "deep-key")
	if err != nil {
		t.Fatalf("FindFlowsReferencingCredential: %v", err)
	}
	if len(names) != 1 || names[0] != "nested" {
		t.Fatalf("names = %v, want exactly [\"nested\"] — a nested marker must still be found", names)
	}
}

func TestFindFlowsReferencingCredential_MatchesCallFlowInput(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	def := FlowDefinition{
		Name: "calls-with-cred",
		Flow: model.FlowVersion{
			ID: "fv-calls",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "call", DisplayName: "Call", Type: model.ActionCallFlow,
					CallFlow: &model.CallFlowSettings{FlowName: "sub", Input: map[string]any{"token": map[string]any{"$credential": "call-key"}}},
				},
			},
		},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingCredential(store, "call-key")
	if err != nil {
		t.Fatalf("FindFlowsReferencingCredential: %v", err)
	}
	if len(names) != 1 || names[0] != "calls-with-cred" {
		t.Fatalf("names = %v, want exactly [\"calls-with-cred\"] — CALL_FLOW's own Input must be scanned too", names)
	}
}

func TestFindFlowsReferencingCredential_MatchesWebhookSecretCredential(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	def := FlowDefinition{Name: "webhook-flow", WebhookEnabled: true, WebhookSecretCredential: "webhook-secret", Flow: validCodeFlow()}
	if err := store.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingCredential(store, "webhook-secret")
	if err != nil {
		t.Fatalf("FindFlowsReferencingCredential: %v", err)
	}
	if len(names) != 1 || names[0] != "webhook-flow" {
		t.Fatalf("names = %v, want exactly [\"webhook-flow\"]", names)
	}
}

func TestFindFlowsReferencingCredential_NoMatches_EmptyNotError(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "unrelated", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingCredential(store, "never-used")
	if err != nil {
		t.Fatalf("FindFlowsReferencingCredential: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %v, want empty", names)
	}
}

func TestFindFlowsReferencingCredential_SameCredentialDifferentName_NotMatched(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(flowWithCredentialInCodeInput("uses-other", "other-key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingCredential(store, "api-key")
	if err != nil {
		t.Fatalf("FindFlowsReferencingCredential: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %v, want empty — a different credential name must not match", names)
	}
}

// --- FindFlowsReferencingPiece -----------------------------------------

func flowUsingPiece(name, pieceName string) FlowDefinition {
	return FlowDefinition{
		Name: name,
		Flow: model.FlowVersion{
			ID: "fv-" + name,
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "call", DisplayName: "Call", Type: model.ActionPiece,
					Piece: &model.PieceSettings{PieceName: pieceName, ActionName: "do"},
				},
			},
		},
	}
}

func TestFindFlowsReferencingPiece_MatchesPieceAction(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(flowUsingPiece("uses-http", "http")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "unrelated", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save unrelated: %v", err)
	}

	names, err := FindFlowsReferencingPiece(store, "http")
	if err != nil {
		t.Fatalf("FindFlowsReferencingPiece: %v", err)
	}
	if len(names) != 1 || names[0] != "uses-http" {
		t.Fatalf("names = %v, want exactly [\"uses-http\"]", names)
	}
}

func TestFindFlowsReferencingPiece_MatchesPieceTrigger(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	def := FlowDefinition{
		Name: "triggered-by-schedule",
		Flow: model.FlowVersion{
			ID: "fv-scheduled",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerPiece,
				PieceName: "schedule", TriggerName: "poll",
			},
		},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingPiece(store, "schedule")
	if err != nil {
		t.Fatalf("FindFlowsReferencingPiece: %v", err)
	}
	if len(names) != 1 || names[0] != "triggered-by-schedule" {
		t.Fatalf("names = %v, want exactly [\"triggered-by-schedule\"]", names)
	}
}

func TestFindFlowsReferencingPiece_MatchesInsideRouterBranch(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	def := FlowDefinition{
		Name: "routed",
		Flow: model.FlowVersion{
			ID: "fv-routed",
			Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: &model.FlowAction{
					Name: "router", DisplayName: "Router", Type: model.ActionRouter,
					Router: &model.RouterSettings{
						Branches: []model.RouterBranch{{Name: "b", Type: model.BranchFallback}},
						Children: []*model.FlowAction{{
							Name: "call", DisplayName: "Call", Type: model.ActionPiece,
							Piece: &model.PieceSettings{PieceName: "deep_piece", ActionName: "do"},
						}},
					},
				},
			},
		},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingPiece(store, "deep_piece")
	if err != nil {
		t.Fatalf("FindFlowsReferencingPiece: %v", err)
	}
	if len(names) != 1 || names[0] != "routed" {
		t.Fatalf("names = %v, want exactly [\"routed\"] — a piece used inside a router branch must still be found", names)
	}
}

func TestFindFlowsReferencingPiece_NoMatches_EmptyNotError(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Save(FlowDefinition{Name: "unrelated", Flow: validCodeFlow()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	names, err := FindFlowsReferencingPiece(store, "never-used")
	if err != nil {
		t.Fatalf("FindFlowsReferencingPiece: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %v, want empty", names)
	}
}
