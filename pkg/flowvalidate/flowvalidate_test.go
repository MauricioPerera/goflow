package flowvalidate_test

import (
	"testing"

	"goflow/pkg/flowvalidate"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

func realRegistry(t *testing.T) *piece.Registry {
	t.Helper()
	r := piece.NewRegistry()
	if err := pieces.RegisterAll(r); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	return r
}

func trigger(next *model.FlowAction) *model.FlowTrigger {
	return &model.FlowTrigger{Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty, NextAction: next}
}

func codeAction(name string) *model.FlowAction {
	return &model.FlowAction{
		Name: name, DisplayName: name, Type: model.ActionCode,
		Code: &model.CodeSettings{Source: "(params) => params"},
	}
}

func TestValidate_WellFormedFlowHasNoErrors(t *testing.T) {
	step2 := codeAction("step_2")
	step1 := codeAction("step_1")
	step1.NextAction = step2
	step1.Code.Input = map[string]any{"x": "{{ trigger_1.output.n + 1 }}"}

	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step1)}

	if errs := flowvalidate.Validate(fv, nil); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
}

func TestValidate_NoTrigger(t *testing.T) {
	fv := &model.FlowVersion{ID: "fv-1"}
	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 || errs[0].Path != "trigger" {
		t.Fatalf("errs = %+v, want exactly one \"trigger\" error", errs)
	}
}

func TestValidate_UnknownPieceTriggerFailsAgainstRealRegistry(t *testing.T) {
	fv := &model.FlowVersion{ID: "fv-1", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerPiece,
		PieceName: "nonexistent", TriggerName: "nope",
	}}
	errs := flowvalidate.Validate(fv, realRegistry(t))
	if len(errs) != 1 || errs[0].Path != "trigger" {
		t.Fatalf("errs = %+v, want exactly one \"trigger\" error", errs)
	}
}

func TestValidate_KnownPieceTriggerPasses(t *testing.T) {
	fv := &model.FlowVersion{ID: "fv-1", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerPiece,
		PieceName: "webhook", TriggerName: "catch_hook", Input: map[string]any{},
	}}
	if errs := flowvalidate.Validate(fv, realRegistry(t)); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
}

func TestValidate_UnknownPieceActionFailsAgainstRealRegistry(t *testing.T) {
	step := &model.FlowAction{
		Name: "step_1", DisplayName: "Step 1", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "does_not_exist", Input: map[string]any{}},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step)}
	errs := flowvalidate.Validate(fv, realRegistry(t))
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one error", errs)
	}
}

func TestValidate_NilRegistrySkipsPieceExistenceChecks(t *testing.T) {
	step := &model.FlowAction{
		Name: "step_1", DisplayName: "Step 1", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "totally_made_up", ActionName: "whatever", Input: map[string]any{}},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step)}
	if errs := flowvalidate.Validate(fv, nil); len(errs) != 0 {
		t.Fatalf("Validate() with nil registry = %+v, want no errors — piece existence isn't checked without a registry", errs)
	}
}

func TestValidate_TypeSettingsMismatch(t *testing.T) {
	cases := []struct {
		name   string
		action *model.FlowAction
	}{
		{"CODE with nil Code", &model.FlowAction{Name: "s", Type: model.ActionCode}},
		{"PIECE with nil Piece", &model.FlowAction{Name: "s", Type: model.ActionPiece}},
		{"ROUTER with nil Router", &model.FlowAction{Name: "s", Type: model.ActionRouter}},
		{"LOOP_ON_ITEMS with nil Loop", &model.FlowAction{Name: "s", Type: model.ActionLoopOnItems}},
		{"unknown type", &model.FlowAction{Name: "s", Type: "BOGUS"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(c.action)}
			errs := flowvalidate.Validate(fv, nil)
			if len(errs) != 1 {
				t.Fatalf("errs = %+v, want exactly one error", errs)
			}
		})
	}
}

func TestValidate_DuplicateStepNames(t *testing.T) {
	step2 := codeAction("same_name")
	step1 := codeAction("same_name")
	step1.NextAction = step2
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step1)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 || errs[0].Path != "steps[same_name]" {
		t.Fatalf("errs = %+v, want exactly one duplicate-name error", errs)
	}
}

func TestValidate_DuplicateNameAgainstTrigger(t *testing.T) {
	step := codeAction("trigger_1") // collides with the trigger's own Name
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 || errs[0].Path != "steps[trigger_1]" {
		t.Fatalf("errs = %+v, want exactly one duplicate-name error", errs)
	}
}

func TestValidate_SimpleCycleIsDetected(t *testing.T) {
	a := codeAction("a")
	b := codeAction("b")
	a.NextAction = b
	b.NextAction = a // cycle
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(a)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a cycle detected")
	}
	found := false
	for _, e := range errs {
		if e.Path == "trigger" {
			found = true
		}
	}
	if !found {
		t.Fatalf("errs = %+v, want the cycle error's Path to be \"trigger\" (the chain it was detected in)", errs)
	}
}

func TestValidate_SelfLoopIsDetected(t *testing.T) {
	a := codeAction("a")
	a.NextAction = a // points to itself
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(a)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want a self-loop detected")
	}
}

func TestValidate_RouterChildrenBranchesLengthMismatch(t *testing.T) {
	router := &model.FlowAction{
		Name: "route", DisplayName: "Route", Type: model.ActionRouter,
		Router: &model.RouterSettings{
			Branches: []model.RouterBranch{{Name: "a", Type: model.BranchFallback}},
			Children: []*model.FlowAction{codeAction("x"), codeAction("y")}, // 2 children, 1 branch
		},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(router)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one length-mismatch error", errs)
	}
}

func TestValidate_RouterChildrenGetWalkedIndependently(t *testing.T) {
	branchA := codeAction("branch_a")
	branchB := codeAction("branch_b")
	router := &model.FlowAction{
		Name: "route", DisplayName: "Route", Type: model.ActionRouter,
		Router: &model.RouterSettings{
			Branches: []model.RouterBranch{{Name: "a", Type: model.BranchFallback}, {Name: "b", Type: model.BranchFallback}},
			Children: []*model.FlowAction{branchA, branchB},
		},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(router)}
	if errs := flowvalidate.Validate(fv, nil); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}

	// Now make one branch cycle back to itself and confirm it's caught,
	// proving each branch is actually walked (not skipped).
	branchA.NextAction = branchA
	errs := flowvalidate.Validate(fv, nil)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want the branch's own cycle to be caught")
	}
}

func TestValidate_LoopBodyGetsWalked(t *testing.T) {
	body := codeAction("body")
	body.NextAction = body // cycle inside the loop body
	loop := &model.FlowAction{
		Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{Items: "{{ [1,2,3] }}", FirstLoopAction: body},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(loop)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) == 0 {
		t.Fatal("Validate() = no errors, want the loop body's cycle to be caught")
	}
}

func TestValidate_LoopItemsInvalidSyntax(t *testing.T) {
	loop := &model.FlowAction{
		Name: "loop", DisplayName: "Loop", Type: model.ActionLoopOnItems,
		Loop: &model.LoopSettings{Items: "{{ [1, 2, }}"}, // malformed
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(loop)}
	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one syntax error", errs)
	}
}

func TestValidate_CallFlowWellFormed_NoErrors(t *testing.T) {
	call := &model.FlowAction{
		Name: "call", DisplayName: "Call", Type: model.ActionCallFlow,
		CallFlow: &model.CallFlowSettings{FlowName: "sub-flow", Input: map[string]any{"n": "{{ trigger_1.output.n }}"}},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(call)}
	if errs := flowvalidate.Validate(fv, nil); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
}

func TestValidate_CallFlowNilSettings(t *testing.T) {
	call := &model.FlowAction{Name: "call", DisplayName: "Call", Type: model.ActionCallFlow}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(call)}
	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one error for nil CallFlow settings", errs)
	}
}

func TestValidate_CallFlowMissingFlowName(t *testing.T) {
	call := &model.FlowAction{
		Name: "call", DisplayName: "Call", Type: model.ActionCallFlow,
		CallFlow: &model.CallFlowSettings{},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(call)}
	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 || errs[0].Path != "trigger > call.callFlow.flowName" {
		t.Fatalf("errs = %+v, want exactly one error at \"trigger > call.callFlow.flowName\"", errs)
	}
}

func TestValidate_CallFlowInputInvalidTemplateSyntax(t *testing.T) {
	call := &model.FlowAction{
		Name: "call", DisplayName: "Call", Type: model.ActionCallFlow,
		CallFlow: &model.CallFlowSettings{FlowName: "sub-flow", Input: map[string]any{"n": "{{ 1 + }}"}},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(call)}
	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one syntax error", errs)
	}
}

func TestValidate_CodeSourceInvalidSyntax(t *testing.T) {
	step := codeAction("step_1")
	step.Code.Source = "(params) => { this is not valid js !! }"
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one syntax error", errs)
	}
}

func TestValidate_TemplateExpressionInvalidSyntax(t *testing.T) {
	step := codeAction("step_1")
	step.Code.Input = map[string]any{"x": "hello {{ 1 +++ }} world"}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step)}

	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one syntax error", errs)
	}
}

func TestValidate_TemplateExpressionInsideNestedInputIsChecked(t *testing.T) {
	step := &model.FlowAction{
		Name: "step_1", DisplayName: "Step 1", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "stringify", Input: map[string]any{
			"data": map[string]any{
				"list": []any{"{{ 1 +++ }}"}, // malformed, nested two levels deep
			},
		}},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step)}
	errs := flowvalidate.Validate(fv, realRegistry(t))
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one syntax error found inside the nested Input", errs)
	}
}

func TestValidate_ConditionValuesAreChecked(t *testing.T) {
	router := &model.FlowAction{
		Name: "route", DisplayName: "Route", Type: model.ActionRouter,
		Router: &model.RouterSettings{
			Branches: []model.RouterBranch{
				{
					Name: "a", Type: model.BranchCondition,
					Conditions: [][]model.Condition{{
						{Operator: model.OpTextExactlyMatches, FirstValue: "{{ 1 +++ }}", SecondValue: "x"},
					}},
				},
			},
			Children: []*model.FlowAction{codeAction("x")},
		},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(router)}
	errs := flowvalidate.Validate(fv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one syntax error from the condition's FirstValue", errs)
	}
}

func TestValidate_RouterConditionBranchWithEmptyConditions(t *testing.T) {
	cases := []struct {
		name     string
		branches []model.RouterBranch
		wantErrs int
		wantPath string // empty = don't assert a specific branch path
	}{
		{
			name:     "CONDITION branch with empty Conditions produces the error",
			branches: []model.RouterBranch{{Name: "a", Type: model.BranchCondition, Conditions: nil}},
			wantErrs: 1,
			wantPath: "trigger > route.router.branches[0]",
		},
		{
			name: "CONDITION branch with a condition group does not produce the error",
			branches: []model.RouterBranch{{
				Name: "a", Type: model.BranchCondition,
				Conditions: [][]model.Condition{{
					{Operator: model.OpTextExactlyMatches, FirstValue: "a", SecondValue: "b"},
				}},
			}},
			wantErrs: 0,
		},
		{
			name:     "FALLBACK branch with empty Conditions is valid and produces no error",
			branches: []model.RouterBranch{{Name: "a", Type: model.BranchFallback, Conditions: nil}},
			wantErrs: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			children := make([]*model.FlowAction, len(c.branches))
			for i := range c.branches {
				children[i] = codeAction("child_" + string(rune('a'+i)))
			}
			router := &model.FlowAction{
				Name: "route", DisplayName: "Route", Type: model.ActionRouter,
				Router: &model.RouterSettings{Branches: c.branches, Children: children},
			}
			fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(router)}
			errs := flowvalidate.Validate(fv, nil)
			if len(errs) != c.wantErrs {
				t.Fatalf("errs = %+v, want %d error(s)", errs, c.wantErrs)
			}
			if c.wantPath != "" {
				if errs[0].Path != c.wantPath {
					t.Fatalf("errs[0].Path = %q, want %q", errs[0].Path, c.wantPath)
				}
			}
		})
	}
}

func TestValidate_TriggerInputTemplatesAreChecked(t *testing.T) {
	fv := &model.FlowVersion{ID: "fv-1", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerPiece,
		PieceName: "webhook", TriggerName: "catch_hook",
		Input: map[string]any{"x": "{{ 1 +++ }}"},
	}}
	errs := flowvalidate.Validate(fv, realRegistry(t))
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want exactly one syntax error", errs)
	}
}

func TestValidate_NonTemplateStringsAreIgnored(t *testing.T) {
	step := codeAction("step_1")
	step.Code.Input = map[string]any{
		"plain":  "just plain text, no templates at all !! ++ this is fine",
		"number": int64(42),
		"bool":   true,
		"nested": map[string]any{"also_plain": "no braces here either"},
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(step)}
	if errs := flowvalidate.Validate(fv, nil); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors — none of these are {{ }} templates", errs)
	}
}

func TestValidate_RealCatalogFlowPassesCleanly(t *testing.T) {
	parseStep := &model.FlowAction{
		Name: "parsed", DisplayName: "Parse", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
			"text": "{{ fetch.output.body }}",
		}},
	}
	fetchStep := &model.FlowAction{
		Name: "fetch", DisplayName: "Fetch", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "request", Input: map[string]any{
			"url": "{{ trigger_1.output.url }}",
		}},
		NextAction: parseStep,
	}
	fv := &model.FlowVersion{ID: "fv-1", Trigger: trigger(fetchStep)}

	if errs := flowvalidate.Validate(fv, realRegistry(t)); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors for a well-formed real-catalog flow", errs)
	}
}
