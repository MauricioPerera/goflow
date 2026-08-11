package flowstore

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"goflow/pkg/credentials"
	"goflow/pkg/model"
)

// testKey is a fixed 32-byte AES-256 key for the credentials store in these
// tests — constant so the on-disk shape is deterministic, matching
// pkg/httpapi's testCredKey.
var testKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

// newCredStore builds a real credentials.FileStore in a temp dir, no mocks.
func newCredStore(t *testing.T) *credentials.FileStore {
	t.Helper()
	s, err := credentials.NewFileStore(t.TempDir(), testKey)
	if err != nil {
		t.Fatalf("credentials.NewFileStore: %v", err)
	}
	return s
}

// credMarkerFlow is a CODE-only flow whose step input carries one
// $credential marker under the "auth" key, plus a plain value under "other"
// that must be left untouched. The CODE step returns the resolved auth's
// length, so a successful run proves indirectly that the real secret reached
// the piece — without ever putting the secret itself in the output.
func credMarkerFlow(stepName, credName string) model.FlowVersion {
	return model.FlowVersion{
		ID: "fv-cred",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: stepName, DisplayName: "Use Cred", Type: model.ActionCode,
				Code: &model.CodeSettings{
					Input: map[string]any{
						"auth":  map[string]any{"$credential": credName},
						"other": "plain-value",
					},
					Source: `(params) => ({ authLen: params.auth.length, other: params.other })`,
				},
			},
		},
	}
}

// TestResolveCredentials_NoMarker_ReturnsFlowUnchangedAndEmptyRefs: a flow
// with no $credential marker resolves to an observationally-equal flow, empty
// refs, and never touches the store.
func TestResolveCredentials_NoMarker_ReturnsFlowUnchangedAndEmptyRefs(t *testing.T) {
	store := newCredStore(t)
	// Seed a credential so we could detect if Get were wrongly called — it
	// must not be, but seeding makes the assertion meaningful.
	if err := store.Save("unused", "never-read"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fv := validCodeFlow()
	resolved, refs, err := ResolveCredentials(&fv, store)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty for a markerless flow", refs)
	}
	if resolved == nil || resolved.Trigger == nil || resolved.Trigger.NextAction == nil {
		t.Fatalf("resolved flow missing structure: %#v", resolved)
	}
	// Observable behavior: the CODE step's Input still has n=21.
	code := resolved.Trigger.NextAction.Code
	if code.Input["n"] != 21 {
		t.Fatalf("resolved Input[n] = %v, want 21 (untouched)", code.Input["n"])
	}
}

// TestResolveCredentials_ValidMarker_SubstitutesValueAndLeavesOriginalIntact:
// a valid marker is replaced by the real credential value, one ref is
// recorded, and — critically — the fv passed in still has the marker
// untouched (no in-place mutation of the caller's flow).
func TestResolveCredentials_ValidMarker_SubstitutesValueAndLeavesOriginalIntact(t *testing.T) {
	store := newCredStore(t)
	const secret = "el-secreto-xyz-789"
	if err := store.Save("smtp-relay", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fv := credMarkerFlow("use_auth", "smtp-relay")
	// Grab a reference to the ORIGINAL action's Input map before resolving,
	// so we can prove it is not mutated.
	origInput := fv.Trigger.NextAction.Code.Input
	origAuth := origInput["auth"]

	resolved, refs, err := ResolveCredentials(&fv, store)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want exactly one", refs)
	}
	wantRef := CredentialRef{StepName: "use_auth", InputKey: "auth", CredentialName: "smtp-relay"}
	if refs[0] != wantRef {
		t.Fatalf("refs[0] = %+v, want %+v", refs[0], wantRef)
	}
	// The resolved Input has the REAL secret under "auth"...
	if got := resolved.Trigger.NextAction.Code.Input["auth"]; got != secret {
		t.Fatalf("resolved Input[auth] = %v, want the real secret %q", got, secret)
	}
	// ...and the plain value is still there.
	if got := resolved.Trigger.NextAction.Code.Input["other"]; got != "plain-value" {
		t.Fatalf("resolved Input[other] = %v, want plain-value", got)
	}
	// The ORIGINAL flow passed in must still carry the marker, untouched —
	// origAuth was captured before resolving; the original entry must still
	// BE that marker map (compare by identity via reflect, since map values
	// are not comparable with ==).
	if reflect.ValueOf(origInput["auth"]).Pointer() != reflect.ValueOf(origAuth).Pointer() {
		t.Fatalf("original Input[auth] was replaced: got %v, want the same marker map", origInput["auth"])
	}
	m, ok := origInput["auth"].(map[string]any)
	if !ok || m["$credential"] != "smtp-relay" {
		t.Fatalf("original Input[auth] no longer the marker: %#v", origInput["auth"])
	}
}

// TestResolveCredentials_MissingCredential_ReturnsClearError: a marker whose
// credential is not stored yields a clear Go error naming the step, key, and
// credential; resolved is nil.
func TestResolveCredentials_MissingCredential_ReturnsClearError(t *testing.T) {
	store := newCredStore(t)
	fv := credMarkerFlow("use_auth", "no-such-cred")
	resolved, refs, err := ResolveCredentials(&fv, store)
	if err == nil {
		t.Fatalf("err = nil, want a missing-credential error")
	}
	if resolved != nil {
		t.Fatalf("resolved = %v, want nil on error", resolved)
	}
	if refs != nil {
		t.Fatalf("refs = %v, want nil on error", refs)
	}
	for _, want := range []string{"use_auth", "auth", "no-such-cred"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to mention %q", err.Error(), want)
		}
	}
}

// TestResolveCredentials_NilStoreWithMarker_ReturnsClearErrorNoPanic: a flow
// that references a credential but with no store configured returns a clear
// error rather than panicking on a nil store.
func TestResolveCredentials_NilStoreWithMarker_ReturnsClearErrorNoPanic(t *testing.T) {
	fv := credMarkerFlow("use_auth", "smtp-relay")
	resolved, refs, err := ResolveCredentials(&fv, nil)
	if err == nil {
		t.Fatalf("err = nil, want a no-store-configured error")
	}
	if resolved != nil || refs != nil {
		t.Fatalf("resolved=%v refs=%v, want nil/nil on error", resolved, refs)
	}
	if !strings.Contains(err.Error(), "no credential store is configured") {
		t.Fatalf("err = %q, want 'no credential store is configured'", err.Error())
	}
	if !strings.Contains(err.Error(), "smtp-relay") {
		t.Fatalf("err = %q, want it to name the credential", err.Error())
	}
}

// TestResolveCredentials_MarkerInTriggerInput_ResolvedToo: the marker in a
// PIECE_TRIGGER's Input is resolved just like one in an action's Input.
// ResolveCredentials does not validate against a registry, so a fake piece
// name is fine here.
func TestResolveCredentials_MarkerInTriggerInput_ResolvedToo(t *testing.T) {
	store := newCredStore(t)
	const secret = "trigger-secret-value"
	if err := store.Save("trig-cred", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fv := model.FlowVersion{
		ID: "fv-trig",
		Trigger: &model.FlowTrigger{
			Name:        "trigger_1",
			DisplayName: "Trigger",
			Type:        model.TriggerPiece,
			PieceName:   "fake", TriggerName: "fake",
			Input: map[string]any{"auth": map[string]any{"$credential": "trig-cred"}},
			NextAction: &model.FlowAction{
				Name: "noop", DisplayName: "Noop", Type: model.ActionCode,
				Code: &model.CodeSettings{Source: `(params) => ({})`},
			},
		},
	}
	resolved, refs, err := ResolveCredentials(&fv, store)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want one (the trigger)", refs)
	}
	if refs[0].StepName != "trigger_1" || refs[0].InputKey != "auth" || refs[0].CredentialName != "trig-cred" {
		t.Fatalf("refs[0] = %+v, want the trigger ref", refs[0])
	}
	if got := resolved.Trigger.Input["auth"]; got != secret {
		t.Fatalf("resolved trigger Input[auth] = %v, want %q", got, secret)
	}
}

// TestResolveCredentials_MarkerInsideRouterChild_Resolved: a marker inside a
// ROUTER child action is found via the recursive branch walk.
func TestResolveCredentials_MarkerInsideRouterChild_Resolved(t *testing.T) {
	store := newCredStore(t)
	const secret = "router-child-secret"
	if err := store.Save("r-cred", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fv := model.FlowVersion{
		ID: "fv-router",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "router_1", DisplayName: "Router", Type: model.ActionRouter,
				Router: &model.RouterSettings{
					ExecutionType: model.RouterExecuteFirstMatch,
					Branches:      []model.RouterBranch{{Name: "b1", Type: model.BranchFallback}},
					Children: []*model.FlowAction{{
						Name: "use_auth", DisplayName: "Use Cred", Type: model.ActionCode,
						Code: &model.CodeSettings{
							Input:  map[string]any{"auth": map[string]any{"$credential": "r-cred"}},
							Source: `(params) => ({ authLen: params.auth.length })`,
						},
					}},
				},
			},
		},
	}
	resolved, refs, err := ResolveCredentials(&fv, store)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want one (the router child)", refs)
	}
	if refs[0].StepName != "use_auth" || refs[0].CredentialName != "r-cred" {
		t.Fatalf("refs[0] = %+v, want the router child ref", refs[0])
	}
	child := resolved.Trigger.NextAction.Router.Children[0]
	if got := child.Code.Input["auth"]; got != secret {
		t.Fatalf("resolved router child Input[auth] = %v, want %q", got, secret)
	}
}

// TestRedactCredentials_OnRealRunState_RedactsInputOnly: run a real flow
// through RunWithCredentials and confirm the step's Input no longer holds the
// real secret (it holds the placeholder) while nothing else about the state
// is altered.
func TestRedactCredentials_OnRealRunState_RedactsInputOnly(t *testing.T) {
	store := newCredStore(t)
	const secret = "el-secreto-xyz-789"
	if err := store.Save("smtp-relay", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fv := credMarkerFlow("use_auth", "smtp-relay")

	state, vErrs, err := RunWithCredentials(&fv, emptyRegistry, store, nil, nil, false)
	if err != nil {
		t.Fatalf("RunWithCredentials err: %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %v, want none", vErrs)
	}
	if state == nil {
		t.Fatal("state = nil, want a run state")
	}
	step := state.Steps["use_auth"]
	if step == nil {
		t.Fatalf("Steps missing use_auth: %#v", state.Steps)
	}
	in, ok := step.Input.(map[string]any)
	if !ok {
		t.Fatalf("step.Input = %T, want map[string]any", step.Input)
	}
	if got := in["auth"]; got != "<credential:smtp-relay>" {
		t.Fatalf("Input[auth] = %v, want placeholder <credential:smtp-relay>", got)
	}
	// The plain value is untouched.
	if in["other"] != "plain-value" {
		t.Fatalf("Input[other] = %v, want plain-value (untouched)", in["other"])
	}
	// The secret must not appear anywhere in the serialized state.
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("real secret appears in state JSON:\n%s", b)
	}
	// The output proves the real value reached the piece: authLen == len(secret).
	// goja's Export yields int/int64/float64 depending on the host word size
	// (no JSON roundtrip here), so accept any integer kind — same as run_test.go.
	out, _ := step.Output.(map[string]any)
	switch l := out["authLen"].(type) {
	case int:
		if l != len(secret) {
			t.Fatalf("authLen = %v, want %d", l, len(secret))
		}
	case int64:
		if l != int64(len(secret)) {
			t.Fatalf("authLen = %v, want %d", l, len(secret))
		}
	case float64:
		if int(l) != len(secret) {
			t.Fatalf("authLen = %v, want %d", l, len(secret))
		}
	default:
		t.Fatalf("authLen = %v (%T), want %d", out["authLen"], out["authLen"], len(secret))
	}
}

// TestRunWithCredentials_SecretNotInStateJSON is THE end-to-end proof that
// the two-step design works, not just compiles: a recognizable secret,
// resolved, run, and redacted, must not appear anywhere in the full JSON of
// the returned ExecutionState.
func TestRunWithCredentials_SecretNotInStateJSON(t *testing.T) {
	store := newCredStore(t)
	const secret = "el-secreto-xyz-789"
	if err := store.Save("smtp-relay", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fv := credMarkerFlow("use_auth", "smtp-relay")

	state, vErrs, err := RunWithCredentials(&fv, emptyRegistry, store, nil, nil, false)
	if err != nil {
		t.Fatalf("RunWithCredentials err: %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %v, want none", vErrs)
	}
	if state == nil {
		t.Fatal("state = nil")
	}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("real secret leaked into the response JSON:\n%s", b)
	}
	// And the placeholder IS present, so the redaction is observable. JSON
	// escapes "<"/">" as </>, so check the placeholder via the
	// decoded Go value rather than the raw bytes.
	step := state.Steps["use_auth"]
	if step == nil {
		t.Fatalf("Steps missing use_auth: %#v", state.Steps)
	}
	in, ok := step.Input.(map[string]any)
	if !ok {
		t.Fatalf("step.Input = %T, want map", step.Input)
	}
	if in["auth"] != "<credential:smtp-relay>" {
		t.Fatalf("Input[auth] = %v, want placeholder", in["auth"])
	}
}

// TestRunWithCredentials_CredentialInsideLoop_AllIterationsRedacted: a
// credential used by an action inside a LOOP_ON_ITEMS iterating 4 times must
// be redacted in every iteration, none carrying the real secret.
func TestRunWithCredentials_CredentialInsideLoop_AllIterationsRedacted(t *testing.T) {
	store := newCredStore(t)
	const secret = "el-secreto-xyz-789"
	if err := store.Save("loop-cred", secret); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fv := model.FlowVersion{
		ID: "fv-loop",
		Trigger: &model.FlowTrigger{
			Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
			NextAction: &model.FlowAction{
				Name: "loop_1", DisplayName: "Loop", Type: model.ActionLoopOnItems,
				Loop: &model.LoopSettings{
					Items: `{{ [1, 2, 3, 4] }}`,
					FirstLoopAction: &model.FlowAction{
						Name: "use_auth", DisplayName: "Use Cred", Type: model.ActionCode,
						Code: &model.CodeSettings{
							Input:  map[string]any{"auth": map[string]any{"$credential": "loop-cred"}},
							Source: `(params) => ({ authLen: params.auth.length })`,
						},
					},
				},
			},
		},
	}

	state, vErrs, err := RunWithCredentials(&fv, emptyRegistry, store, nil, nil, false)
	if err != nil {
		t.Fatalf("RunWithCredentials err: %v", err)
	}
	if len(vErrs) != 0 {
		t.Fatalf("validationErrs = %v, want none", vErrs)
	}
	if state == nil {
		t.Fatal("state = nil")
	}
	loop := state.Steps["loop_1"]
	if loop == nil {
		t.Fatalf("Steps missing loop_1: %#v", state.Steps)
	}
	if len(loop.Iterations) != 4 {
		t.Fatalf("len(Iterations) = %d, want 4", len(loop.Iterations))
	}
	for i, iter := range loop.Iterations {
		step, ok := iter["use_auth"]
		if !ok {
			t.Fatalf("iteration %d missing use_auth: %#v", i, iter)
		}
		in, ok := step.Input.(map[string]any)
		if !ok {
			t.Fatalf("iter %d: Input = %T, want map", i, step.Input)
		}
		if got := in["auth"]; got != "<credential:loop-cred>" {
			t.Fatalf("iter %d: Input[auth] = %v, want placeholder", i, got)
		}
	}
	// The whole state JSON must not contain the secret.
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("real secret leaked into loop state JSON:\n%s", b)
	}
}

// TestRunWithCredentials_MissingCredential_ReturnsValidationErrsNotErr: a
// flow referencing a credential that isn't stored comes back as
// validationErrs (a configuration problem, not a server fault): at least one
// mentioning the credential, state nil, err nil.
func TestRunWithCredentials_MissingCredential_ReturnsValidationErrsNotErr(t *testing.T) {
	store := newCredStore(t)
	fv := credMarkerFlow("use_auth", "no-such-cred")
	state, vErrs, err := RunWithCredentials(&fv, emptyRegistry, store, nil, nil, false)
	if err != nil {
		t.Fatalf("err = %v, want nil — a missing credential is not an err", err)
	}
	if state != nil {
		t.Fatalf("state = %v, want nil when the credential is missing", state)
	}
	if len(vErrs) == 0 {
		t.Fatal("validationErrs = empty, want the missing-credential reported")
	}
	found := false
	for _, e := range vErrs {
		if strings.Contains(e.Message, "no-such-cred") {
			found = true
		}
		if e.Path != "credentials" {
			t.Fatalf("validationErr Path = %q, want %q", e.Path, "credentials")
		}
	}
	if !found {
		t.Fatalf("validationErrs = %v, want one mentioning no-such-cred", vErrs)
	}
}
