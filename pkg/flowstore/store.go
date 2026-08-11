// Package flowstore persists named flows — the first step toward exposing
// them later as MCP tools. A flow today can only be run ad-hoc via
// POST /flows/run (the whole FlowVersion comes in the body, runs, and is
// forgotten); this package adds the missing half: save a FlowVersion with a
// name/description, then fetch and run it by name later.
//
// The on-disk shape mirrors pkg/catalog and pkg/credentials on purpose: one
// JSON file per flow, atomic write (CreateTemp in the same dir + Rename),
// and the same path-traversal sanitization on the name. Only flowstore adds
// a GatedStore that runs flowvalidate.Validate against the piece registry AS
// IT IS at save time — a saved flow must validate against the catalog of
// pieces that exists right now, including JS pieces added after the process
// started. The package uses nothing but the standard library (encoding/json,
// errors, fmt, os, path/filepath, strings) — no new dependency, matching the
// rest of this project's stance.
package flowstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"goflow/pkg/model"
)

// ErrNotFound is returned by Delete when no flow file exists for the given
// name. Callers (the HTTP layer) map it to 404 without inspecting the raw os
// error. Get does NOT use it: a missing name is the (zero, false, nil) case,
// matching catalog.FileStore.Get and credentials.FileStore.Get.
var ErrNotFound = errors.New("flowstore: not found")

// FlowDefinition is one named, persisted flow — a FlowVersion plus the
// human/agent-facing metadata that makes it discoverable. Name is the
// on-disk key (and the HTTP path segment); DisplayName/Description/InputSchema
// are what a future tools/list surface shows so a caller can pick a flow
// without reading its full FlowVersion.
type FlowDefinition struct {
	Name        string
	DisplayName string
	Description string

	// InputSchema is free-text describing the trigger payload this flow
	// expects (e.g. "n (number, required) — doubled by the first step") —
	// deliberately not a formal JSON Schema for v1, same reasoning as
	// catalog.ActionDefinition.InputSchema: a flow's trigger payload is
	// untyped (model.FlowTrigger.Input is map[string]any, and an EMPTY
	// trigger passes whatever BeginInput.TriggerPayload carries straight
	// through) with no engine-enforced shape to derive a real schema from,
	// so a hand-written description is what's actually available; a formal
	// schema would need the flow author to also author (and keep in sync
	// with) a separate machine-checked contract, which is real added scope
	// this package doesn't take on.
	InputSchema string

	// Flow is the actual flow definition — the same model.FlowVersion
	// POST /flows/run already accepts inline. Persisted verbatim so a run
	// by name runs exactly what was saved.
	Flow model.FlowVersion

	// WebhookEnabled opts this flow into POST /webhooks/{name} — an
	// unauthenticated (no GOFLOW_API_TOKEN) ingress route a third party
	// (Stripe, GitHub, ...) can call directly, since it has no way to know
	// that token. Off by default: saving a flow must never silently create
	// a publicly-triggerable endpoint. See pkg/httpapi's webhook handler for
	// the full contract.
	WebhookEnabled bool

	// WebhookSecretCredential, if set, names a credential in the
	// credentials store (pkg/credentials) whose value POST /webhooks/{name}
	// compares, constant-time, against the request's X-Webhook-Secret
	// header. Deliberately a credential REFERENCE, not a plain string field
	// here — a plain field would put the secret in this store's own
	// plaintext JSON file, exactly what pkg/credentials exists to avoid.
	// This is a coarse floor for providers that don't sign their payloads;
	// a provider that does (GitHub's X-Hub-Signature-256, Stripe-Signature,
	// ...) should still verify that signature itself inside the flow, using
	// the hash piece's HMAC support — see README's webhook-ingress entry.
	WebhookSecretCredential string

	// OnFailureFlow, if set, names ANOTHER saved flow to run whenever this
	// one's Verdict.Status ends up FAILED — a webhook- or scheduler-fired
	// run has no human watching it in real time, so without this the only
	// way to learn a run failed is polling GET /runs. Empty (the default)
	// disables it entirely: saving a flow must never silently start firing
	// another one. See TriggerOnFailure for the exact contract (what the
	// on-failure flow receives as its trigger payload, and why it can
	// never chain more than one hop deep).
	OnFailureFlow string

	// OnPauseFlow, if set, names ANOTHER saved flow to run whenever this
	// one pauses (Verdict.Status PAUSED — e.g. the "approval" piece
	// calling ctx.Run.WaitForWaitpoint). Mirrors OnFailureFlow's own
	// reasoning: a webhook- or scheduler-fired run that pauses has no
	// human watching it, so without this the only way to learn it's
	// waiting is polling GET /runs. Empty (the default) disables it
	// entirely. Unlike OnFailureFlow, the on-pause flow's trigger payload
	// carries what's needed to actually ACT on the pause — the paused
	// run's own id and its runstore.Record.ResumeToken — so a flow using
	// this can build a one-click approval link (POST
	// /public/runs/{runId}/resume with X-Resume-Secret: {resumeToken},
	// no GOFLOW_API_TOKEN required) and email/notify it. See the
	// unexported triggerOnPause for the exact contract and why it can
	// never chain more than one hop deep, same as OnFailureFlow.
	OnPauseFlow string

	// Examples are worked trigger/expected-result cases GatedStore.Save
	// actually RUNS against def.Flow before persisting — the flow-level
	// analogue of catalog.ActionDefinition.Examples. Unlike a piece
	// example (one action, one return value), a flow example naturally
	// has more than one place a result could matter, so WantStepOutputs
	// lets the author pick WHICH step(s) to assert on by name, rather
	// than compare the whole Steps map (which would also drag in
	// per-step noise like DurationMs that varies every run).
	//
	// Deliberately OPTIONAL, unlike catalog pieces (which require at
	// least one Example): every flow saved before this field existed —
	// every flow saved in production today — has none, and requiring
	// them now would reject flows that always saved fine. Empty (the
	// default) means no gate at all, identical to this field never
	// having existed. See ValidateExamples for the exact run contract
	// (structural validation must already pass before any example runs;
	// examples get no credentials.Store and no CALL_FLOW support).
	Examples []FlowExample
}

// FlowExample is one worked case for FlowDefinition.Examples.
type FlowExample struct {
	Description string

	// Trigger is the trigger payload this example runs with — the same
	// shape POST /flows/{name}/run's own "trigger" field accepts.
	Trigger any
	// ExecuteTrigger mirrors POST /flows/{name}/run's own field: false
	// (the default) passes Trigger straight through as the trigger
	// step's output; true actually invokes a PIECE_TRIGGER's Run hook.
	ExecuteTrigger bool

	// WantError, if true, means this example is expected to end with a
	// FAILED verdict — proving a flow correctly REJECTS certain input,
	// the same "prove the failure path too" idea catalog.Example.WantError
	// already covers for a single action.
	WantError bool

	// CheckOutputs, if true, requires every key in WantStepOutputs to
	// match that step's actual Output (deep JSON equality, same
	// comparison catalog.Example.CheckOutput already uses) — a step name
	// missing from the run's Steps, or a value that doesn't match, is a
	// failure. Ignored when WantError is true (a failed run's steps
	// aren't the point of that example).
	CheckOutputs    bool
	WantStepOutputs map[string]any
}

// Store is the surface a flow store exposes: Save persists a FlowDefinition
// keyed by its Name; Get returns one by name (missing -> (zero, false, nil),
// not an error); List returns every saved definition; Delete removes one
// (missing -> ErrNotFound, so the HTTP layer can map to 404 without
// inspecting the raw os error).
type Store interface {
	Save(def FlowDefinition) error
	Get(name string) (FlowDefinition, bool, error)
	List() ([]FlowDefinition, error)
	Delete(name string) error
}

// FileStore is a Store backed by one JSON file per flow, in a directory on
// disk — real persistence across process restarts, using nothing but
// encoding/json and os (no new dependency, matching pkg/catalog.FileStore and
// pkg/credentials.FileStore).
type FileStore struct {
	dir string
}

// NewFileStore returns a FileStore rooted at dir, creating it if it doesn't
// already exist (0o755, same as catalog.FileStore and credentials.FileStore).
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("flowstore: creating store directory: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// path validates name and returns the on-disk file it maps to. A
// FlowDefinition's Name may be agent-authored — never trust it as a raw path
// segment. Same rejection criteria as catalog.FileStore.path and
// credentials.FileStore.path: reject "", any "/" or "\", "." and "..", rather
// than trying to "clean" a traversal attempt. The validation happens before
// any filesystem touch.
func (s *FileStore) path(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("flowstore: flow name is empty")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("flowstore: invalid flow name %q", name)
	}
	return filepath.Join(s.dir, name+".json"), nil
}

// Save encodes def and writes it atomically (CreateTemp in the same dir +
// Rename, same pattern as catalog.FileStore.Save, with cleanup at every
// failure point and 0o644 before the rename). A reader (or a crash) only
// ever sees the previous version or the fully-written new one.
func (s *FileStore) Save(def FlowDefinition) error {
	p, err := s.path(def.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return fmt.Errorf("flowstore: encoding %q: %w", def.Name, err)
	}

	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("flowstore: creating temp file for %q: %w", def.Name, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("flowstore: writing %q: %w", def.Name, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("flowstore: closing temp file for %q: %w", def.Name, err)
	}
	// os.CreateTemp uses 0o600; match the 0o644 mode catalog/credentials use,
	// since os.Rename carries the source's mode over.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("flowstore: setting mode for %q: %w", def.Name, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		cleanup()
		return fmt.Errorf("flowstore: renaming %q into place: %w", def.Name, err)
	}
	return nil
}

// Get returns the FlowDefinition for name. If the file does not exist it
// returns (zero, false, nil) — not an error — matching catalog.FileStore.Get
// and credentials.FileStore.Get.
func (s *FileStore) Get(name string) (FlowDefinition, bool, error) {
	p, err := s.path(name)
	if err != nil {
		return FlowDefinition{}, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return FlowDefinition{}, false, nil
		}
		return FlowDefinition{}, false, fmt.Errorf("flowstore: reading %q: %w", name, err)
	}
	var def FlowDefinition
	if err := json.Unmarshal(data, &def); err != nil {
		return FlowDefinition{}, false, fmt.Errorf("flowstore: decoding %q: %w", name, err)
	}
	return def, true, nil
}

// List returns every saved FlowDefinition, reading each .json file in the
// store directory. It returns whatever it finds decoded; callers that only
// need names/metadata project what they want from the result (the HTTP layer
// keeps GET /flows light by projecting Name/DisplayName/Description).
func (s *FileStore) List() ([]FlowDefinition, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("flowstore: listing store directory: %w", err)
	}
	defs := make([]FlowDefinition, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("flowstore: reading %q: %w", e.Name(), err)
		}
		var def FlowDefinition
		if err := json.Unmarshal(data, &def); err != nil {
			return nil, fmt.Errorf("flowstore: decoding %q: %w", e.Name(), err)
		}
		defs = append(defs, def)
	}
	return defs, nil
}

// Delete removes the flow file for name. If no such file exists it returns
// ErrNotFound (not the raw os.IsNotExist) so the HTTP layer can map it to
// 404 without inspecting the underlying error — same shape as
// credentials.FileStore.Delete.
func (s *FileStore) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("flowstore: deleting %q: %w", name, err)
	}
	return nil
}
