# goflow

A from-scratch automation-flow engine in Go — **not** a port of activepieces,
not compatible with activepieces' piece ecosystem or flow format. Built after
evaluating a literal "activepieces engine in Go" rewrite and concluding it
wasn't viable: activepieces' ~300+ pieces are TypeScript/JS packages that run
in a JS engine no matter what language orchestrates them, and its user "Code"
step needs a JS sandbox regardless. Porting the actual value (the piece
ecosystem) to Go would mean embedding a JS runtime anyway or rewriting every
piece by hand — neither is "the engine in Go" anymore. This project instead
asks: if you're building a *new*, Go-native automation engine and accept
losing that ecosystem, what does that look like? See "Design decisions"
below for what a Go rewrite gets and gives up.

## What's here (v1 scope)

- **Flow execution engine** (`pkg/engine`): trigger → chained actions
  (`NextAction`), `ROUTER` (branches + fallback, first-match/all-match),
  `LOOP_ON_ITEMS`, retry with exponential backoff, `ContinueOnFailure`,
  pause/resume via a waitpoint hook — including resuming a specific paused
  loop iteration in place, not restarting the whole loop — and a
  `LOG_SIZE_EXCEEDED` run limit. `*Engine` and a shared `*model.FlowVersion`
  are both safe to use from multiple goroutines concurrently — verified
  under `go test -race`, not just assumed — since each `ExecuteBegin`/
  `ExecuteActionRun` call allocates its own fresh `*model.ExecutionState`
  and nothing touches an action's `Input` map except to read it.
- **JSON flow definitions** (`pkg/model/json.go`, `model.ParseFlowVersion`):
  every flow-definition type (`FlowVersion` down to `Condition`) carries a
  `json` tag, so a flow can be authored as data — by a human, or an AI
  agent — instead of only as Go struct literals. `Input` values decode as
  whatever `encoding/json` produces for arbitrary JSON; there's no JSON
  representation for `*piece.OAuth2Auth`, but a plain string works for every
  other secret a catalog piece needs (`http`'s Authorization header,
  `crypto`'s and `hash`'s keys — the latter two were extended to accept a
  string alongside `[]byte` specifically so a JSON-defined flow can use
  them at all). Runtime/result types (`StepOutput`, `Verdict`,
  `ExecutionState`) deliberately have no tags — nothing has asked for those
  to be caller-authored data yet.
- **JS-authored pieces** (`pkg/jspiece`, `jspiece.New`/`NewAction`): Phase 2
  of the "AI-first" direction — an action can be authored as JS source
  (`(ctx) => value`, synchronous only, same rule as CODE steps) instead of
  Go code, and the resulting `piece.Piece` goes through the exact same
  `piece.Validate`/`RegisterValidated` path as any hand-written Go piece —
  the engine cannot tell the difference. This is what makes a piece
  something an agent can create and use *at runtime, with no Go recompile*,
  closing the gap Phase 1 (JSON flows, above) explicitly left open. `ctx`
  gives JS near-parity with Go's `ActionContext`: `ctx.input`, `ctx.auth`
  (`*piece.OAuth2Auth` becomes `{accessToken, data, props}`, `[]byte`
  becomes a string), `ctx.executionType`, `ctx.resumePayload`,
  `ctx.files.write(fileName, content)`, `ctx.run.stop`/`respond`/
  `waitForWaitpoint`, and `ctx.fetch({url, method, headers, body})` for a
  real synchronous HTTP call — deliberately the one capability beyond pure
  logic and the `ActionContext` hooks (see "Design decisions" for the
  tradeoff). A 5s execution deadline (`goja.Runtime.Interrupt`, via
  `jspiece.DefaultTimeout`) bounds a runaway/infinite-loop action — CODE
  steps have no such limit, but that's human-reviewed code; a JS piece is
  explicitly meant for code an agent generates with no review gate.
  `jspiece.NewTrigger` does the same for `piece.Trigger`: `(ctx) => value`
  must return an ARRAY (rejected clearly otherwise), `ctx` exposes
  `ctx.payload`, `ctx.input`, and `ctx.store.get(key)`/`put(key, value)`
  bound to the real `piece.Store` the caller supplied — nil-guarded the
  same way `ctx.files`/`ctx.run` are, since `Engine.ExecuteBegin` never
  actually supplies one (confirmed by reading it, not assumed — see
  "Design decisions"). `jspiece.NewDropdown` does the same for
  `piece.DropdownProperty`: `(propsValue, ctx) => value` must return an
  object matching `piece.DropdownState`'s shape
  (`{disabled?, placeholder?, options: [{label, value}, ...]}`, rejected
  clearly otherwise), `ctx` exposes `ctx.searchValue`
  (`piece.PropertyContext.SearchValue`); wire one into an `ActionSource`
  via its new `Dropdowns map[string]DropdownSource` field, resolved
  through `Engine.LoadOptions` exactly like any Go piece's dropdown — same
  call path a real editor UI would use. Persistence — where a JS piece's
  source lives and how it survives a restart — is `pkg/catalog` (below),
  not this package, and only covers actions so far (`catalog.Definition`
  has no trigger or Dropdown equivalent yet).
- **Piece catalog persistence & discovery** (`pkg/catalog`): Phase 3 of the
  "AI-first" direction, closing the gap Phase 2 explicitly left open —
  `jspiece.New` builds a piece purely in memory, so a piece an agent
  creates dies with the process, and nothing let an agent check "does a
  piece for this already exist" before generating a near-duplicate one,
  which defeats the entire point of a *reused* catalog instead of one
  recreated every time. A `catalog.Definition` (name, description, and per
  action: description, a free-text `InputSchema`, and its JS source) is
  what a `Store` persists; `Definition.ToPiece()` turns it into a real
  `piece.Piece` via `jspiece.New`, and `RegisterFromStore` loads every
  Definition in a Store straight into an `Engine`'s registry. Two `Store`
  implementations, same "simple in-memory default, real persistence is
  the caller's choice" convention as `piece.Store`/`FileWriter`:
  `MemoryStore` (dies with the process, mainly for tests) and `FileStore`
  (one JSON file per piece in a directory — real, actual disk persistence
  across restarts, zero new dependencies). `catalog.Describe(store)`
  renders every piece's name/description/actions/input-schema as plain
  text meant to be handed directly to an agent's context before it
  decides whether to reuse something instead of authoring a piece again —
  deliberately plain text for a language model to read, not a search
  index or embedding lookup; a program that needs the data structurally
  should call `store.List()` directly. A quality gate now covers whether a
  Definition actually works (below); a *flow* that references a cataloged
  piece is checked before running by `pkg/flowvalidate` (below), the last
  of the four AI-first gaps.
- **Quality gate before saving** (`pkg/catalog/validate.go`,
  `GatedStore`): closes the other half of Phase 3 — a Definition could
  previously be saved (and later loaded and run for real) without ever
  having been shown to actually work. Each `ActionDefinition` now carries
  `Examples` (`Input`/`Auth` in, `WantError` or `CheckOutput`+`WantOutput`
  expected out); `Validate(def)` runs `piece.Validate` structurally AND
  actually **executes every example against the real JS-backed action**,
  same "run it for real, don't just check its shape" discipline as every
  test in this project. At least one Example per action is required — an
  action with none fails validation outright, there's no way to skip the
  check by omission. `GatedStore` wraps any `Store` and refuses `Save` on
  a validation failure before the underlying store is ever touched — same
  "wrap the raw op, offer the safe path as a decorator" shape as
  `piece.RegisterValidated` wrapping `Registry.Register` and
  `piece.ScopedStore` wrapping a shared `Store`. Output comparison goes
  through JSON encoding rather than `reflect.DeepEqual` specifically to
  tolerate goja's int64-vs-float64 export quirks (documented elsewhere in
  this project) instead of false-failing a correct piece over a numeric
  type mismatch that was never semantically meaningful.
  `TestValidate_OutputComparisonToleratesNumericTypeQuirks` proves this
  directly (`int64(42)` vs. `float64(42)` compares equal).
- **Flow validation** (`pkg/flowvalidate`): the fourth and last AI-first
  gap — a flow (especially one built entirely from Phase 1's JSON data,
  potentially agent-authored end to end) was never checked for structural
  soundness before its first real run, which matters once that run has
  real side effects. `Validate(fv, registry)` walks every reachable step
  (trigger → `NextAction` chain → each `ROUTER` branch's own chain → each
  `LOOP_ON_ITEMS` body's own chain) and reports, without stopping at the
  first problem: a `FlowActionType` whose matching settings field
  (`Code`/`Piece`/`Router`/`Loop`) is nil; a `PIECE` action or trigger
  referencing a piece/action that doesn't exist in `registry` (pass `nil`
  to skip this specific check); `Router.Children`/`Router.Branches` length
  mismatch; duplicate step names anywhere in the flow (silently clobber
  each other in `ExecutionState.Steps`, a real and easy mistake); and,
  most importantly, **a cycle in any `NextAction` chain** — confirmed by
  reading `Engine.executeChain`'s actual dispatch loop first (a plain
  `for action != nil { ...; action = action.NextAction }`, no cycle
  protection at all) rather than assuming: an author-introduced cycle
  would hang `ExecuteBegin` forever, with zero recovery. Every `{{ }}`
  expression found anywhere in `Input` maps, `Condition` values, and
  `LOOP_ON_ITEMS.Items` is checked for valid JS *syntax* via
  `goja.Compile` — confirmed via `go doc` to build an internal
  representation without executing anything, so this is safe against
  adversarial expressions the same way the other `Validate` functions in
  this project are. Deliberately NOT semantic: this can't and doesn't try
  to prove `{{ step_1.output.foo }}` will resolve correctly — that depends
  on `step_1`'s real output once it actually runs, and a bad reference
  already surfaces as a normal, clear `expr.Eval` error at that point;
  duplicating that check here would mean simulating execution or risking
  false positives on expressions this package genuinely can't evaluate
  ahead of time.
- **CODE step sandbox** (`pkg/sandbox`): runs user JS via
  [goja](https://github.com/dop251/goja) (pure Go, no cgo). Has its own
  direct unit tests (`sandbox_test.go`) — try/catch/finally, nested
  try/catch, rethrow, custom `Error` subclasses — verifying goja's JS
  fidelity for error handling, not just exercised indirectly through
  `pkg/engine`'s flow tests.
- **`{{ }}` templating** (`pkg/expr`): also built on goja — the trimmed
  expression is evaluated as real JS rather than reimplementing a JS-expression
  subset by hand (activepieces itself does the latter, with `jsep`).
- **Native piece SDK** (`pkg/piece`): a plain Go interface
  (`Action.Run(ctx) (any, error)`, `Trigger.Run(ctx) ([]any, error)`) plus an
  in-memory registry — no dynamic module loading, no npm. Auth is a reserved
  input key (`piece.AuthInputKey`, `"auth"`) resolved through the same `{{ }}`
  templating as any other field and surfaced as `ActionContext.Auth` for
  convenience — the engine never requires it to be present; a piece that
  needs auth checks `ctx.Auth` itself and fails clearly if it's missing
  (matches activepieces: it doesn't gate execution on auth presence either).
  `piece.OAuth2Auth` is an optional convenience type for the OAuth2 shape
  (`AccessToken`/`Data`/`Props`) — not engine-enforced either, still just a
  value behind `ctx.Auth`.
  `TriggerContext.Store` (`piece.Store` / `piece.MemoryStore`) lets a trigger
  persist a cursor across separate `Run()` calls — what a POLLING trigger
  needs to return only new items each time it's invoked. `piece.ScopedStore`
  namespaces a shared backend per flow (`'flow_'+flowID+'/'+key`), so
  multiple flows using the same trigger don't clash over the same cursor
  key. There's no
  `TriggerStrategy` concept in the engine itself (see `Trigger`'s doc
  comment) since every strategy shares one `Run` signature and the engine
  never branches on which kind a trigger is. `RunHooks.Stop`/`Respond` let a
  PIECE action reply synchronously to whatever triggered the flow over a
  webhook — `Stop` ends the run right there (`Verdict.StopResponse`);
  `Respond` replies but lets the run keep going (`ExecutionState.RespondedEarly`).
  `ActionContext.Files` (`piece.FileWriter` / `piece.MemoryFileWriter`,
  default `Engine.Files`) lets an action upload file data it produced and get
  back a reference — activepieces' `context.files.write`; incoming file
  attachments are just `*piece.ApFile` (filename/data/extension +
  `.Base64()`) passed as a normal Input value, no special engine handling
  needed since non-string/map/slice values already pass through `{{ }}`
  resolution untouched. `Action.Dropdowns` + `Engine.LoadOptions` are the Go
  analogue of activepieces' dynamic-dropdown (`loadOptions`) mechanism — a
  config-time operation, separate from running the flow, whose function
  receives the already-resolved values of the action's OTHER inputs
  (including auth) so one dropdown can depend on another (e.g. a "channel"
  list that depends on a chosen "workspace").
- **Piece validation** (`pkg/piece/validate.go`): `piece.Validate`/
  `Registry.RegisterValidated` catch structural mistakes (nil `Run`, an
  `Action`/`Trigger`'s `Name` not matching its own registry key) before they
  become a runtime panic or a silent, confusing bug — aimed specifically at
  pieces generated on the fly by an agent rather than hand-written and
  reviewed. Paired with a real, invokable Claude Skill
  (`.claude/skills/goflow-piece-authoring/`) documenting the piece contract
  and every pitfall in this list, meant to be loaded before writing a new
  `piece.Piece{...}` literal at all — catching a mistake before it's typed
  beats catching it after.
- **A small piece catalog** (`pkg/pieces`): thirteen independently-tested,
  ready-to-use pieces — `http` (a real `net/http` request, auth sent as an
  `Authorization` header — a plain string verbatim, or a `*piece.OAuth2Auth`
  formatted as `Bearer <AccessToken>` — a timeout so a hung server can't hang a
  flow, an opt-in `failOnErrorStatus` so a >=400 response can actually
  drive `RetryOnFailure`/`ContinueOnFailure` — off by default, since Run
  succeeding either way is what lets a flow inspect a 404 itself instead —
  and an opt-in `respectRetryAfter` that handles 429 rate-limiting *inside*
  the action itself: reads the `Retry-After` header, sleeps, re-sends,
  bounded by `maxRateLimitRetries` (default 3) and a `maxRetryAfterWaitMs`
  cap (default 30s) against a server asking it to wait an unreasonable
  amount of time; only the simple integer-seconds form of the header is
  supported, not the HTTP-date form), `json` (parse/stringify), `delay` (a real synchronous wait — see
  its own doc comment for why that's a deliberately different mechanism
  from activepieces' waitpoint-based Delay piece), `webhook` (a generic
  pass-through trigger — the reusable version of every ad-hoc "hub" fixture
  built across `pkg/engine`'s own tests), and `crypto` (AES-GCM
  encrypt/decrypt, key via `ctx.Auth` as a `[]byte` — the catalog version of
  the hand-rolled "vault" piece `TestFlow_EncryptDecryptRoundTrip` already
  proved in `pkg/engine`; no key management, that's the caller's job),
  `storage` (`ctx.Files.Write` — text or base64, selected via the catalog's
  first `Dropdowns` use), `approval` (pauses via `ctx.Run.WaitForWaitpoint`,
  resumes with an `{"approved": bool, "comment": string}` decision),
  `webhook_reply` (`respond`/`stop` over `ctx.Run.Respond`/`Stop` — the
  synchronous-webhook-reply mechanism as a reusable piece), `text`
  (split/join/replace/trim/case), `datetime` (parse/format/add/diff over
  RFC3339 timestamps), `hash` (md5/sha1/sha256/sha512 digests plus HMAC,
  key via `ctx.Auth` — for verifying an incoming webhook's signature),
  `regex` (match/find_all/replace/extract_groups — "no match" is a valid
  result, not an error, same philosophy as `failOnErrorStatus`), and `csv`
  (parse/stringify, `hasHeader` toggles row shape between `[][]string` and
  `[]map[string]any`). `pieces.RegisterAll(registry)` registers all
  thirteen in one call; `pieces.All()` if you want to filter
  first. No dynamic discovery, no marketplace, no versioning — add a piece
  by importing its package and listing it in `pieces.go`, same as any other
  Go dependency. Every catalog piece calls `piece.MustValidate` on itself in
  its own `New()`.
- **Standalone action runs** (`Engine.ExecuteActionRun`): run one CODE or
  PIECE action outside any flow — no trigger, no chain, retry/continueOnFailure
  still apply, pausing is rejected outright (there's nothing to resume it in).
  The Go analogue of activepieces' `actionRunStepRunner` (its "test this step"
  feature) — see "Design decisions" for how it reuses the same executors.
- Tests (`pkg/engine/engine_test.go`) proving all of the above actually work,
  mirroring the scenario-by-scenario verification done against the
  activepieces TS extraction this project's design is informed by: two-step
  chaining, router branching (including AND-within-a-condition-group and
  OR-across-groups), loop iteration, `continueOnFailure`, retry
  (both "succeeds on 3rd attempt" and "exhausts and fails"), pause→resume, a
  real (non-EMPTY) trigger, `LOG_SIZE_EXCEEDED`, pause/resume nested inside a
  loop iteration, pause/resume nested inside a ROUTER branch inside a loop
  iteration, two levels of nested `LOOP_ON_ITEMS` (both plain iteration and a
  pause/resume two loops deep), standalone action runs (success, mock context
  data, rejected pause, retry, and rejected ROUTER/LOOP_ON_ITEMS), a
  standalone action run against a piece requiring auth (missing auth fails
  clearly, a literal auth value works, a `{{ }}`-templated one does too), and
  a POLLING-style trigger discovering new items across repeated polls via a
  persisted `Store` cursor, fanning each one out into its own flow run, a
  synchronous webhook trigger whose flow replies immediately — both `Stop`
  (ends the run right there) and `Respond` (replies, then keeps running) —
  and a standalone action run reading a file attachment (`*piece.ApFile`)
  and uploading a transformed file through `ActionContext.Files`, a full
  flow (not standalone) authenticating with an OAuth2-shaped `ctx.Auth`,
  including the flow correctly failing when the token is present but expired,
  and a dynamic dropdown (`Engine.LoadOptions`) whose choices depend on a
  sibling input, feeding into a real flow that runs with the chosen value.
  Also: a real flow chaining an "encrypt" PIECE action into a "decrypt" one
  via `{{ }}` templating (AES-GCM, key carried through `ctx.Auth`), including
  the flow correctly failing when decryption is attempted with the wrong key.
  And rate limiting: a self-throttling piece rejecting over-quota calls
  inside a `LOOP_ON_ITEMS`, all 5 items eventually succeeding purely through
  the engine's existing `RetryOnFailure` — no new engine code involved.
  And real concurrency: 50 flow runs launched as goroutines against one
  shared `*Engine`, each getting back correctly isolated results with no
  cross-contamination, checked under `go test -race`; plus a timing test
  showing parallel runs measurably beat sequential ones for a workload with
  real per-run latency. And nested error handling: a CODE step whose own
  internal try/catch (wrapping a `JSON.parse`, one function call deep) never
  lets a malformed-input error reach the engine at all, its clean recovery
  output driving a `ROUTER` branch — plus direct `pkg/sandbox` unit tests for
  try/catch/finally, rethrow, and custom `Error` subclasses on their own.
  And combined events: one real trigger receiving a union of event shapes
  (`order.created` / `user.signup` / anything else), immediately dispatched
  by a `ROUTER` to the right handler, including a clean fallback. And trigger
  retries: confirming `ExecuteBegin` genuinely does NOT retry a failed
  trigger on its own (exactly one attempt, clean failure), paired with a
  trigger that recovers from transient failures via its own internal retry
  loop — zero engine involvement either way. And multiple flows sharing one
  trigger: proving a raw shared `Store` genuinely clashes between two flows
  polling the same source (the failure demonstrated directly, not assumed),
  then `piece.ScopedStore` fixing it — each flow's cursor advances
  independently against the same underlying data. And multi-tenancy: two
  projects with an identically-named flow isolated via a composed
  project+flow scope key (`ScopedStore`, zero new code), and full
  per-tenant isolation (`Registry`, `Files`, `Steps`) proven by using one
  `*Engine` per tenant — the actually-recommended pattern, since a single
  shared `Engine.Files` has no access control of its own.
- `examples/main.go`: a runnable end-to-end flow (trigger → PIECE → CODE,
  cross-referencing outputs through templating), printing the result as JSON.

## Explicitly NOT in v1

No server/API, no persistence, no auth, no UI, no piece marketplace, no
streaming progress, no distributed workers. This is a library + example, the
same scope as the activepieces TS extraction's `example-standalone.ts` — a
proof the mechanism works, not a deployable product.

## Run it

```bash
go build ./...
go test ./...
go run ./examples
```

## Design decisions worth knowing

- **goja over v8go/shelling out to Node**, per explicit request: pure Go, no
  cgo, single static binary — the actual "why Go" argument. Trade-off: no
  Node APIs (`require`, `fs`, npm packages) inside CODE steps, and no
  async/await (see below). This is arguably a *safer* sandbox than
  activepieces' `isolated-vm` for that reason (smaller surface), just not a
  more capable one.
- **CODE steps are synchronous-only.** goja supports the `Promise`
  constructor, but draining microtasks needs an explicit event-loop pump
  (`goja_nodejs/eventloop`), not wired up in v1. A `Source` that returns a
  Promise is rejected with a clear error rather than silently resolving to
  `"[object Promise]"`.
- **A fresh `goja.Runtime` per CODE call**, deliberately — no shared JS state
  leaks between steps, retries, or loop iterations. This is why the retry
  test that proves "the code really re-runs on retry" uses a PIECE action
  (a Go closure can hold a counter across calls) rather than CODE (a JS
  closure can't, by design here).
- **Router operators are a 5-item subset** of activepieces' 24
  (`TEXT_EXACTLY_MATCHES`, `TEXT_CONTAINS`, `NUMBER_IS_GREATER_THAN`,
  `NUMBER_IS_LESS_THAN`, `BOOLEAN_IS_TRUE`) — enough to prove branching works;
  add more in `pkg/engine/engine.go`'s `evaluateCondition` as real flows need them.
- **A branch's compound conditions (AND within a group, OR across groups)
  work correctly** — `Conditions` is `[][]Condition`, matching activepieces'
  own `conditionGroups.some(group => group.every(...))` shape, but every
  router test before this one used exactly one group with exactly one
  condition, so that shape's actual AND/OR logic had never run. Checked with
  a truth table (`TestRouterCondition_AndWithinGroup`,
  `TestRouterCondition_OrAcrossGroups`) — like nested loops, this is the
  second case in the series that turned out already correct on first test,
  not a bug.
- **Pause inside a loop iteration is supported**, but was NOT in the first cut
  of this README — and the gap wasn't just missing, it was actively broken:
  `executeLoop` hardcoded the loop's own step status to `StepFailed` on *any*
  non-success from a nested iteration, including a pause. That made
  `ExecuteResume`'s SUCCEEDED-or-PAUSED restore filter skip the loop step
  entirely, so resuming silently restarted the whole loop from item 1 and
  discarded the resume payload — a real bug, found by actually testing the
  scenario (see `TestPauseInsideLoop_*` in `engine_test.go`), not just a
  documented limitation. Fixed: the loop step now reports PAUSED correctly,
  and `executeLoop` resumes the specific paused iteration (seeded with its
  own prior steps, so the nested action sees `ExecutionType RESUME` and the
  real `ResumePayload`) before continuing with the remaining items.
- **A ROUTER inside a loop iteration (or anywhere) whose matched branch
  pauses is supported** — same root bug as above, worse in effect: `executeRouter`
  hardcoded its own step to `StepSucceeded` regardless of what its branch did.
  On resume, that made the router's `IsCompleted` check short-circuit
  immediately (`SUCCEEDED` looks done), so it never re-entered the branch at
  all — the paused child was left orphaned, still `PAUSED`, while the overall
  run silently reported `SUCCEEDED`. Found the same way: build the scenario
  (`TestRouterInsideLoop_*`), run it, see what breaks, then fix. Fixed:
  the router's own status now reflects its branch's outcome
  (`containerStepStatus`, shared with the loop fix above), and its
  `IsCompleted` check is only true once it's genuinely done — re-entering a
  PAUSED router on resume is safe because branch selection is a pure,
  deterministic re-evaluation, and the branch's own child executors have
  their own `IsCompleted` guards against re-running already-finished work.
  Along the way, also found and fixed `toNumber` silently returning `0` for
  any un-templated numeric literal (e.g. a condition's `SecondValue: "10"`
  typed directly, not wrapped in `{{ }}`) — it only handled Go numeric types,
  not numeric strings, which is how most literal condition values actually
  arrive.
- **Nested loops (a `LOOP_ON_ITEMS` inside another) work by plain composition,
  no special-casing** — the one case in this whole bug-hunting series that
  *didn't* turn up a bug on first test. Every loop/router mutation reads and
  writes `state.Steps[action.Name]` on whatever `*model.ExecutionState` it was
  handed, so nesting is just that same logic recursing on progressively more
  local iteration states — `outer_loop`'s and `inner_loop`'s synthetic scope
  entries coexist without clobbering each other, and a pause two loops deep
  restores correctly: outer resumes its specific paused iteration, which
  itself resumes the inner loop's specific paused iteration, which resumes
  the actual paused action — three layers of the identical mechanism, not
  three different ones. See `TestNestedLoops_*`.
- **Standalone action runs (`ExecuteActionRun`) didn't exist at all before
  this request** — not a bug fix like the last two entries, a from-scratch
  addition, checked against activepieces' `action-run-step-runner.ts` first
  (it's a five-line file: call the same per-action executor with an empty
  context and `actionRunMode: true`). Ported faithfully — same executors,
  same retry/`continueOnFailure` — with one deliberate simplification: a
  PIECE that tries to pause. Activepieces throws from inside the
  `WaitForWaitpoint` hook itself (`assertActionRunCannotSuspend`), aborting
  the piece's `run()` mid-call. Changing `piece.RunHooks.WaitForWaitpoint`'s
  signature to support that (it's currently a no-return-value `func(string)`)
  would ripple into the piece SDK for a v1-scope edge case, so instead the
  hook just flags it, the piece's `Run` finishes normally, and
  `ActionRunMode` makes `executePiece` discard the result and fail the step
  afterward. Observably identical for a well-behaved piece that
  pauses-then-returns; different only if the piece does something *after*
  calling `WaitForWaitpoint` before returning (that still runs here; it
  wouldn't in activepieces). Documented on both `ExecuteActionRun` and the
  `ActionRunMode` field, not silently different.
- **Piece auth is not a separate mechanism from a normal input field** —
  checked against `piece-executor.ts` before adding anything: activepieces
  itself just reserves the input key `'auth'`, resolves it through the exact
  same props pipeline as every other field (so it can be a literal or a
  `{{ }}` template), and additionally exposes it as `context.auth` for
  convenience. Ported exactly that (`piece.AuthInputKey`,
  `ActionContext.Auth`) — no separate "connections" store, no OAuth flow, no
  engine-side "auth is required" enforcement (activepieces doesn't have that
  either: `props-processor.ts` only validates an auth sub-object if one was
  actually supplied, never blocks a run over a missing one). A piece that
  needs auth is responsible for checking `ctx.Auth` and failing on its own —
  see `TestActionRun_PieceRequiringAuth_*`.
- **Polling lives almost entirely outside `pkg/engine`, on purpose.** The
  only engine-adjacent addition for it is `piece.Store` — everything else
  (calling `Trigger.Run` on a timer, fanning discovered items out into
  separate `ExecuteBegin` calls) is orchestration the real activepieces
  worker does too, outside `packages/server/engine` — consistent with this
  project's existing boundary (`ExecuteActionRun`'s design decision above,
  the "no flow-run timeout" one below: anything the real engine package
  doesn't own, this one doesn't fake owning either). `TestPollingTrigger_*`
  demonstrates the whole shape by simulating the scheduler directly in the
  test, calling `Registry.GetTrigger` + `Trigger.Run` by hand rather than
  through a new `Engine` method that would have had nothing engine-specific
  to do.
- **Sync webhook (`Stop`/`Respond`) checked against `piece-executor.ts`
  before adding anything, same discipline as auth.** Real activepieces gates
  the actual mid-flight HTTP send on `constants.triggerPieceName ===
  action.settings.pieceName` (only the trigger's OWN piece can reply to its
  own webhook) plus `workerHandlerId`/`httpRequestId` being set (the worker
  is already blocking on that specific HTTP request) — deliberately not
  ported: this engine has no worker, no HTTP layer, no concept of "a request
  is currently blocked waiting." Any PIECE action can call `Stop`/`Respond`
  here, unconditionally. What WAS ported faithfully: `Stop` ends the run as
  `SUCCEEDED` (never `FAILED` — stopping isn't an error) with the response on
  `Verdict.StopResponse`, while `Respond` leaves the run's `Verdict` alone
  and the chain keeps walking `NextAction` — two genuinely different
  behaviors, both real activepieces has and both proven here
  (`TestSyncWebhook_*`), not a single hook flattening the distinction away.
- **File attachments needed almost no engine changes** — checked
  `file-uploader.ts` and `ApFile` first: activepieces' file handling is two
  small, independent pieces (`ApFile{filename, data, extension}` for input,
  `context.files.write` for output), not one big subsystem. Ported both as
  plain Go types (`piece.ApFile`, `piece.FileWriter`/`MemoryFileWriter`) with
  no real storage backend (in-memory only — same "no server" boundary as
  everything else in this list). The one genuinely interesting finding: the
  *input* side required zero engine work, because `expr.Resolve`'s switch
  already has a `default: return value, nil` case for anything that isn't a
  string/map/slice — a `*piece.ApFile` passed as an Input value was already
  going to flow through `{{ }}` resolution untouched before this feature
  existed. Not ported: activepieces' `AP_MAX_FILE_SIZE_MB` cap and streaming
  (`Buffer`/`Readable`) uploads — v1 takes a plain `[]byte`, no size limit.
- **OAuth2 needed zero new engine code** — the most extreme case of this
  pattern yet. Checked `piece-helper.ts`'s `executeRefreshTokenAuth` before
  writing anything: for a standard OAuth2 connection it unconditionally
  returns `{skipped: true}` — activepieces refreshes OAuth2 tokens in the
  *server*, outside `packages/server/engine`, not in the engine at all. So
  there was no refresh mechanism to port, faithfully or otherwise; "OAuth2
  support" here is entirely `piece.OAuth2Auth`, an optional named-fields
  convenience type for the same untyped `ctx.Auth` every other auth test
  already exercises. What the tests actually add is the one thing that IS
  new about OAuth2 versus a plain API key: auth being *present but stale* —
  `TestFlow_OAuth2Auth_ExpiredTokenFailsClearly` is a different failure mode
  than "missing auth" (already covered), and it runs as a full flow
  (`ExecuteBegin`, not `ExecuteActionRun`) specifically because the request
  was for testing this in a flow.
- **Dynamic dropdowns are their own operation, deliberately not woven into
  flow execution** — checked `property.operation.ts`/`piece-helper.ts`'s
  `executeProps` first: activepieces calls a dropdown's `options()` from the
  UI editor, config-time, entirely separate from `flowOperation.execute`.
  `Engine.LoadOptions` mirrors that split rather than trying to make it part
  of `ExecuteBegin`/`ExecuteActionRun`. Narrowed on purpose: only `DROPDOWN`
  (activepieces also has `MULTI_SELECT_DROPDOWN` and `DYNAMIC` — same
  "prove the mechanism, not the whole catalog" scoping as the router's
  5-of-24 operators), and `LoadOptionsInput.Input` is NOT run through `{{ }}`
  templating like `ExecuteBegin`'s inputs are — activepieces' editor mostly
  hands the props resolver already-literal form values here, so skipping
  template resolution is a real (documented, not accidental) narrowing, not
  an oversight. `TestFlow_DynamicDropdown_SelectedValueRunsInFlow` closes the
  loop: whatever `LoadOptions` returned as a `Value`, a real flow using that
  exact value as a plain input runs with zero special-casing — the dropdown
  is purely an editor-time concept, invisible at run time.
- **Data encryption/decryption is not an engine mechanism at all, in either
  codebase — confirmed by grep before writing anything.** Searched the whole
  TS extraction for encrypt/decrypt/crypto: nothing in
  `packages/server/engine`. Connection secrets arrive at the engine already
  decrypted, fetched from the server over an authenticated API call
  (`createConnectionResolver(...).obtain(key)` in `utils.ts`) — encryption at
  rest is a server/database concern, same boundary as OAuth2 refresh,
  polling scheduling, and flow timeout. So there was nothing to port here,
  faithfully or otherwise. What `TestFlow_EncryptDecrypt_*` actually proves
  is unrelated to any new engine surface: an "encrypt" PIECE action chained
  into a "decrypt" one through a real flow, `{{ }}` templating carrying the
  ciphertext between them, AES-GCM key delivered through `ctx.Auth` — every
  piece of that (chaining, templating, auth-as-a-value) already existed.
  `[]byte` as an auth/input value needed the same zero engine changes as
  `*piece.ApFile` and `*piece.OAuth2Auth` before it, for the same reason:
  `expr.Resolve` already passes anything that isn't a string/map/slice
  through untouched.
- **Rate limiting is not an engine mechanism either — checked first, same as
  every other "not this engine's job" entry above.** Grepped the whole TS
  extraction; the only hit was `projectRateLimiterEnabled`, a boolean config
  flag in a health/status schema (`core/shared/health/index.ts`) — a knob
  that presumably enables limiting *somewhere* in the real product, not an
  implementation living in `packages/server/engine`. So there was nothing to
  port. `TestFlow_RateLimiting_LoopEventuallySucceedsViaRetry` proves
  something more specific than "a piece can throttle itself" (unsurprising —
  any piece can run arbitrary Go): that a rate-limited piece can lean on
  `RetryOnFailure` — a capability the engine already had, verified
  independently — to survive being throttled, without the engine needing to
  know anything about rate limits as a concept. Kept the timing small (an
  8ms window, 3ms retry interval) and re-ran it standalone five times before
  trusting it — timing-sensitive tests are exactly the kind that look green
  once and flake later.
- **Concurrency is the one request in this series that isn't "check
  activepieces first" — there's nothing to check.** activepieces' router/
  loop executors never run branches or iterations in parallel either (no
  `Promise.all` anywhere in `router-executor.ts`/`loop-executor.ts` —
  confirmed by grep, not assumed): both engines execute a single flow's
  steps strictly sequentially. What's different is Node's engine can't
  meaningfully run MULTIPLE flow RUNS in parallel within one process either
  — real concurrency there means spawning separate OS processes
  (`isolated-vm`/child-process forking, one per sandboxed CODE step or flow
  run) — while this is a Go library any caller can already hand to N
  goroutines. That capability had never actually been exercised by this
  test suite until now: every prior test called the engine from a single
  goroutine, so `go test`'s default (no parallelism without explicit
  `t.Parallel()`) never exercised it. Building `TestConcurrentFlowRuns_*`
  turned up a real, if narrow, gap along the way: `piece.Registry`'s
  backing map had no mutex. Fixed with a `sync.RWMutex` — but stated
  honestly in the test's own doc comment: the tests as written only call
  `Register` once, before any goroutine starts, so concurrent-reads-only
  isn't actually a data race Go's detector would have flagged; the fix
  covers a realistic pattern (registering pieces while flows elsewhere are
  already reading the registry) that this specific test doesn't exercise,
  not a red-handed catch. Two tests: one on correctness (50 concurrent runs,
  zero cross-contamination, clean under `-race`), one on the actual payoff
  (parallel meaningfully faster than sequential for a workload with real
  per-run latency) — re-ran both several times standalone before trusting
  them, same discipline as the rate-limiting test above.
- **Nested try/catch is standard JS, not a goflow mechanism — so this request
  was really "does goja hold up," and it was checked directly rather than
  assumed.** `pkg/sandbox` had zero unit tests of its own before this;
  everything about CODE-step behavior had only ever been exercised
  indirectly through `pkg/engine`'s flow tests. Added `sandbox_test.go`
  covering what a hand-rolled or approximated JS evaluator (activepieces'
  own `jsep`-based templating included) most commonly gets wrong: a
  try/catch that recovers and returns normally, try/catch nested inside
  another try/catch (both the "inner handles it, outer never fires" and the
  "inner rethrows, outer catches the wrapped error" shapes), an error that
  escapes every catch and must still surface as a real Go error with the
  right message, a custom `class X extends Error` preserving `instanceof`
  and custom fields, and `finally` running on both the throw and no-throw
  paths. All six passed on the first run — goja's own fidelity, not
  something this project had to work around. The flow-level test
  (`TestFlow_NestedTryCatch_RecoveryFeedsRouterBranch`) is the other half:
  proving a step's *internal* JS error handling (catching a `JSON.parse`
  failure) and the *engine's* error handling (`ContinueOnFailure`, already
  covered elsewhere) are genuinely different layers — a caught error inside
  a step's own try/catch never reaches the engine as a failure at all, its
  clean recovery output just drives a `ROUTER` branch like any other value.
- **"Multiple triggers on one flow" doesn't exist to build — checked the data
  model, not just the engine, before writing anything.** `FlowVersion.trigger`
  in activepieces' `flow-version.ts` is `trigger: FlowTrigger`, a single
  field, not an array — same as `model.FlowVersion.Trigger *FlowTrigger`
  here. This isn't a boundary finding like timeout/OAuth2/rate-limiting
  (something real that lives outside the engine package); it's that neither
  codebase's flow model has a "more than one trigger" concept at all, so
  adding one would be inventing a new shape neither engine actually has, not
  porting something real. What real platforms do instead, and what
  `TestFlow_CombinedEvents_OneTriggerDispatchesByEventType` builds: ONE
  trigger receiving a union of event shapes (a webhook fed by two unrelated
  upstream systems, in this case), dispatched immediately by a `ROUTER` on
  an event-type field. Every piece already existed and was independently
  tested (real trigger, `FIRST_MATCH` branching, fallback) — this is a
  composition test proving the idiomatic pattern works end-to-end, not new
  engine surface.
- **Trigger retries surfaced a real, previously-unpinned asymmetry: actions
  get `RetryOnFailure`, triggers get nothing — in both codebases.** Checked
  `trigger-helper.ts`/`flow.operation.ts` before writing anything:
  `runWithExponentialBackoff` is only ever called from `codeExecutor`/
  `pieceExecutor`; there is no equivalent wrapper anywhere around
  `triggerHelper.executeTrigger`. `ExecuteBegin` here already matched that
  (a single unwrapped `trig.Run(...)` call), but nothing had ever pinned the
  behavior down with a test — `TestTrigger_NoEngineLevelRetry_FailsOnFirstAttempt`
  does, with a call counter proving exactly one attempt, not just checking
  the end verdict. The realistic mitigation is the same shape as rate
  limiting and encryption earlier in this series: ordinary logic inside the
  piece's own `Run`, not an engine feature —
  `TestTrigger_InternalRetrySucceedsAfterTransientFailures` proves a trigger
  can retry its own flaky upstream call internally (2 failures then success,
  call-counted so it's not just passing by luck) and `ExecuteBegin` never
  needs to know that happened.
- **Multiple flows sharing a trigger turned up a real, previously-untested
  gap: `piece.MemoryStore` had no per-flow namespacing at all.** Checked
  `store.ts`'s `createContextStore`/`createKey` first: activepieces has
  exactly ONE store-entries API backing every flow in the project, and
  isolates them by prefixing every key with `'flow_' + flowId + '/'` for the
  default `StoreScope.FLOW` (only `StoreScope.PROJECT` skips that prefix,
  for state meant to be intentionally shared). `piece.MemoryStore` before
  this had no such prefixing — two flows sharing one instance for the same
  trigger's cursor (an ordinary setup: the same Slack channel triggering two
  independent flows) would genuinely clash. Proved the failure mode exists
  first (`TestMultipleFlowsSameTrigger_UnscopedSharedStoreClashes` — flow B's
  first-ever poll wrongly returns 0 items because it silently inherited flow
  A's cursor), then fixed it with `piece.ScopedStore`, a thin per-flow
  key-prefixing wrapper around any `Store`, and proved that isolates them
  correctly. Simplified vs. activepieces: no per-call `scope` parameter on
  `Get`/`Put` (its default `StoreScope.FLOW` vs. explicit `StoreScope.PROJECT`)
  — `ScopedStore` only ever provides `FLOW` scope; for the project-shared
  case, use the underlying `Store` directly instead of wrapping it, since an
  unprefixed key already *is* what `StoreScope.PROJECT` does.
- **Multi-tenancy/project isolation is not an engine mechanism in
  activepieces either — confirmed by the exact signature of the function
  that would have to implement it.** `createContextStore`'s params are
  `(apiUrl, prefix, flowId, engineToken)` — no `projectId` at all. Tenant
  isolation in the real system is enforced by the SERVER validating each
  `engineToken`'s implicit project scope on every store/connection lookup;
  `projectId` only ever shows up as metadata handed to a piece
  (`context.project.id`), never as a security boundary the engine itself
  checks. Same class of finding as OAuth2 refresh and encryption: nothing to
  port. Two things worth proving instead: (1) `piece.ScopedStore` — built
  for flow-level isolation, not project-level — composes for free into
  project-level isolation too, by folding the project into the same scope
  key (`"project-A/flow-1"` vs `"project-B/flow-1"`); zero new engine code,
  same shape as the OAuth2/file-attachment findings. (2) The actually-robust
  answer is one `*Engine` per tenant, not scope-key discipline alone —
  proven by `TestMultiTenancy_SeparateEnginesFullyIsolated`, which along the
  way surfaced a real (if narrow) nuance in `piece.MemoryFileWriter`: its ID
  counter starts independently per instance, so two SEPARATE writers can
  coincidentally mint the identical URL string for two different files. Not
  a leak (each instance's `Get` only ever returns its own stored bytes for
  that key — verified by content, not just key presence, after the first
  version of this test's assertion falsely flagged one on a coincidental ID
  match) — but a real reason a URL is only ever meaningful within the writer
  instance that produced it, never treat it as globally unique.
- **`LOG_SIZE_EXCEEDED` sizing is `json.Marshal(state.Steps)` length**, checked
  after every step — simple and correct, not optimized (activepieces
  incrementally tracks size instead of re-serializing the whole journal each
  time; fine for v1, would matter for very large/hot flows).
- **No flow-run timeout support** — matching the finding from evaluating the
  TS extraction: activepieces' own engine has no internal timeout logic
  either (`timeoutInSeconds` is accepted but never read); it's enforced by
  the external worker killing the process. Same story here: nothing to port,
  nothing implemented.
- **The catalog (`http`/`json`/`delay`/`webhook`) is the first addition in
  this whole series that needed the guardrails built specifically for it to
  matter** — `piece.Validate`/`MustValidate` and the goflow-piece-authoring
  skill existed before a single catalog piece did, precisely so building
  "real" pieces (not ad-hoc test fixtures a human reviews line by line)
  would be exercised against them immediately rather than eventually. Net
  result: zero engine bugs found writing four new pieces, unlike several
  earlier entries in this file — the closest comparison is the nested-loops
  and AND/OR-router-condition tests, which also passed clean on the first
  try, except this time by design rather than coincidence. `http`'s tests
  use `httptest.Server`, never real network calls, for the same determinism
  reason every other timing/network-adjacent test in this project does.
  `TestCatalog_WebhookThenHTTPThenJSON` (`pkg/pieces/integration_test.go`)
  is the one integration test proving the catalog composes through a real
  `ExecuteBegin` flow, not just through isolated per-piece unit tests.
- **Testing the catalog with retries surfaced one real, immediately-fixed
  gap: the `http` piece could never trigger `RetryOnFailure` at all.**
  `RetryOnFailure`/`ContinueOnFailure` (`pkg/engine`'s `recordStep`) only
  ever look at whether `Run` returned a Go `error` — never at `Output`. The
  `http` piece's original behavior (any response, even a 500, is just data;
  `Run` always succeeds) meant a flaky server could never be retried through
  the engine's own mechanism, no matter what `ErrorHandling.RetryOnFailure`
  said. Added `failOnErrorStatus` (opt-in, default off — many flows
  legitimately want to inspect a 404/500 themselves) so a >=400 response
  becomes a real error when asked. `TestCatalog_HTTPRetriesAgainstFlakyServer`
  is the catalog's version of `TestRetryOnFailureSucceedsOnThirdAttempt` —
  same proof shape (a server-side request counter, not just an eventual
  SUCCEEDED verdict) — but against a real `httptest.Server` through the real
  `http` piece instead of a hand-rolled test fixture.
- **Testing the catalog with rate limiting found a real gap `failOnErrorStatus`
  doesn't cover: nothing read the `Retry-After` header.** The earlier
  engine-level rate-limiting test (`TestFlow_RateLimiting_LoopEventuallySucceedsViaRetry`
  in `pkg/engine/engine_test.go`) used a hand-rolled, self-throttling piece
  leaning on the engine's fixed exponential backoff — reasonable there,
  since the engine has no rate-limiting concept either (confirmed against
  the TS source: only a `projectRateLimiterEnabled` config flag exists,
  nothing that reads response headers). But the engine's backoff is fixed
  and blind to the response, so it can never wait the *exact* duration a
  real API asks for via `Retry-After` — only the piece that actually sees
  the header can. Added `respectRetryAfter` to the `http` piece: on a 429 it
  reads `Retry-After` (integer-seconds form only), sleeps, and re-sends
  *inside* `Run` — deliberately independent of `ErrorHandling.RetryOnFailure`,
  since the engine's retry loop has no way to plug in a server-supplied
  delay. Bounded two ways so a hostile or misconfigured server can't hang a
  flow run: `maxRateLimitRetries` (default 3, how many internal retries) and
  `maxRetryAfterWaitMs` (default 30s, a wait longer than this is not
  attempted at all — the 429 is returned as-is instead). Rebuilding the
  request each internal attempt (via a `buildRequest` closure) was necessary
  because `*http.Request`'s body reader is consumed after one use — reusing
  the original `req` on retry would silently send an empty body on POST/PUT.
  `TestHTTP_RespectsRetryAfterHeader` proves real wait timing (elapsed time
  assertion, not just a retry count) with a genuine ~1s sleep — the one test
  in this project that deliberately pays real wall-clock time instead of
  using millisecond-scale fakes, because proving "it waited approximately
  the right amount" needs an actual clock. `TestCatalog_HTTPRespectsRateLimitInRealFlow`
  (`pkg/pieces/integration_test.go`) proves this composes through a real
  `ExecuteBegin` flow with *no* `RetryOnFailure` configured on the step at
  all — the retry is entirely the piece's own doing, not the engine's.
- **Testing the catalog with OAuth2 found the `http` piece silently dropped
  OAuth2 auth entirely.** The engine's OAuth2 support
  (`pkg/engine/engine_test.go`'s `TestFlow_OAuth2Auth_*`) was already proven
  against a hand-rolled "crm" piece that just type-asserted `ctx.Auth` itself
  — it never actually built an HTTP request, so it could never have caught
  this. The real `http` piece's auth handling was `ctx.Auth.(string)` only:
  a `*piece.OAuth2Auth` value fails that assertion, `ok` is false, and the
  piece proceeds with **no** `Authorization` header at all — no error, no
  warning, request just goes out unauthenticated. Fixed by switching on
  `ctx.Auth`'s type: a plain string is still sent verbatim (unchanged), a
  `*piece.OAuth2Auth` is now formatted as `Bearer <AccessToken>` — the
  convention essentially every OAuth2-authenticated activepieces piece uses
  by hand. `TestHTTP_OAuth2AuthSentAsBearerToken` and
  `TestHTTP_NilOAuth2AuthSendsNoAuthorizationHeader` cover the piece in
  isolation; `TestCatalog_HTTPSendsOAuth2AuthInRealFlow`
  (`pkg/pieces/integration_test.go`) proves it through a real `ExecuteBegin`
  flow with the auth value placed under `piece.AuthInputKey` exactly like
  every other catalog integration test — no OAuth2-specific engine code
  needed, same as the original finding.
- **A dedicated unit-test pass per catalog piece, on top of the
  integration/scenario tests above, found no engine or piece bugs — just
  filled real gaps in what each piece's own `_test.go` covered.** `json`:
  non-object top-level JSON (array/string/number/bool/`null`, not just
  objects), an unsupported Go value (`func()`) failing `stringify` clearly,
  a present-but-`nil` `data` value encoding as `"null"` rather than being
  treated as missing, and a fully-missing `data` key still failing.
  `delay`: `float64`/`int` inputs (not just `int64`) accepted the same as
  the templated-JSON case elsewhere in this project, and a non-numeric input
  (a numeric *string*) correctly rejected — `delay` does not lean on
  `pkg/engine`'s own lenient `toNumber` string-parsing, so this needed its
  own coverage. `webhook`: a nil, string, slice, or bare-number payload all
  pass through `catch_hook` completely untouched, not just the map-shaped
  payload the original test used. `http`: a malformed URL fails at
  request-building (before any network call), and the method defaults to
  GET when omitted. Every catalog piece now has its own battery of
  unit tests independent of the cross-piece integration tests in
  `pkg/pieces/integration_test.go`.
- **Testing the catalog with multi-tenancy found nothing to fix — worth
  stating honestly rather than inventing a gap.** `pkg/engine`'s own
  multi-tenancy tests (`TestMultiTenancy_SeparateEnginesFullyIsolated`,
  `TestMultiTenancy_ComposedProjectAndFlowScoping`) prove isolation via
  separate `Engine`s and `ScopedStore`. None of that applies to the catalog:
  none of `http`/`json`/`delay`/`webhook` ever touch `Store` or `FileWriter`
  — every one of them is pure per-call logic (`http` allocates a fresh
  `*http.Request`/`*http.Client` inside `Run` every time, never a shared/cached
  one), so there's no per-tenant state for the catalog itself to leak. The
  question actually worth answering for the catalog is the realistic
  deployment shape: one process, one shared registry/engine, many tenants'
  flows running *concurrently* — does the real `http` piece ever cross one
  tenant's OAuth2 token or response into another's under real concurrency?
  `TestCatalog_MultiTenancy_ConcurrentTenantsSharingOneRegistryDontLeak`
  (`pkg/pieces/integration_test.go`) runs 8 tenants concurrently against 8
  distinct `httptest.Server`s sharing one `Engine`/registry, each server
  rejecting any request not carrying its own token (so a crossed auth header
  can't be missed), verified under `-race` across 5 repeated runs. No leak
  — confirms the catalog composes safely under concurrent multi-tenant use
  for free, same as `TestConcurrentFlowRuns_IsolatedAndRaceFree` already
  showed at the engine level.
- **Asked to test the catalog with encryption/decryption, the honest answer
  was "there's nothing to test" — the catalog had no crypto piece.**
  Confirmed with a grep before assuming, not guessed. Rather than force-fit
  encryption into an existing piece, added `crypto` (`pkg/pieces/crypto`) as
  a genuinely new fifth catalog piece — explicitly a deliberate scope
  addition, not something "testing the catalog" implied on its own, done
  only after asking and getting a yes. It's a direct formalization of the
  hand-rolled "vault" piece `pkg/engine`'s `TestFlow_EncryptDecryptRoundTrip`
  already exercised — identical mechanism (AES-GCM via `crypto/aes`+
  `crypto/cipher`, nonce prepended to the sealed bytes, base64-encoded),
  same `ctx.Auth`-as-`[]byte`-key convention as every other secret in this
  project, so zero engine changes were needed here either.
  `TestCrypto_DifferentKeyLengths` covers AES-128/192/256 (`aes.NewCipher`
  accepts 16/24/32-byte keys); `TestCrypto_Decrypt_WrongKeyFailsClearly` and
  `TestCrypto_Decrypt_TamperedCiphertextFailsClearly` both lean on GCM being
  an *authenticated* cipher mode — a wrong key or a single flipped
  ciphertext byte must fail decryption outright, not silently return garbage
  plaintext. `TestCatalog_EncryptDecryptRoundTripThroughRealFlow`
  (`pkg/pieces/integration_test.go`) is the catalog's version of
  `TestFlow_EncryptDecryptRoundTrip`, additionally round-tripping the
  ciphertext through the real `json` piece (stringify then parse) to prove
  it survives as an ordinary opaque string value through the rest of the
  catalog, not just as a direct step-to-step reference;
  `TestCatalog_DecryptWithWrongKeyFailsClearly` is the catalog's version of
  `TestFlow_DecryptWithWrongKeyFailsClearly`.
- **Eight more catalog pieces (`storage`, `approval`, `webhook_reply`,
  `text`, `datetime`, `hash`, `regex`, `csv`) were added from a written spec
  (`TICKETS.md`) sized for parallel/delegated implementation, then built out
  — and nothing surprising turned up.** Worth stating plainly rather than
  padding this section: `storage`, `approval`, and `webhook_reply` are the
  first catalog pieces to touch `ctx.Files`, `ctx.Run.WaitForWaitpoint`, and
  `ctx.Run.Respond`/`Stop` respectively — previously only exercised by
  hand-rolled fixtures inside `pkg/engine`'s own test suite — and every one
  of them worked exactly as `pkg/piece`'s own doc comments already
  documented, zero engine changes, zero behavioral surprises.
  `TestCatalog_StorageWriteThroughRealFlow` and
  `TestCatalog_ApprovalPauseResumeThroughRealFlow`
  (`pkg/pieces/integration_test.go`) prove the first two through the real
  catalog end to end (`storage.write` producing bytes an engine-level
  `*piece.MemoryFileWriter` actually holds; `approval.request` pausing a
  real `ExecuteBegin`, resuming via `ExecuteResume`, and a chained step
  reading the decision through `{{ }}` templating). `storage` is also the
  catalog's first use of `Action.Dropdowns`/`LoadOptions`. `hash`'s known
  test vectors (`TestHash_Digest_KnownVectors`, RFC 4231's HMAC-SHA256 case)
  caught a transcription typo in the test's own expected values on first
  run — not an engine or piece bug, just a reminder that "known vectors"
  only catch what you actually verify against a real published source, not
  what you recall from memory.
- **Tested all eight new pieces together in one realistic flow, not just
  individually.** Per-piece unit tests call `Run` directly with hand-built
  `Input` maps — they can't catch a composition problem where one piece's
  real `Output` shape doesn't feed cleanly into another's `Input` through
  `{{ }}` templating. `TestCatalog_NewPiecesComposeInRealOrderFlow`
  (`pkg/pieces/integration_test.go`) chains seven of them plus the
  pre-existing `webhook` trigger into one order-processing flow: `webhook`
  receives a raw line and its signature → `hash.hmac` verifies it →
  `regex.extract_groups` pulls fields out (via `{{ trigger_1.output.body }}`)
  → `text.case` uppercases the order id → `datetime.now` stamps it →
  `csv.stringify` builds a log line referencing three prior steps' outputs
  in one nested `rows` map → `storage.write` persists it → `webhook_reply.respond`
  replies with the result. Every step's input is a template reference to a
  real prior step's output, including array-index access
  (`{{ extract.output.groups[0] }}` — a Go `[]string` indexed as a JS array
  through goja). Passed on the first run: no cross-piece type mismatches,
  no template-resolution surprises. `approval` is the only new piece not
  included here — its pause/resume shape doesn't fit a single linear
  `ExecuteBegin` chain, and it's already proven end-to-end on its own via
  `TestCatalog_ApprovalPauseResumeThroughRealFlow`.
- **JSON flow definitions were built as phase 1 of an explicit "AI-first"
  direction**: instead of a large fixed node catalog most users barely
  touch (the stated problem with n8n/activepieces-style tools), the goal is
  an AI defining flows as data, and growing the catalog itself when a real
  gap appears — rather than hand-coding a piece per use. Phase 1 only
  covers the FLOW side: `model.ParseFlowVersion` plus `json` tags on every
  definition type. Adding json tags was mechanical (Go's default
  marshaling already handles pointer trees like `FlowAction.NextAction` and
  `RouterSettings.Children` correctly) — the one real finding was that
  `pkg/pieces/crypto` and `pkg/pieces/hash`'s `ctx.Auth` handling
  (`ctx.Auth.([]byte)` only) made both pieces **entirely unusable from a
  JSON-defined flow**: JSON has no `[]byte` type, so a JSON-supplied secret
  always decodes as a Go `string`, and the type assertion would just fail
  silently ("missing key") even though a key was supplied. Fixed by adding
  a string case to both (`authKey` in each package) — a small, safe,
  backward-compatible change (existing `[]byte`-passing tests untouched)
  that was necessary, not optional, for those two pieces to be reachable
  from JSON at all. `TestJSONDefinedFlow_ExecutesThroughRealCatalog`
  (`pkg/pieces/integration_test.go`) is the decisive proof: that flow exists
  *only* as a JSON string in the test — never built as a Go struct — parsed
  via `ParseFlowVersion` and executed through the real catalog, including
  `hash.hmac`'s new string-auth path.
- **Phase 2 (deliberately not started): letting an AI add a genuinely new
  piece to the catalog without a Go recompile.** Every catalog piece today
  is Go source code registered at compile time
  (`pieces.RegisterAll`'s hardcoded import list) — an AI "adding a node" in
  this repo today still means writing a `.go` file and rebuilding the
  binary (what the goflow-piece-authoring skill and `TICKETS.md` both
  assume). The architecturally sound path for true runtime piece authoring
  is JS-defined pieces on top of the goja sandbox already embedded for CODE
  steps — but that needs a JS-facing equivalent of the Piece contract
  (`ctx.Auth`/`ctx.Files`/`ctx.Run.Stop`/`Respond`/`Dropdowns`), which
  doesn't exist yet. Not attempted here; noted so this isn't mistaken for
  "AI can add pieces at runtime" already working.
- **Building the Phase 2 loader (`pkg/jspiece`) found a real, pre-existing
  bug in `pkg/sandbox` that had shipped since the very first version of
  this project, undetected the whole time.** `sandbox.Run`'s Promise
  rejection (`isPromiseLike`) checked `goja.Object.ClassName() == "Promise"`
  — but goja's actual `ClassName()` for a Promise object is `"Object"`, not
  `"Promise"`. The check never once matched. `sandbox.Run` was silently
  returning the raw `*goja.Promise` value as a CODE step's `Output` for any
  async function, instead of rejecting it — exactly the "resolves to
  `[object Promise]`-shaped trap" the function's own doc comment says it
  guards against. Undetected because no test ever called `sandbox.Run` with
  a Promise-returning source — `sandbox_test.go` covered try/catch/finally
  fidelity thoroughly but never this path, and every `pkg/engine` CODE-step
  test happens to use synchronous sources. Found by writing `pkg/jspiece`,
  which copied the same (broken) check, then wrote a real test for it
  first — the test failed immediately by returning `nil` error, not the
  expected rejection, forcing an actual investigation. Root cause: `Export()`
  on a Promise value returns a `*goja.Promise`, not something
  `ClassName()`-detectable as `"Object"` vs `"Promise"` — confirmed by a
  throwaway diagnostic Go program, not guessed. Fixed the check in **both**
  packages (`_, ok := result.Export().(*goja.Promise)`), and added
  `TestRun_PromiseReturnIsRejected` to `pkg/sandbox/sandbox_test.go` — a gap
  that existed since before this Phase 2 work started, closed as a
  byproduct of it.
- **`ctx.fetch` gives JS pieces real, unrestricted outbound network
  access — a deliberate risk, confirmed explicitly rather than defaulted
  to.** The alternative (pure logic + `ctx.files`/`ctx.run`/`ctx.auth`
  only, no network) is safer for code an agent generates and registers
  with no human review, but makes JS pieces unable to call a real API at
  all — a large chunk of what makes a piece useful. Chose network access;
  there is no SSRF protection (no blocked internal/private address ranges)
  — noted here as a real gap, not silently accepted. `fetchJS`'s shape
  (`{url, method, headers, body}` in, `{status, headers, body}` out)
  deliberately mirrors `pkg/pieces/http`'s contract for consistency, though
  the two share no code.
- **`ctx.fetch({})` (missing `url`), `ctx.files.write` and `ctx.run.*`'s
  Go-typed arguments (fixed arity: `write(fileName, content)`,
  `stop(resp)`, `waitForWaitpoint(id)`) were deliberately NOT modeled as
  `goja.FunctionCall` raw bindings with manual optional-argument handling.**
  Considered it (to avoid any doubt about how goja converts a missing/extra
  JS argument into a Go function's parameters) but confirmed via `go doc`
  that "any other Go function is wrapped so that the arguments are
  automatically converted" — bundling `fetch`'s options into a single
  object argument (`ctx.fetch({url, ...})`) sidesteps the
  optional-second-argument question entirely rather than needing to solve
  it, and the fixed-arity hooks are always called with all their arguments
  by any well-formed piece. Simpler than it first looked; verified by the
  tests actually passing, not just by reading the doc.
- **A returned JS number's exported Go type depends on whether it passed
  through untouched or was computed.** `ctx.fetch(...)`'s `status` (an
  untouched `resp.StatusCode` `int`, wrapped and handed back to JS without
  modification) exports back to Go as a plain `int`. `Number(res.body) * 2`
  (genuine JS arithmetic) exports as `int64` for a whole number — both
  confirmed empirically (a throwaway diagnostic program for the former,
  `TestJSPiece_RunsThroughRealEngineFlow`'s passing assertion for the
  latter) rather than assumed from the pre-existing "int64 vs float64
  depending on whether arithmetic occurred" note elsewhere in this file.
  Same lesson as every other numeric-quirk finding in this project: don't
  guess goja's export type, check it.
- **Tested a JS piece registered alongside the real catalog, not just in
  `pkg/jspiece`'s own isolated registry.** `pkg/jspiece`'s existing tests
  (including one full engine flow) all used a bare `piece.NewRegistry()`
  with only the JS piece in it — never proved a JS piece coexists safely
  with `pieces.RegisterAll`'s thirteen Go pieces in the same registry, or
  that its output/input actually composes with real catalog pieces through
  `{{ }}` templating in both directions.
  `TestCatalog_JSPieceComposesWithRealCatalog`
  (`pkg/pieces/integration_test.go`) does both: a `risk_score` JS piece
  (pure classification logic with no Go catalog equivalent — the exact
  kind of one-off, flow-specific logic Phase 2 exists so nobody has to
  write and ship a Go piece for) is fed by a real trigger's payload,
  classifies it, and its output feeds `json.stringify` then
  `storage.write`, both real catalog pieces. Passed on the first run — no
  registry conflicts, no cross-piece type mismatches between JS-exported
  and Go-native values.
- **An intense, three-part test battery (adversarial security, engine-scale
  concurrency/load, and fuzzing) found two real bugs and one real,
  previously-undocumented safety limitation — worth a consolidated summary
  since it spans several packages.**
  - **Adversarial (`pkg/jspiece/adversarial_test.go`)**: deep recursion,
    prototype pollution across calls, thrown non-Error values, circular
    return objects, SSRF-reaching fetch, concurrent isolation under
    `-race` — all held. Found and fixed one real bug:
    `ctx.run.stop`/`respond`/`waitForWaitpoint` and `ctx.files.write`
    called from JS against a zero-value `piece.ActionContext` (no hooks
    wired — e.g. any bare `act.Run()` call outside the engine, which is
    what most of this package's own tests already do) used to panic with
    an uncaught Go nil-pointer dereference instead of returning a clean
    error. The real engine always wires all three hooks, so this never
    fired in a normal flow — but crashed the calling goroutine for any
    other caller. Fixed by guarding each hook and returning an error
    instead (goja converts a Go function's trailing `error` return into a
    normal, catchable JS exception).
  - **Concurrency/load (`pkg/engine/stress_test.go`)**: scaled the
    existing `TestConcurrentFlowRuns_*` tests up an order of magnitude —
    real catalog pieces under concurrent load, a 5,000-item
    `LOOP_ON_ITEMS`, a 200-deep nested `ROUTER` chain, a shared registry
    under simultaneous reads and flow execution — all correct and
    race-free. One genuine test-infrastructure limit found (not an engine
    bug): hammering a single local `httptest.Server` with 500 simultaneous
    raw connections hit Windows' TCP accept-queue limits
    (`WSAECONNREFUSED`), reproducible across repeated runs. Not a goflow
    issue — dialed back to 150 concurrent runs against one listener, which
    is stable and still a real scale increase over the original tests'
    50.
  - **Fuzzing (`pkg/model/fuzz_test.go`, `pkg/expr/fuzz_test.go`,
    `pkg/jspiece/fuzz_test.go`)**: native Go fuzzing (`go test -fuzz`)
    against `model.ParseFlowVersion` (~12M execs), `expr.Eval`/
    `expr.Resolve` (~2M execs combined), and JS piece source (~500K
    execs) — zero panics found across all three. Building `FuzzEval`
    exposed a real, previously-missing safeguard: `expr.Eval` (the `{{ }}`
    templating engine — reachable with agent-authored input via Phase 1's
    JSON flows) had **no execution timeout at all**, unlike
    `pkg/jspiece`/`pkg/sandbox`. A `{{ while(true){} }}` in any flow's
    Input would have hung the executing goroutine forever, on every step
    resolution, with zero recovery. Fixed by adding the same
    `goja.Interrupt`-based `DefaultTimeout` (5s) the other two packages
    already use.
  - **The deeper finding, surfaced only by combining fuzzing with direct
    measurement, not by fuzzing alone**: that new timeout (and the
    pre-existing ones in `pkg/jspiece`/`pkg/sandbox`) does NOT bound
    wall-clock execution time the way it looks like it should.
    `goja.Runtime.Interrupt`'s own doc comment says it "does not interrupt
    native Go functions (which includes all built-ins)" — read naively,
    that sounds like "native calls are just uncovered." A direct,
    measured comparison
    (`TestEval_TimeoutDoesNotBoundNativeBuiltInWallClockTime`) shows
    something more serious: a native built-in call already in progress —
    `String.prototype.repeat` on a huge count, `new Array(n)` — runs to
    **full completion**, paying its entire CPU/memory cost, no matter how
    long that naturally takes; the timeout only discards the result
    afterward instead of returning it, it never prevents the cost. A
    100M-char `repeat()` took the same ~200ms whether the timeout was 1ms
    or absent. First surfaced as an anomaly (one fuzz seed,
    `'x'.repeat(1e9)`, took 39.5s under `-race` despite a 30ms timeout
    override) — confirmed with an isolated diagnostic comparing
    interrupted vs. uninterrupted native calls at several sizes before
    writing it up, not assumed from the anomaly alone. Documented in all
    three affected `DefaultTimeout` doc comments (`pkg/expr`,
    `pkg/jspiece`) as a real, load-bearing limitation: these timeouts
    protect against runaway *interpreted* execution (loops, JS function
    calls) but provide no bound whatsoever on a single expensive native
    built-in call. No fix attempted — bounding native call cost would mean
    either patching goja or abandoning the calling goroutine after a
    deadline while the native call keeps running unsupervised in the
    background, both bigger changes than this testing pass was scoped for.
- **`pkg/catalog`'s `FileStore` treats a `Definition.Name` as untrusted
  input, not a safe path segment — a deliberate guard, not an
  afterthought.** A piece's `Name` can be agent-authored (that's the whole
  point of this package), and `FileStore` maps it directly to a filename
  under its store directory. Without validation, a `Name` like
  `"../../../etc/cron.d/x"` would let a save escape the store directory
  entirely. `FileStore.path` rejects any name containing a path separator
  or equal to `"."`/`".."` outright rather than trying to sanitize/clean
  it — same "reject, don't try to fix adversarial input" posture as
  `pkg/jspiece`'s guarded `ctx.run.*` hooks.
  `TestFileStore_RejectsPathTraversalNames` covers both separator styles
  (`/` and `\`, since this project runs on Windows) plus the bare `.`/`..`
  cases. Scoped narrowly to what `catalog` actually needs — this is not a
  general path-sanitization utility, and nothing else in this project
  currently maps caller-supplied strings to filesystem paths this
  directly.
- **`TestAIFirst_AllFourPhasesTogether` (`pkg/pieces/ai_first_test.go`) is
  the capstone test for the whole AI-first direction — every phase
  combined in one flow, none hand-waved.** An agent authors a JS piece
  with no Go catalog equivalent (`order_utils.summarize`); it's saved
  through a `GatedStore` (Phase 3's quality gate actually runs its
  `Example` before allowing the save) onto a `FileStore` (real disk
  persistence). A *separate* `FileStore` instance, pointed at the same
  directory, stands in for a later session — nothing but the directory
  carries over. The flow chaining that persisted piece with three real Go
  catalog pieces (`http`, `json`, `text`, `storage`) exists only as a JSON
  string (Phase 1) — never a Go struct literal. `flowvalidate.Validate`
  (Phase 4) checks the parsed flow against the full registry before it's
  ever executed. Passed on the first real run (after a sloppy first draft
  — a leftover unused placeholder block and a dead import — was caught and
  cleaned up before running, not left in). No engine or piece changes were
  needed to make four independently-built subsystems compose this way;
  each one's own "goes through the same path as any other piece/flow" design
  choice is what made this work rather than needing a fifth, glue-specific
  mechanism.
- **JS triggers (`jspiece.NewTrigger`) found the same class of gap the
  adversarial battery already found for actions — checked for directly
  rather than assumed fixed by analogy.** Read `Engine.ExecuteBegin`
  before writing `buildTriggerContext`: it constructs a PIECE trigger's
  `piece.TriggerContext` as `{Payload, Input}` only —
  `Store` is never set, meaning `ctx.store` is always unavailable for a
  trigger invoked through a real flow run. Guarded `ctx.store.get`/`put`
  against a nil `Store` the same way `ctx.files`/`ctx.run` already are for
  actions (same fix, same reasoning — a real trigger built this way could
  otherwise panic with an uncaught nil-pointer dereference).
  `TestJSTrigger_StoreUnavailableFailsClearlyNotPanic` covers it directly.
  The polling-cursor pattern (`ctx.store.get("lastId")`, filter, advance)
  still works and is genuinely useful — just only when a caller invokes
  `trig.Run()` directly and reuses one `Store` across repeated calls
  itself (`TestJSTrigger_PollingCursorFiltersOnlyNewItems`), matching
  `piece.MemoryStore`'s own doc comment about simulating a polling
  scheduler — not when driven through `ExecuteBegin`.
  `TestCatalog_JSTriggerComposesWithRealCatalog`
  (`pkg/pieces/integration_test.go`) proves a JS trigger works as a real
  flow's entry point registered alongside the full Go catalog, and
  confirms (by asserting on it directly, not assuming) that
  `ExecuteBegin` only ever uses `items[0]` of what a PIECE trigger
  returns — a second/third item in the array is never seen by that flow
  run, matching what reading `engine.go` already showed.
- **JS Dropdowns (`jspiece.NewDropdown`) needed a two-argument JS function
  (`(propsValue, ctx) => value`), unlike actions/triggers' single `ctx`
  argument — `compileAndRun` was generalized from a fixed one-argument
  signature to variadic (`args ...any`) rather than adding a
  dropdown-specific parallel code path.** Small, mechanical change (call
  sites passing one argument keep working unchanged, since a lone `any`
  argument to a variadic parameter is just a one-element slice); worth
  noting because it's the kind of shared-core refactor this project
  prefers over duplicating the compile/timeout/error-handling boilerplate
  a third time. The returned value is validated strictly against
  `piece.DropdownState`'s shape — a non-object return, or a non-object
  entry inside `options`, fails clearly (`TestJSDropdown_NonObjectReturnFailsClearly`,
  `TestJSDropdown_NonObjectOptionFailsClearly`) rather than silently
  producing an empty/wrong dropdown. `TestCatalog_JSDropdownComposesWithRealCatalog`
  resolves a JS dropdown through `Engine.LoadOptions` specifically (not
  `DropdownProperty.LoadOptions` called directly) — the same public API
  path a real editor UI would call, registered alongside the full Go
  catalog.
- **Tested a flow actually USING a value a JS Dropdown offered, not just
  resolving the dropdown in isolation — and confirmed a fact worth
  stating plainly: Dropdowns are advisory metadata only, the engine never
  enforces them.** `TestFlow_UsesValueSelectedFromJSDropdown`
  (`pkg/pieces/integration_test.go`) simulates the real editor workflow
  end to end: call `Engine.LoadOptions` to discover valid values, pick one
  from what actually came back (`state.Options[1].Value`, not
  hardcoded), bake that exact value into a flow's JSON `Input` (Phase 1
  style), `flowvalidate.Validate` it, then run it for real — the action
  receives and correctly uses the dropdown-sourced value.
  `TestFlow_RegionOutsideDropdownFailsClearly` is the other half: a value
  the dropdown never offered (`"ap-south-1"`) still reaches the action's
  `Run` completely unchanged — neither `Engine.ExecuteBegin` nor
  `flowvalidate.Validate` know or care what a `Dropdown`'s `Options` are.
  The only reason that flow fails is `regionPickerPiece`'s own JS
  throwing on an unrecognized region — exactly the same "the engine
  doesn't enforce a schema, the piece validates its own Input if it
  cares" philosophy already true for every other Input field in this
  project, now confirmed to hold for Dropdown-backed ones too rather than
  assumed by analogy.
