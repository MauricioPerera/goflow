package catalog_test

import (
	"strings"
	"testing"

	"goflow/pkg/catalog"
)

func TestDescribe_EmptyStore(t *testing.T) {
	text, err := catalog.Describe(catalog.NewMemoryStore())
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if !strings.Contains(text, "empty") {
		t.Fatalf("text = %q, want it to say the catalog is empty", text)
	}
}

func TestDescribe_ListsNameDescriptionAndActions(t *testing.T) {
	store := catalog.NewMemoryStore()
	store.Save(sampleDefinition("risk_score"))
	store.Save(catalog.Definition{
		Name: "greeter", DisplayName: "Greeter", Description: "says hello",
		Actions: []catalog.ActionDefinition{
			{Name: "greet", DisplayName: "Greet", Description: "returns a greeting"},
		},
	})

	text, err := catalog.Describe(store)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	for _, want := range []string{
		"risk_score", "does sample things",
		"risk_score.run: runs the sample action",
		"input: x (number, required)",
		"greeter", "says hello",
		"greeter.greet: returns a greeting",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want it to contain %q", text, want)
		}
	}

	// Stable ordering: "greeter" sorts before "risk_score".
	if strings.Index(text, "greeter") > strings.Index(text, "risk_score") {
		t.Fatalf("text = %q, want entries sorted by name", text)
	}
}

func TestDescribe_ShowsRequiresAuthWhenSet(t *testing.T) {
	store := catalog.NewMemoryStore()
	store.Save(catalog.Definition{
		Name: "slack", DisplayName: "Slack", Description: "posts to slack",
		Actions: []catalog.ActionDefinition{
			{
				Name: "post", DisplayName: "Post", Description: "posts a message",
				Source:       `(ctx) => ({ ok: true })`,
				RequiresAuth: "Slack Bot Token (string, starts with xoxb-)",
			},
		},
	})

	text, err := catalog.Describe(store)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if !strings.Contains(text, "requires auth: Slack Bot Token (string, starts with xoxb-)") {
		t.Fatalf("text = %q, want it to show the action's RequiresAuth", text)
	}
}

func TestDescribe_OmitsRequiresAuthLineWhenUnset(t *testing.T) {
	store := catalog.NewMemoryStore()
	store.Save(sampleDefinition("risk_score")) // RequiresAuth left empty
	text, err := catalog.Describe(store)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if strings.Contains(text, "requires auth") {
		t.Fatalf("text = %q, want no \"requires auth\" line for an action that doesn't set it", text)
	}
}
