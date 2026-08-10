package catalog_test

import (
	"strings"
	"testing"

	"goflow/pkg/catalog"
)

func TestDescribeCombined_BothEmpty(t *testing.T) {
	text, err := catalog.DescribeCombined(catalog.NewMemoryStore(), nil)
	if err != nil {
		t.Fatalf("DescribeCombined() error = %v", err)
	}
	if !strings.Contains(text, "empty") {
		t.Fatalf("text = %q, want it to say the catalog is empty", text)
	}
}

func TestDescribeCombined_OnlyGoCatalog(t *testing.T) {
	goCatalog := map[string]string{
		"http": "real HTTP requests",
		"json": "parse/stringify JSON",
	}
	text, err := catalog.DescribeCombined(catalog.NewMemoryStore(), goCatalog)
	if err != nil {
		t.Fatalf("DescribeCombined() error = %v", err)
	}
	for _, want := range []string{
		"Go pieces (compiled):",
		"- http: real HTTP requests",
		"- json: parse/stringify JSON",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want it to contain %q", text, want)
		}
	}
	if strings.Contains(text, "JS pieces") {
		t.Fatalf("text = %q, want no JS section when store is empty", text)
	}
	// Stable alphabetical ordering: http before json (h < j).
	if strings.Index(text, "http") > strings.Index(text, "json") {
		t.Fatalf("text = %q, want Go pieces sorted by name", text)
	}
}

func TestDescribeCombined_OnlyStore(t *testing.T) {
	store := catalog.NewMemoryStore()
	store.Save(sampleDefinition("risk_score"))
	store.Save(catalog.Definition{
		Name: "greeter", DisplayName: "Greeter", Description: "says hello",
		Actions: []catalog.ActionDefinition{
			{Name: "greet", DisplayName: "Greet", Description: "returns a greeting"},
		},
	})

	text, err := catalog.DescribeCombined(store, nil)
	if err != nil {
		t.Fatalf("DescribeCombined() error = %v", err)
	}
	if strings.Contains(text, "Go pieces") {
		t.Fatalf("text = %q, want no Go section when goCatalog is empty", text)
	}
	for _, want := range []string{
		"JS pieces (saved in catalog):",
		"greeter", "says hello",
		"greeter.greet: returns a greeting",
		"risk_score", "does sample things",
		"risk_score.run: runs the sample action",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want it to contain %q", text, want)
		}
	}
	// Stable ordering within the JS section: greeter before risk_score.
	if strings.Index(text, "greeter") > strings.Index(text, "risk_score") {
		t.Fatalf("text = %q, want JS pieces sorted by name", text)
	}
}

func TestDescribeCombined_BothPopulated(t *testing.T) {
	store := catalog.NewMemoryStore()
	store.Save(sampleDefinition("risk_score"))
	store.Save(catalog.Definition{
		Name: "greeter", DisplayName: "Greeter", Description: "says hello",
		Actions: []catalog.ActionDefinition{
			{Name: "greet", DisplayName: "Greet", Description: "returns a greeting"},
		},
	})
	goCatalog := map[string]string{
		"http": "real HTTP requests",
		"json": "parse/stringify JSON",
	}

	text, err := catalog.DescribeCombined(store, goCatalog)
	if err != nil {
		t.Fatalf("DescribeCombined() error = %v", err)
	}

	// Both section titles present, Go section before JS section.
	for _, want := range []string{"Go pieces (compiled):", "JS pieces (saved in catalog):"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want it to contain %q", text, want)
		}
	}
	if strings.Index(text, "Go pieces (compiled):") > strings.Index(text, "JS pieces (saved in catalog):") {
		t.Fatalf("text = %q, want Go section before JS section", text)
	}

	// Names from both catalogs appear.
	for _, want := range []string{"- http: real HTTP requests", "- json: parse/stringify JSON", "greeter", "risk_score"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want it to contain %q", text, want)
		}
	}

	// The JS section reuses Describe's exact per-piece rendering.
	solo, err := catalog.Describe(store)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	jsSection := text[strings.Index(text, "JS pieces (saved in catalog):"):]
	// Strip the title line from jsSection before comparing to Describe's body.
	jsBody := strings.TrimPrefix(jsSection, "JS pieces (saved in catalog):\n")
	if jsBody != solo {
		t.Fatalf("JS section body = %q, want it to equal Describe() output %q", jsBody, solo)
	}
}

func TestDescribeCombined_StableAcrossCalls(t *testing.T) {
	store := catalog.NewMemoryStore()
	store.Save(sampleDefinition("zebra"))
	store.Save(sampleDefinition("alpha"))
	goCatalog := map[string]string{
		"zpiece": "z desc",
		"apiece": "a desc",
	}

	first, err := catalog.DescribeCombined(store, goCatalog)
	if err != nil {
		t.Fatalf("first DescribeCombined() error = %v", err)
	}
	second, err := catalog.DescribeCombined(store, goCatalog)
	if err != nil {
		t.Fatalf("second DescribeCombined() error = %v", err)
	}
	if first != second {
		t.Fatalf("output not stable between calls:\nfirst  = %q\nsecond = %q", first, second)
	}

	// Alphabetical within each section.
	if strings.Index(first, "apiece") > strings.Index(first, "zpiece") {
		t.Fatalf("Go pieces not sorted: %q", first)
	}
	if strings.Index(first, "alpha") > strings.Index(first, "zebra") {
		t.Fatalf("JS pieces not sorted: %q", first)
	}
}
