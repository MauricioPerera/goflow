package catalog_test

import (
	"testing"

	"goflow/pkg/catalog"
	"goflow/pkg/engine"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

// workingDropdownDefinition is the dropdown analogue of workingDefinition /
// workingTriggerDefinition: a Definition carrying one JS-authored action
// ("deploy") whose "region" property is a JS-authored Dropdown. The dropdown
// returns a fixed two-region option set, filtered by ctx.searchValue, with a
// placeholder and disabled=false; the action validates its chosen region
// against the same set. The action carries one passing Example and the
// dropdown carries two passing DropdownExamples — the baseline
// "structurally sound AND every example behaves as asserted" case Validate
// must accept.
func workingDropdownDefinition(name string) catalog.Definition {
	return catalog.Definition{
		Name: name, DisplayName: "Region Catalog", Description: "deploys to a region picked from a dropdown",
		Actions: []catalog.ActionDefinition{
			{
				Name: "deploy", DisplayName: "Deploy",
				Description: "validates the chosen region and reports where it deployed",
				InputSchema: "region (string, required — one of the dropdown's options)",
				Source: `(ctx) => {
					const valid = ["us-east-1", "eu-west-1"];
					if (valid.indexOf(ctx.input.region) === -1) {
						throw new Error("invalid region: " + ctx.input.region);
					}
					return { deployedTo: ctx.input.region, status: "deployed" };
				}`,
				Examples: []catalog.Example{
					{
						Description: "a valid region deploys",
						Input:       map[string]any{"region": "us-east-1"},
						CheckOutput: true,
						WantOutput:  map[string]any{"deployedTo": "us-east-1", "status": "deployed"},
					},
				},
				Dropdowns: map[string]catalog.DropdownDefinition{
					"region": {
						Source: `(propsValue, ctx) => {
							const all = [
								{ label: "US East", value: "us-east-1" },
								{ label: "EU West", value: "eu-west-1" },
							];
							const q = ctx.searchValue;
							const opts = q ? all.filter(o => o.value.indexOf(q) !== -1) : all;
							return {
								disabled: false,
								placeholder: "Select a region",
								options: opts,
							};
						}`,
						Examples: []catalog.DropdownExample{
							{
								Description:     "no search returns every region",
								SearchValue:     "",
								CheckOutput:     true,
								WantDisabled:    false,
								WantPlaceholder: "Select a region",
								WantOptions: []piece.DropdownOption{
									{Label: "US East", Value: "us-east-1"},
									{Label: "EU West", Value: "eu-west-1"},
								},
							},
							{
								Description:     "search filters to us-east-1",
								SearchValue:     "us-east",
								CheckOutput:     true,
								WantDisabled:    false,
								WantPlaceholder: "Select a region",
								WantOptions: []piece.DropdownOption{
									{Label: "US East", Value: "us-east-1"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestValidate_DropdownWithPassingExamplesSucceeds(t *testing.T) {
	if errs := catalog.Validate(workingDropdownDefinition("region_catalog")); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
}

func TestValidate_DropdownWithoutExamplesFails(t *testing.T) {
	def := workingDropdownDefinition("region_catalog")
	def = withRegionDropdownExamples(def, nil)

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection for a dropdown with zero Examples")
	}
}

func TestValidate_DropdownExampleExpectedErrorButSucceededFails(t *testing.T) {
	def := workingDropdownDefinition("region_catalog")
	// The dropdown succeeds for any propsValue/searchValue, but this example
	// claims it must error — Validate must reject that mismatch.
	def = withRegionDropdownExamples(def, []catalog.DropdownExample{
		{
			Description: "claims this errors",
			WantError:   true,
		},
	})

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection — the dropdown succeeded but the example was marked WantError")
	}
}

func TestValidate_DropdownExampleOutputMismatchFails(t *testing.T) {
	def := workingDropdownDefinition("region_catalog")
	def = withRegionDropdownExamples(def, []catalog.DropdownExample{
		{
			Description: "no search",
			CheckOutput: true,
			// WantOptions deliberately wrong (extra label the real dropdown
			// never returns) so the real dropdown's output can't match.
			WantOptions: []piece.DropdownOption{
				{Label: "Asia Pacific", Value: "ap-south-1"},
			},
		},
	})

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection for a mismatched dropdown output")
	}
}

// withRegionDropdownExamples replaces the "region" dropdown's Examples on
// def's first action. DropdownDefinition is a struct stored in a map, so its
// fields can't be assigned directly (Go rejects "cannot assign to struct
// field ... in map"); take the value out, mutate the copy, put it back.
func withRegionDropdownExamples(def catalog.Definition, exs []catalog.DropdownExample) catalog.Definition {
	dd := def.Actions[0].Dropdowns["region"]
	dd.Examples = exs
	def.Actions[0].Dropdowns["region"] = dd
	return def
}

func TestDefinition_ToPiece_BuildsInvocableDropdown(t *testing.T) {
	def := workingDropdownDefinition("region_catalog")
	p := def.ToPiece()

	if p.Name != "region_catalog" || p.DisplayName != "Region Catalog" {
		t.Fatalf("p = %+v", p)
	}
	act, ok := p.Actions["deploy"]
	if !ok {
		t.Fatal("action \"deploy\" not present after ToPiece()")
	}
	dd, ok := act.Dropdowns["region"]
	if !ok {
		t.Fatal("dropdown \"region\" not present on \"deploy\" after ToPiece()")
	}
	if dd.LoadOptions == nil {
		t.Fatal("LoadOptions is nil — ToPiece did not build a real dropdown")
	}

	// Invokable for real, not a mock: run it with no search and confirm it
	// returns the two regions a hand-written Go dropdown would.
	state, err := dd.LoadOptions(nil, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if state.Disabled {
		t.Fatalf("state.Disabled = true, want false")
	}
	if state.Placeholder != "Select a region" {
		t.Fatalf("state.Placeholder = %q, want %q", state.Placeholder, "Select a region")
	}
	if len(state.Options) != 2 || state.Options[0].Value != "us-east-1" || state.Options[1].Value != "eu-west-1" {
		t.Fatalf("state.Options = %+v, want [us-east-1, eu-west-1]", state.Options)
	}

	// And the search filter path the second example exercises, invoked
	// directly here too.
	filtered, err := dd.LoadOptions(nil, piece.PropertyContext{SearchValue: "us-east"})
	if err != nil {
		t.Fatalf("LoadOptions(search) error = %v", err)
	}
	if len(filtered.Options) != 1 || filtered.Options[0].Value != "us-east-1" {
		t.Fatalf("filtered.Options = %+v, want only us-east-1", filtered.Options)
	}
}

// TestDropdown_PersistedAcrossProcessesAndResolvesThroughRealEngine is the
// decisive end-to-end proof for dropdown persistence: a Definition with a
// valid action+dropdown is saved to disk by one FileStore instance wrapped in
// a GatedStore (standing in for one authoring process), loaded by a completely
// separate FileStore instance pointed at the same directory (a later
// process), registered into a fresh *piece.Registry via RegisterFromStore,
// and resolved through the real engine's public LoadOptions API — the same
// call path a real editor UI would use to populate a dropdown, exactly like
// pkg/pieces' own TestCatalog_JSDropdownComposesWithRealCatalog does for an
// in-memory jspiece dropdown.
func TestDropdown_PersistedAcrossProcessesAndResolvesThroughRealEngine(t *testing.T) {
	dir := t.TempDir()

	authoringStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	gated := &catalog.GatedStore{Underlying: authoringStore}
	if err := gated.Save(workingDropdownDefinition("region_catalog")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A later process: a brand new Store instance, a brand new registry,
	// nothing carried over except the directory on disk.
	laterStore, err := catalog.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore (later process): %v", err)
	}
	registry := piece.NewRegistry()
	// RegisterAll supplies the real Go catalog — same call
	// TestCatalog_JSDropdownComposesWithRealCatalog makes, so the persisted
	// JS dropdown piece coexists with the real catalog.
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := catalog.RegisterFromStore(registry, laterStore); err != nil {
		t.Fatalf("RegisterFromStore: %v", err)
	}

	state, err := engine.New(registry).LoadOptions(engine.LoadOptionsInput{
		PieceName: "region_catalog", ActionName: "deploy", PropertyName: "region",
	})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 2 || state.Options[0].Value != "us-east-1" || state.Options[1].Value != "eu-west-1" {
		t.Fatalf("state.Options = %+v, want [us-east-1, eu-west-1] resolved from the persisted dropdown", state.Options)
	}
}
