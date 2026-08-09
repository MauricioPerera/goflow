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
