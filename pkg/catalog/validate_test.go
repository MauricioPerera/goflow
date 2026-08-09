package catalog_test

import (
	"testing"

	"goflow/pkg/catalog"
)

func workingDefinition(name string) catalog.Definition {
	return catalog.Definition{
		Name: name, DisplayName: "Risk Score", Description: "classifies an amount",
		Actions: []catalog.ActionDefinition{
			{
				Name: "classify", DisplayName: "Classify",
				Description: "returns high/medium/low for a numeric amount",
				InputSchema: "amount (number, required)",
				Source: `(ctx) => {
					const amount = Number(ctx.input.amount);
					if (amount === undefined || isNaN(amount)) { throw new Error("missing required input: amount"); }
					let level;
					if (amount > 1000) level = "high";
					else if (amount > 100) level = "medium";
					else level = "low";
					return { level: level };
				}`,
				Examples: []catalog.Example{
					{
						Description: "large amount classifies as high",
						Input:       map[string]any{"amount": int64(5000)},
						CheckOutput: true,
						WantOutput:  map[string]any{"level": "high"},
					},
					{
						Description: "missing amount fails clearly",
						Input:       map[string]any{},
						WantError:   true,
					},
				},
			},
		},
	}
}

func TestValidate_PassingExamplesSucceed(t *testing.T) {
	if errs := catalog.Validate(workingDefinition("risk_score")); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
}

func TestValidate_NoExamplesFails(t *testing.T) {
	def := workingDefinition("risk_score")
	def.Actions[0].Examples = nil

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection for an action with zero Examples")
	}
}

func TestValidate_UnexpectedErrorFails(t *testing.T) {
	def := workingDefinition("risk_score")
	// This example expects success but the action actually errors (amount
	// missing) since CheckOutput/WantError weren't set to expect that.
	def.Actions[0].Examples = []catalog.Example{
		{Input: map[string]any{}},
	}

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection — the example errored but wasn't marked WantError")
	}
}

func TestValidate_ExpectedErrorButSucceededFails(t *testing.T) {
	def := workingDefinition("risk_score")
	def.Actions[0].Examples = []catalog.Example{
		{Input: map[string]any{"amount": int64(50)}, WantError: true},
	}

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection — the example succeeded but was marked WantError")
	}
}

func TestValidate_OutputMismatchFails(t *testing.T) {
	def := workingDefinition("risk_score")
	def.Actions[0].Examples = []catalog.Example{
		{
			Input:       map[string]any{"amount": int64(5000)},
			CheckOutput: true,
			WantOutput:  map[string]any{"level": "low"}, // wrong on purpose
		},
	}

	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a rejection for a mismatched output")
	}
}

func TestValidate_OutputComparisonToleratesNumericTypeQuirks(t *testing.T) {
	// The action returns a plain JS number untouched by arithmetic; this
	// proves the comparison doesn't false-fail over int64-vs-float64 goja
	// export quirks documented elsewhere in this project.
	def := catalog.Definition{
		Name: "echo", DisplayName: "Echo",
		Actions: []catalog.ActionDefinition{
			{
				Name: "echo", DisplayName: "Echo",
				Source: `(ctx) => ({ n: ctx.input.n })`,
				Examples: []catalog.Example{
					{
						Input:       map[string]any{"n": int64(42)},
						CheckOutput: true,
						WantOutput:  map[string]any{"n": float64(42)}, // deliberately the "other" numeric type
					},
				},
			},
		},
	}
	if errs := catalog.Validate(def); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors — int64(42) and float64(42) must compare equal", errs)
	}
}

func TestValidate_StructuralErrorsAreAlsoCaught(t *testing.T) {
	def := workingDefinition("") // empty Name -- structurally invalid
	errs := catalog.Validate(def)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a structural rejection for an empty Name")
	}
}

func TestValidate_CheckOutputFalseSkipsOutputComparison(t *testing.T) {
	def := workingDefinition("risk_score")
	def.Actions[0].Examples = []catalog.Example{
		{Input: map[string]any{"amount": int64(5000)}}, // CheckOutput left false
	}
	if errs := catalog.Validate(def); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors — CheckOutput is false, output shouldn't be compared at all", errs)
	}
}
