// Package piece defines goflow's native piece SDK — a from-scratch Go
// interface, NOT compatible with activepieces' TypeScript piece packages
// (that incompatibility is the whole reason a Go rewrite means abandoning
// activepieces' existing piece ecosystem; see the engine's own README).
package piece

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"goflow/pkg/model"
)

// ApFile is a file value — the Go analogue of activepieces' ApFile class.
// Used both ways: as an Input value an action reads (an "attachment"), and
// as what FileWriter.Write accepts to produce one.
type ApFile struct {
	Filename  string
	Data      []byte
	Extension string
}

func (f *ApFile) Base64() string {
	return base64.StdEncoding.EncodeToString(f.Data)
}

// AuthInputKey is the reserved Input key an action's auth value is passed
// under — mirrors activepieces' AUTHENTICATION_PROPERTY_NAME ('auth'): auth
// is not a separate channel, just a normal (often {{ }}-templated) input
// field the engine additionally surfaces as ActionContext.Auth for
// convenience. The engine does not require it to be present or non-empty —
// same as activepieces (props-processor.ts only conditionally validates an
// auth sub-object if one was actually supplied); a piece whose action needs
// auth is responsible for checking ctx.Auth itself and failing clearly if
// it's missing.
const AuthInputKey = "auth"

// OAuth2Auth is the Go analogue of activepieces' OAuth2PropertyValue — a
// convenience TYPE ONLY, not an engine-enforced shape: ActionContext.Auth
// stays `any`, exactly like every other auth value (see AuthInputKey). A
// piece requiring OAuth2 auth may use this to type-assert ctx.Auth into
// something with named fields instead of digging through a map[string]any.
//
// There is no OAuth2 token refresh here, matching activepieces itself:
// checked piece-helper.ts's executeRefreshTokenAuth before adding
// anything — for a standard OAuth2 connection (AppConnectionType !=
// CUSTOM_AUTH) it unconditionally returns {skipped: true}. Real OAuth2
// refresh happens in the activepieces SERVER (a direct HTTP call to the
// provider's token endpoint with grant_type=refresh_token, outside
// packages/server/engine entirely) — not something the engine package
// does even in the system this is modeled on. Same "not this engine's job"
// boundary as flow timeout, polling scheduling, and action-run fan-out.
type OAuth2Auth struct {
	AccessToken string
	// Data holds whatever else the provider's token response included —
	// refresh_token, expires_in, token_type, etc. — deliberately untyped
	// since providers vary (mirrors OAuth2PropertyValue.data: Record<string, any>).
	Data map[string]any
	// Props holds the piece-defined extra OAuth2 config values (client_id
	// equivalents, tenant IDs, etc. — whatever OAuth2Props declared).
	Props map[string]any
}

// ActionContext is what an Action's Run function receives.
type ActionContext struct {
	ExecutionType model.ExecutionType
	Input         map[string]any
	Auth          any // Input[AuthInputKey], if present — see AuthInputKey
	ResumePayload any // set only when ExecutionType == model.ExecutionResume
	Files         FileWriter

	// Store lets an action remember state across separate flow runs — the
	// action-side counterpart to TriggerContext.Store (same interface, same
	// "nil unless the caller supplies one" rule for a Piece.Run invoked
	// directly). Unlike TriggerContext.Store, *Engine DOES wire this one in
	// (Engine.Store, defaulting to a fresh MemoryStore in New()) for every
	// PIECE action it runs, through both ExecuteBegin and
	// ExecuteActionRun — an action's cross-run memory doesn't depend on an
	// external scheduler the way a polling trigger's does, so there's no
	// reason to leave it for the caller to wire by hand. Not scoped per flow
	// by default: two flows sharing one Engine share one Store, same as
	// Engine.Files — wrap Engine.Store in a *ScopedStore (or use one Engine
	// per tenant) if that matters, exactly the pattern
	// TestMultipleFlowsSameTrigger_ScopedStoreIsolatesCursors and
	// TestMultiTenancy_SeparateEnginesFullyIsolated already prove for
	// triggers and Files.
	Store Store

	// Run exposes the hooks a Piece action calls to affect the surrounding
	// flow run — pause it, or reply synchronously to whatever triggered it
	// over a webhook. Mirrors activepieces' context.run.{waitForWaitpoint,
	// stop, respond}.
	Run RunHooks
}

type RunHooks struct {
	// WaitForWaitpoint pauses the flow — activepieces' context.run.waitForWaitpoint(id).
	// Calling it marks the step PAUSED; the engine stops walking nextAction
	// after this step returns.
	WaitForWaitpoint func(waitpointID string)

	// Stop ends the run right here with SUCCEEDED and resp as the synchronous
	// webhook reply — activepieces' context.run.stop({response}). Whatever
	// this action's own Run return value is still gets recorded as the
	// step's Output; NextAction never runs.
	Stop func(resp *model.WebhookResponse)

	// Respond sends resp as the synchronous webhook reply WITHOUT ending the
	// run — activepieces' context.run.respond({response}): the flow keeps
	// executing NextAction afterward. See model.ExecutionState.RespondedEarly's
	// doc comment for why there's no simulated delivery, just an observable
	// marker of when this fired.
	Respond func(resp *model.WebhookResponse)
}

// Action is one invocable operation a Piece exposes.
type Action struct {
	Name        string
	DisplayName string
	Run         func(ctx ActionContext) (any, error)

	// Dropdowns declares this action's dynamically-loaded properties, keyed
	// by property name — activepieces' DropdownProperty.options, invoked
	// through Engine.LoadOptions rather than Run: loading a dropdown's
	// choices is a config-time (editor) operation, never part of running the
	// flow. Optional; most actions have none.
	Dropdowns map[string]DropdownProperty
}

// DropdownOption is one selectable choice — activepieces' DropdownOption{label, value}.
type DropdownOption struct {
	Label string
	Value any
}

// DropdownState is what LoadOptions returns — activepieces' DropdownState.
type DropdownState struct {
	Disabled    bool
	Placeholder string
	Options     []DropdownOption
}

// PropertyContext is what a dropdown's LoadOptions receives alongside the
// sibling input values — reduced from activepieces' PropertyContext (which
// also carries server/project/connections/flows context) to what this
// engine actually has: nothing beyond the search term, since there's no
// server/connections/flows infrastructure here at all (see "explicitly NOT
// in v1"). Auth is not part of this — it travels inside propsValue under
// AuthInputKey, same convention as ActionContext.Auth, since activepieces'
// own DynamicDropdownOptions type folds auth into propsValue rather than ctx.
type PropertyContext struct {
	SearchValue string
}

// DropdownProperty is one dynamically-loaded input property on an Action.
// Refreshers names the sibling input properties this dropdown depends on —
// purely documentation here (nothing enforces it); LoadOptions itself
// decides what it actually reads from propsValue.
type DropdownProperty struct {
	Refreshers  []string
	LoadOptions func(propsValue map[string]any, ctx PropertyContext) (DropdownState, error)
}

// FileWriter is what an action's Run uploads produced file data through —
// the Go analogue of activepieces' context.files (createFileUploader):
// write() only, no read — a piece uploads a file and gets back a reference
// to hand off to whatever it's talking to (or to the flow's own output);
// nothing reads it back through the engine. There is no equivalent to
// activepieces' AP_MAX_FILE_SIZE_MB cap or streaming upload (Buffer/Readable)
// — v1 accepts a plain []byte and has no size limit.
type FileWriter interface {
	Write(fileName string, data []byte) (url string, err error)
}

// MemoryFileWriter is the default FileWriter: keeps uploaded bytes in
// memory and hands back a "memfile://" reference, instead of activepieces'
// real engineFileApi.upload (there is no file storage server here — see
// "explicitly NOT in v1"). Get is a test/inspection hook only; it's not
// exposed to piece code (ActionContext.Files is typed as the narrower
// FileWriter interface, write-only, matching activepieces).
type MemoryFileWriter struct {
	mu      sync.Mutex
	counter int
	files   map[string][]byte
}

func NewMemoryFileWriter() *MemoryFileWriter {
	return &MemoryFileWriter{files: map[string][]byte{}}
}

func (w *MemoryFileWriter) Write(fileName string, data []byte) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.counter++
	id := fmt.Sprintf("f%d", w.counter)
	w.files[id] = data
	return fmt.Sprintf("memfile://%s/%s", id, fileName), nil
}

// Get looks up previously written bytes by the exact URL Write returned —
// test/inspection only, not part of FileWriter.
func (w *MemoryFileWriter) Get(url string) ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, data := range w.files {
		if strings.HasPrefix(url, "memfile://"+id+"/") {
			return data, true
		}
	}
	return nil, false
}

// Store is what a trigger's Run reads/writes to remember state across
// separate invocations — mirrors activepieces' context.store, minus the
// server-side persistence (this is in-memory only; the caller decides how
// long an instance lives). A POLLING-style trigger's whole job is comparing
// freshly-fetched data against a cursor kept here to figure out what's new.
type Store interface {
	Get(key string) (any, bool)
	Put(key string, value any)
}

// MemoryStore is the default Store: a plain, mutex-guarded map. Reuse the
// SAME instance across repeated Trigger.Run calls to simulate a polling
// scheduler invoking the trigger on a timer — a fresh MemoryStore per call
// would defeat the entire point (every poll would see no cursor and
// rediscover everything as "new").
type MemoryStore struct {
	mu   sync.Mutex
	data map[string]any
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: map[string]any{}}
}

func (s *MemoryStore) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *MemoryStore) Put(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// ScopedStore wraps an underlying Store, namespacing every key by FlowID —
// the Go analogue of activepieces' createContextStore's key construction for
// StoreScope.FLOW (its default scope): 'flow_' + flowId + '/' + key. Without
// this, two different flows using the exact same trigger — a very ordinary
// setup (e.g. the same Slack channel triggering two independent flows) —
// and the exact same literal key (e.g. "last_id", which most polling
// triggers would naturally use) would silently clash if they share one
// underlying Store: whichever flow polls second sees the first flow's
// cursor and wrongly thinks its own already-seen items are new, or worse,
// misses items it hasn't actually processed yet. Wrap any shared backend
// (activepieces has exactly one: a single store-entries API serving every
// flow in the project) with a *ScopedStore per flow to avoid that.
//
// Simplified vs. activepieces: no per-call scope parameter (its Get/Put/
// Delete take an optional StoreScope, defaulting to FLOW) — ScopedStore only
// ever provides FLOW scope. For the rarer intentionally-shared case
// (activepieces' StoreScope.PROJECT), use the underlying Store directly
// instead of wrapping it — an unprefixed key IS project scope.
type ScopedStore struct {
	Underlying Store
	FlowID     string
}

func (s *ScopedStore) scopedKey(key string) string {
	return "flow_" + s.FlowID + "/" + key
}

func (s *ScopedStore) Get(key string) (any, bool) {
	return s.Underlying.Get(s.scopedKey(key))
}

func (s *ScopedStore) Put(key string, value any) {
	s.Underlying.Put(s.scopedKey(key), value)
}

// TriggerContext is what a Trigger's Run function receives.
type TriggerContext struct {
	Payload any
	Input   map[string]any
	Store   Store // nil unless the caller supplies one — see Store's doc comment
}

// Trigger is a real (non-EMPTY) flow entry point: Run can transform the raw
// event payload, unlike model.TriggerEmpty which passes it through untouched.
//
// There is no TriggerStrategy field (activepieces has WEBHOOK/POLLING/MANUAL/
// APP_WEBHOOK) — every strategy shares the identical Run(ctx) ([]any, error)
// signature, and the engine never branches on which one a trigger is. What
// makes a trigger "a polling trigger" is entirely a convention of how it's
// invoked and what it does with Store, not an engine-level concept: an
// external scheduler (activepieces' worker; not part of this project — see
// "explicitly NOT in v1") calls Run() on a timer, and Run() uses Store to
// return only items newer than what it saw last time. See
// TestPollingTrigger_* for the whole shape simulated end-to-end.
type Trigger struct {
	Name        string
	DisplayName string
	Run         func(ctx TriggerContext) ([]any, error)
}

// Piece is a named bundle of actions and triggers.
type Piece struct {
	Name        string
	DisplayName string
	Actions     map[string]Action
	Triggers    map[string]Trigger
}

// Registry resolves piece names to Piece definitions — the Go analogue of
// activepieces' piece-loader.ts, but a plain in-memory map instead of dynamic
// filesystem/npm module resolution (there is no piece marketplace here).
//
// Mutex-guarded: an *Engine is meant to be shared across concurrent
// ExecuteBegin/ExecuteActionRun calls (real goroutine parallelism — the
// actual "why Go" this whole project exists to test), and every one of them
// reads through this Registry. Without the RWMutex, concurrent GetAction/
// GetTrigger calls racing a Register (or each other, since Go maps aren't
// safe for concurrent access even read-only-vs-read-only is fine but
// read-vs-write isn't) would be a real data race — this was unverified
// until TestConcurrentFlowRuns_* actually ran multiple ExecuteBegin calls
// in parallel under `go test -race` and would have caught it.
type Registry struct {
	mu     sync.RWMutex
	pieces map[string]Piece
}

func NewRegistry() *Registry {
	return &Registry{pieces: map[string]Piece{}}
}

func (r *Registry) Register(p Piece) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pieces[p.Name] = p
}

func (r *Registry) GetAction(pieceName, actionName string) (Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pieces[pieceName]
	if !ok {
		return Action{}, false
	}
	a, ok := p.Actions[actionName]
	return a, ok
}

func (r *Registry) GetTrigger(pieceName, triggerName string) (Trigger, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pieces[pieceName]
	if !ok {
		return Trigger{}, false
	}
	t, ok := p.Triggers[triggerName]
	return t, ok
}
