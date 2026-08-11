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
  not this package, and now covers actions (`catalog.Definition`),
  triggers (`catalog.TriggerDefinition`), and dropdowns
  (`catalog.DropdownDefinition`).
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
  Definition in a Store straight into an `Engine`'s registry.
  `catalog.TriggerDefinition` and `catalog.DropdownDefinition` are the
  symmetric persisted forms for JS-authored triggers and dropdowns —
  same `Store` interface, loaded back into an `Engine` alongside the
  actions, so a trigger or dropdown an agent authors survives a restart
  the same way an action does. Two `Store`
  implementations, same "simple in-memory default, real persistence is
  the caller's choice" convention as `piece.Store`/`FileWriter`:
  `MemoryStore` (dies with the process, mainly for tests) and `FileStore`
  (one JSON file per piece in a directory — real, actual disk persistence
  across restarts, zero new dependencies; a save is atomic — encode into
  a temp file in the same directory, then `os.Rename` it into place, so a
  crash mid-save never leaves a half-written piece file). `catalog.Describe(store)`
  renders every piece's name/description/actions/input-schema as plain
  text meant to be handed directly to an agent's context before it
  decides whether to reuse something instead of authoring a piece again —
  deliberately plain text for a language model to read, not a search
  index or embedding lookup; a program that needs the data structurally
  should call `store.List()` directly. `catalog.DescribeCombined(store,
  goCatalog)` renders the Go-native catalog and the persisted JS catalog
  side by side in one plain-text block, so an agent sees both in a single
  context instead of having to be told the Go catalog exists separately.
  A quality gate now covers whether a
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
  `pkg/engine`'s flow tests. A 5s execution deadline
  (`sandbox.DefaultTimeout`, via the new `InterruptAfter` helper) bounds a
  runaway/infinite-loop CODE step — added late, closing the same class of
  gap `pkg/expr` and `pkg/jspiece` already had fixed: a CODE action's
  `Source` can arrive as agent-authored JSON data (Phase 1) exactly like a
  JS piece's or a template's, and `pkg/flowvalidate` only checks that it
  *compiles*, never how it behaves at runtime. Same confirmed limitation
  as the other two: a native built-in call already in progress isn't
  preempted by the interrupt.
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
  key. `ActionContext.Store` is the action-side counterpart, added later to
  close a gap TICKETS.md documented as open ("a `Store`-using action piece"
  wasn't possible — `ActionContext` had no `Store` field at all): unlike
  `TriggerContext.Store`, which `Engine.ExecuteBegin` never populates (a
  trigger's cross-poll memory is an external scheduler's job, outside this
  project), `*Engine` DOES wire this one in — `Engine.Store`, defaulting to
  a fresh `MemoryStore` in `New()` — for every PIECE action it runs, through
  both `ExecuteBegin` and `ExecuteActionRun`, no caller wiring needed. Same
  "one shared instance, wrap in `ScopedStore` or use one `Engine` per tenant
  for isolation" convention as `Engine.Files`. There's no
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
- **A small piece catalog** (`pkg/pieces`): seventeen independently-tested,
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
  built across `pkg/engine`'s own tests), `schedule` (a real POLLING trigger
  — fires at most once per configured `intervalSeconds`, inert on its own
  until `pkg/scheduler` actually calls it on a timer; see the `pkg/scheduler`
  entry below), `crypto` (AES-GCM
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
  result, not an error, same philosophy as `failOnErrorStatus`), `csv`
  (parse/stringify, `hasHeader` toggles row shape between `[][]string` and
  `[]map[string]any`), `uuid` (RFC 4122 v4 generation, `crypto/rand` +
  `encoding/hex`, no third-party UUID library), `base64` (encode/decode
  text via `encoding/base64`), and `email` (send via `net/smtp.SendMail`,
  `smtp.PlainAuth` when `ctx.Auth` is a `"user:password"` string,
  unauthenticated otherwise — tested against a hand-rolled, stdlib-only
  fake SMTP server). `pieces.RegisterAll(registry)` registers all
  seventeen in one call; `pieces.All()` if you want to filter
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
- **HTTP API server** (`pkg/httpapi` + `cmd/server`): the first process
  wrapped around the library — `net/http` + `crypto/subtle`, no new
  dependency. `GET /health` (unauthenticated liveness), `GET /catalog`
  (`catalog.DescribeCombined`), `POST /pieces` (saves through the same
  `GatedStore` quality gate) and `DELETE /pieces/{name}` (no gate needed —
  see the MCP meta tools entry below for why), `POST /flows/run` (ad-hoc: a
  `model.FlowVersion` in the request body, validated then executed, the
  full `*model.ExecutionState` as the response) — all gated by a single
  shared bearer token (`GOFLOW_API_TOKEN`, compared via
  `crypto/subtle.ConstantTimeCompare`) except `/health`, the
  unauthenticated-by-necessity OAuth endpoints (`pkg/oauth`, below — they
  ARE the mechanism a client without a token yet uses to get one), and
  `POST /webhooks/{name}` (below — a third party has no way to know that
  token either; gated a different way, see its own entry). An
  OAuth-issued access token is accepted everywhere the static token is,
  through the same check. `cmd/server/main.go`
  refuses to start without the token configured — never boots an
  unauthenticated code-execution endpoint by accident. Deployed as a
  systemd service on a real VPS (binary cross-compiled
  `GOOS=linux GOARCH=amd64`, bound to `127.0.0.1` only, never exposed past
  loopback).
- **Encrypted credential store** (`pkg/credentials`): built for a case this
  project didn't have an answer for — an *agent*, not a human, needs to
  supply a piece's `ctx.Auth` without that secret ever traveling or resting
  in plaintext. AES-256-GCM, one `.enc` file per credential (fresh random
  nonce per save, atomic write — same `CreateTemp`+`Rename` pattern as
  `catalog.FileStore`), key from `GOFLOW_CREDENTIALS_KEY` (32 bytes, hex,
  mandatory — same fail-closed startup as the API token).
  `POST`/`GET`/`DELETE /credentials` only ever handle names — there is no
  HTTP route that returns a decrypted value, by design; `Store.Get` is for
  trusted Go callers only.
- **Credential references inside a flow** (`flowstore.ResolveCredentials`/
  `RedactCredentials`, wired in as `flowstore.RunWithCredentials` — itself
  now wrapped by `flowstore.RunWithHistory`, see the run-history entry
  below): the reference mechanism the credential store above was missing. Any Input
  value (trigger, PIECE, or CODE, any key) can be
  `{"$credential": "<name>"}` instead of a literal; `ResolveCredentials`
  substitutes the real decrypted value into a deep-enough COPY of the flow
  before it runs (the stored/caller-held `*model.FlowVersion` is never
  mutated), and `RedactCredentials` walks the resulting `ExecutionState`
  — including every `LOOP_ON_ITEMS` iteration, recursively — replacing each
  substituted value with `<credential:name>` afterward. Both passes are
  required together: the engine records each step's already-resolved Input
  verbatim, and `POST /flows/run`, `POST /flows/{name}/run`, and MCP's
  `tools/call`/`goflow_run_flow` all serialize that Input straight back in
  the response — without the redaction pass a resolved secret would leak
  into the HTTP/MCP reply. Every way to run a flow — ad-hoc, named, MCP
  `tools/call`, MCP `goflow_run_flow`, `POST /webhooks/{name}`, and
  `pkg/scheduler` (all below) — goes through `RunWithCredentials` (via
  `RunWithHistory`) now, not just `flowstore.Run` directly. A flow
  referencing a credential that isn't stored fails as a validation error
  (400/`isError`), the same
  category as referencing a piece that doesn't exist — not a 500.
  Verified end-to-end over real HTTP and MCP with the raw response body
  searched for the literal secret (absent) alongside proof the real value
  reached the piece.
- **Named flow persistence** (`pkg/flowstore`): closes the gap between "run
  a flow once, ad-hoc" (`POST /flows/run`) and "a flow is a reusable,
  discoverable thing" — the prerequisite for exposing flows as MCP tools
  below. `FileStore` mirrors `catalog.FileStore`'s pattern exactly;
  `GatedStore` runs `flowvalidate.Validate` against a freshly-built piece
  registry before every save (never cached — a flow must validate against
  the catalog *as it is now*, including JS pieces registered after the
  process started), rejecting a flow that references a missing piece before
  it ever reaches disk. `POST`/`GET /flows`, `GET`/`DELETE /flows/{name}`,
  and `POST /flows/{name}/run` share the same execution path as the ad-hoc
  route (`flowstore.Run`/`RunWithCredentials`/`RunWithHistory`, extracted
  once a third caller — MCP, below — needed it too, so the transports
  can't drift apart in what "run a flow" means).
- **Webhook ingress** (`POST /webhooks/{name}`): the piece a persisted flow
  was still missing — a way for a THIRD PARTY (Stripe, GitHub, anything
  that can't know `GOFLOW_API_TOKEN`) to actually trigger one. Deliberately
  NOT behind `s.auth` — the whole point is a caller with no token — gated
  instead by two things checked in order: the flow must have
  `FlowDefinition.WebhookEnabled` set (off by default, so saving a flow
  never silently creates a public endpoint), and if
  `WebhookSecretCredential` names a credential, the request's
  `X-Webhook-Secret` header must match it, constant-time compared, failing
  CLOSED (401) if the credential can't be resolved at all. That field is a
  credential *reference*, not a plain string on `FlowDefinition` — a plain
  field would put the secret in `flowstore`'s own plaintext JSON file,
  exactly what `pkg/credentials` exists to avoid; this is the same
  encrypted-at-rest store `$credential` markers already use, applied to a
  second concern. A shared secret is only a floor, not a replacement for
  real signature verification — a provider that signs its payloads
  (GitHub's `X-Hub-Signature-256`, Stripe-Signature, ...) should still
  verify that signature itself, inside the flow, using the `hash` piece's
  existing HMAC support. An unknown flow name and a known-but-disabled one
  get the identical 404, so an outside caller can't probe for valid names.
  The request body is decoded as JSON and becomes the trigger payload
  directly (no `{"trigger": ...}` envelope — a webhook sender has no reason
  to know goflow's own request shape). The response is the other half of
  what makes this route different from every other run-triggering one:
  `model.ExecutionState.RespondedEarly`'s own doc comment says plainly
  "there is deliberately no simulated HTTP layer here to actually deliver
  it" — true at the engine layer, but `/webhooks/{name}` IS that HTTP
  layer, the first transport in this project where a real outside caller is
  actually waiting on `ctx.Run.Stop`/`Respond`'s reply, so it delivers that
  status/body/headers verbatim instead of dumping the full
  `ExecutionState` (which would leak internal step Input/Output to an
  unauthenticated caller by default). Neither hook fired: a deliberately
  generic ack — `{"status":"ok"}`/200 on success, `{"status":"failed"}`/500
  otherwise, no step-level detail. Goes through the same
  `flowstore.RunWithHistory` as every other transport, so a webhook-
  triggered run shows up in `GET /runs` exactly like any other.
- **Persistent run history** (`pkg/runstore`, wired in as
  `flowstore.RunWithHistory`): closes the gap this file used to describe
  under "Explicitly NOT in v1" — a `Verdict`/`Steps` map that existed only
  for the duration of one call and was returned to the caller, never stored.
  `RunWithHistory` wraps `RunWithCredentials` (credentials already resolved
  *and redacted* by the time a run reaches it, so a secret never gets as far
  as the history store) and is now the single path every way to run a flow
  calls — `POST /flows/run`, `POST /flows/{name}/run`, MCP's `tools/call`
  and `goflow_run_flow`, `POST /webhooks/{name}`, and `pkg/scheduler` — so a
  run is recorded identically regardless of which one triggered it. Unlike `catalog.Store`/`credentials.Store`/`flowstore.Store`,
  a run record has no caller-chosen name to key on, so `Store.Save` assigns
  a fresh random id and hands it back, the way a database insert returns a
  generated primary key. A run is recorded on both success AND failure (a
  FAILED verdict belongs in history exactly as much as a SUCCEEDED one) but
  NOT when the flow never actually ran — a validation failure or a
  registry-build fault records nothing, the same reasoning a rejected
  malformed HTTP request elsewhere in this project doesn't get logged as a
  completed one. `GET /runs` lists every run (metadata only — a run's full
  state can be large); `GET /runs/{id}` returns the full record. Same
  on-disk shape as the other three stores: one JSON file per record, atomic
  write, zero new dependencies.
- **Flows as MCP tools** (`pkg/mcpapi`): a hand-written JSON-RPC 2.0
  transport — no SDK, no streaming SSE, no sessions — mounted at
  `POST /mcp` under the same bearer auth as everything else. Implements the
  four methods needed for an LLM client to discover and call goflow's saved
  flows as tools: `initialize`, `notifications/initialized`, `tools/list`
  (one tool per saved flow; `inputSchema` is a deliberately permissive
  `{type: object, additionalProperties: true}` carrying
  `FlowDefinition.InputSchema`'s free text as its `description`, since that
  field isn't a formal JSON Schema), and `tools/call` (the call's
  `arguments` become the flow's trigger payload). An unknown tool name is a
  JSON-RPC protocol error (`-32602`); a *known* flow that fails validation
  or execution comes back as a normal tool result with `isError: true` —
  the two are kept distinct on purpose, since only the first is a client
  mistake. Verified against a real client, not just hand-built JSON-RPC
  requests: the official `@modelcontextprotocol/inspector` CLI, run on the
  same VPS against the deployed server, completed a real
  `initialize`/`tools/list`/`tools/call` sequence and correctly surfaced an
  unknown-tool error. The real, load-bearing limitation this found — auth
  was the same plain bearer token as every other route, not the OAuth 2.1
  flow the MCP spec defines for remote HTTP servers, so a real MCP client
  that only speaks that flow got stuck retrying OAuth discovery on a
  401 — is closed now; see `pkg/oauth` below.
- **OAuth 2.1 for MCP** (`pkg/oauth`): closes the gap the `pkg/mcpapi` entry
  above used to end on. Deliberately NOT general-purpose multi-user OAuth —
  goflow still has no concept of accounts (see "Explicitly NOT in v1"
  below) — there is exactly one principal, whoever knows
  `GOFLOW_API_TOKEN`, the same secret every other route already trusts.
  `/oauth/authorize` grants an authorization code to that principal and no
  one else: it requires the token presented either as a Bearer header (a
  machine/CLI client that already knows the token, e.g. the MCP Inspector's
  `--header` flag) or, for a real browser redirect that can't set a header,
  typed into a one-field HTML form served at the same URL — no separate
  login/account system behind that form, just the same constant-time
  compare every other route already does. Auto-approving every
  `/oauth/authorize` request with no credential check at all was
  considered and rejected: it would let anyone who can merely reach the
  server mint a fully valid access token without ever knowing
  `GOFLOW_API_TOKEN` — strictly weaker than today, not a neutral addition.
  What IS real, stdlib-only, zero new dependencies: RFC 8414
  authorization-server metadata and RFC 9728 protected-resource metadata
  (both served unauthenticated, as they must be), RFC 7591 dynamic client
  registration (also unauthenticated — registering a client identifies it
  for redirect_uri matching but grants no access by itself), mandatory PKCE
  (S256 only — OAuth 2.1 drops "plain") on every authorization code, and
  short-lived single-use codes exchanged for opaque access/refresh tokens
  (refresh rotates on every use, old refresh tokens are dead the moment a
  new pair is issued). `/mcp`'s 401 carries a `WWW-Authenticate: Bearer
  resource_metadata="..."` header (RFC 9728) pointing a compliant client at
  the metadata instead of leaving it to retry the same bare token forever
  — the exact failure mode the `pkg/mcpapi` finding above described. An
  OAuth-issued access token and the static token grant identical access on
  every route (this project has no scopes/permissions concept to narrow
  one against), not just `/mcp`.
- **MCP meta tools — read and write** (`pkg/mcpapi`): closes a gap specific
  to a client that only speaks MCP, not HTTP — `tools/list`/`tools/call`
  used to only DISCOVER-and-RUN a flow someone else already built; browsing
  the piece catalog, listing/saving/deleting flows, authoring a piece, or
  managing credentials all required falling back to raw HTTP with the
  bearer token, even though an MCP-authenticated caller (static token or an
  OAuth access token) already has that exact same access on every httpapi
  route today — there's no scopes/permissions concept anywhere in this
  project to make MCP meaningfully narrower, so none of this is a new
  privilege, only new reachability. Fifteen fixed tools are always present
  in `tools/list` alongside the per-flow ones, each with a real, precise
  `inputSchema` (unlike a per-flow tool's deliberately permissive one,
  since these have well-defined Go types behind them, not an untyped
  trigger payload) — EXCEPT `goflow_save_flow`'s `flow` argument and
  `goflow_save_piece`'s `actions`/`triggers`, which stay loosely typed on
  purpose: they're recursive, variant-typed structures, the same "not a
  formal JSON Schema for v1" territory `FlowDefinition.InputSchema` and
  `ActionDefinition.InputSchema` already occupy elsewhere in this project,
  for the same reason. Shipped in two tiers so mutating power didn't land
  bundled with what was initially a pure discoverability improvement:
  - Read-only: `goflow_describe_catalog`, `goflow_export_catalog` (the MCP
    equivalent of `GET /pieces/export` — every JS-authored piece's FULL
    `Definition`, source and examples included, not `goflow_describe_
    catalog`'s lossy text; feed one entry straight back into
    `goflow_save_piece` to recreate it elsewhere. Built-in Go pieces have
    no `Definition` — native code, not data — and never appear here),
    `goflow_list_flows`, `goflow_get_flow`, `goflow_list_runs`,
    `goflow_get_run`, `goflow_export_flow_js` (the MCP equivalent of
    `POST /flows/export/js` and `POST /flows/{name}/export/js` combined
    into one tool — takes exactly one of `name` or `flow`, since export
    runs no code and persists nothing either way, unlike `goflow_run_flow`
    below).
  - Write: `goflow_save_flow` and `goflow_delete_flow` (through the exact
    same `*flowstore.GatedStore` `POST`/`DELETE /flows` use — a flow
    referencing a missing piece is rejected, never partially saved);
    `goflow_save_piece` and `goflow_delete_piece` (through the exact same
    `*catalog.GatedStore` `POST`/`DELETE /pieces` use — every
    action/trigger/dropdown's examples are actually RUN before a save
    persists, one failing example rejects the whole piece, and delete
    needs no gate at all, since removal can't fail a quality check the
    way a save can — same reasoning `flowstore.GatedStore.Delete` already
    documents); `goflow_list_credentials`, `goflow_save_credential`,
    `goflow_delete_credential` (a credential's value is never echoed back
    in a tool result, matching `POST /credentials` exactly — `Store.Get`,
    the only thing that ever returns a decrypted value, is for trusted Go
    callers only and is never wired to any transport, MCP included).
    `goflow_delete_piece` closes a gap found while smoke-testing the rest
    of this tier live: nothing, on ANY transport, could remove a catalog
    piece before this — `catalog.Store` itself had no `Delete` method at
    all, so `DELETE /pieces/{name}` is new over HTTP too, not just MCP.
  - Neither, exactly: `goflow_run_flow` is the MCP equivalent of
    `POST /flows/run` — runs a flow inline WITHOUT saving it, recorded in
    `goflow_list_runs`/`GET /runs` with an empty flow name like any other
    ad-hoc run. It has the same real side effects any flow run can (a
    PIECE action can call a real API, use a credential, ...) but persists
    nothing of its own, so it doesn't fit either tier above cleanly — it
    closes the one asymmetry left after the rest of this tier shipped: an
    MCP-only client could save/delete/run a *named* flow but had no way to
    try an inline one first, the way an HTTP caller already could before
    ever committing to `goflow_save_flow`.

  A saved flow (or, symmetrically, a credential name) that collides with
  one of the fifteen reserved names is excluded from `tools/list` — not
  deleted, not un-runnable/un-referenceable by name over HTTP — just
  shadowed in this one listing, and `tools/call` resolves that name to the
  fixed tool the same way, so the two methods never disagree about what a
  reserved name means. A gate-rejected save is a tool RESULT with
  `isError: true` (the caller-supplied flow/piece is broken, not a server
  fault) — same category `tools/call` already uses for a broken flow it's
  asked to *run*.
- **Schedule trigger + a real scheduler** (`pkg/pieces/schedule`,
  `pkg/scheduler`): closes a gap found while auditing real-world use cases
  against this project's actual catalog — every other piece here is
  event/on-demand (`webhook`, or an action run manually/via MCP), nothing
  fires a flow on its own on a timer, and n8n-style incidents from exactly
  that pattern (hundreds of 1-second schedule triggers erroring in a loop
  after a dependency vanished) are the direct reason to build this
  carefully rather than by reflex. `schedule.Run` is a real POLLING-style
  `piece.Trigger` (see `pkg/piece`'s own doc comment on what that means):
  it takes `intervalSeconds` from `Input` and a cursor from `Store`, fires
  (and reseeds the cursor) once the interval has elapsed since it last did,
  and — like every polling trigger in this project — fires immediately on
  its very first call for a fresh cursor, the same "existing data counts as
  new on the first poll" shape `TestPollingTrigger_*` already established.
  The piece alone is inert, though: `pkg/piece.Trigger`'s doc comment is
  explicit that calling `Run` on a timer is an external scheduler's job,
  and before this, nothing in `cmd/server`/`pkg/httpapi` did that —
  `TestPollingTrigger_*` only ever simulated it inside a test. `pkg/scheduler`
  is that external scheduler for real: it ticks on its own interval
  (`GOFLOW_SCHEDULER_TICK`, default 1s — how often it CHECKS, not any one
  flow's own `intervalSeconds`), lists every saved flow each tick, asks the
  schedule-triggered ones whether they're due (each against its own
  `piece.ScopedStore`-namespaced cursor, so two schedule-triggered flows
  never clash over the same literal cursor key — same reasoning
  `ScopedStore`'s doc comment already gives for exactly this "multiple
  flows, one trigger mechanism" shape), and for a due flow runs it through
  `flowstore.RunWithHistory` — the identical path every other transport
  already shares, so a schedule-fired run resolves and redacts credentials
  and lands in run history exactly like a manually triggered one, not a
  new, subtly different way to run a flow.
  In-memory cursors only, deliberately (a restart re-fires every
  schedule-triggered flow once, immediately — the same first-call behavior
  a brand-new flow already has, not a new failure mode); a per-flow
  in-flight guard skips a tick for a flow whose previous scheduled run
  hasn't finished yet, rather than overlapping runs of the same flow.
  `pkg/catalog/registry.go`'s `BuildRegistry` — extracted from what was
  inline in `Server.buildRegistry` — is what lets `pkg/scheduler` validate
  and run a flow against the exact same piece registry every other
  transport uses, without duplicating that assembly a third time.
- **Export a simple linear flow to standalone JavaScript** (`pkg/exportjs`,
  wired in as `POST /flows/export/js`, `POST /flows/{name}/export/js`, and
  MCP's `goflow_export_flow_js` — see the MCP meta tools entry below):
  a flow made of an `EMPTY` trigger plus a linear chain of `CODE`-only
  actions — no `ROUTER`, `LOOP_ON_ITEMS`, or `PIECE` action, no
  `PIECE_TRIGGER` — can be exported to a single, self-contained `.js` file
  with no goflow/goja/Go dependency at all, runnable directly in Node or a
  browser. `exportjs.Supported` reports every unsupported trigger/action a
  flow has (not just the first), and `exportjs.Export` refuses to produce
  output for a flow that fails it, naming each violation.
  The generated file isn't an approximation: `{{ }}` template resolution is
  re-hosted as real JS (`new Function(...)` against the same named
  step-scope bindings `pkg/expr` builds — the same approach `pkg/expr`
  itself takes, since it also evaluates `{{ }}` content as real JS through
  goja, just re-hosted here instead of in goja), and the retry/backoff
  (`maxAttempts: 4`, exponential base 2, 2s base interval),
  `ContinueOnFailure`, `Skip`, and chain-walking/verdict logic mirror
  `pkg/engine` exactly. The output shape matches `model.ExecutionState`
  field-for-field, including the always-present `Iterations`/`LastItem`/
  `LastIndex` on every step (Go's `json.Marshal` includes them regardless —
  no `omitempty` tag on those three fields), so tooling built against a
  normal goflow run's output works against an export's output unchanged.
  Verified end-to-end: the same flow, run through the real engine and
  through the exported JS under real Node.js, produced identical output
  (`Steps`, `Verdict`) field-for-field except `DurationMs`.
  Known, disclosed differences from running the same flow through goflow
  itself: no per-step execution timeout (`goja.Interrupt` has no
  synchronous-JS equivalent in a real engine); relies on `new Function(...)`
  for dynamic code evaluation (fine in Node/browsers unconditionally, but
  worth checking against a CSP-restricted deployment target); an async
  (Promise-returning) `CODE` step is rejected, matching `pkg/sandbox.Run`'s
  synchronous-only rule exactly.
- **Run a single fixed flow on AWS Lambda** (`cmd/lambda`): a second,
  much narrower deployment target than `cmd/server`, built after
  confirming the *whole server* doesn't actually work on Lambda — its
  four Store-backed pieces (catalog/flowstore/credentials/runstore) are
  all local-`FileStore`, which don't survive Lambda's many parallel,
  ephemeral execution environments; `pkg/oauth`'s state is in-memory only
  (its own doc comment says so); and `pkg/scheduler` needs a continuously
  running process, which Lambda's freeze-between-invocations model
  doesn't give it. `cmd/lambda` sidesteps all four instead of fixing
  them: the one flow it runs is embedded in the binary at BUILD time
  (`cmd/lambda/flow.json`, via `go:embed`) rather than loaded from a
  Store at runtime, so there's no persistence for a cold/parallel
  instance to ever disagree about, and no runtime flow-authoring surface
  for OAuth or a scheduler to need to gate or drive. It calls
  `pkg/engine.ExecuteBegin` directly — no HTTP, no MCP — with the Lambda
  invocation event decoded as the trigger payload, and returns the full
  `*model.ExecutionState` as the response, the same "arbitrary JSON in,
  full ExecutionState out" contract `POST /flows/run` and
  `goflow_run_flow` already share. Because it runs the real engine (not
  a re-hosted subset like `pkg/exportjs`), it supports the full action
  set — `ROUTER`, `LOOP_ON_ITEMS`, real `PIECE` actions — not just a
  linear `CODE` chain. The registry is built from only `pkg/pieces.All()`
  (the built-in Go pieces), the same two-line construction
  `pkg/catalog.BuildRegistry` uses for its own built-in half, just
  without a Store to layer anything else on top of; a `PIECE` action
  needing a real secret must have it baked into `flow.json` directly,
  since `pkg/engine` has no concept of a `$credential` marker at all —
  that substitution is `pkg/flowstore`'s job, sitting above the engine on
  every other transport, and there is no `pkg/flowstore` in this path.
  Deploying a different flow means replacing `flow.json` and rebuilding.
  This is the one new dependency in the project beyond
  `github.com/dop251/goja`: `github.com/aws/aws-lambda-go`, to receive
  invocations the way AWS actually documents and maintains, rather than
  hand-rolling a client against the Lambda Runtime API.
- **Export the piece catalog** (`GET /pieces/export`, MCP's
  `goflow_export_catalog`): the piece-authoring story was always "the AI
  populates the catalog itself" (see JS-authored pieces above), so the
  actual gap wasn't authoring — it was that nothing could get a saved
  piece's real content back OUT in a re-importable shape.
  `catalog.DescribeCombined` (`GET /catalog`, `goflow_describe_catalog`)
  is deliberately lossy plain text for a model to READ, not parse — it
  never included `Source` or `Examples`. `catalog.Store.List()` already
  returned the full `Definition` internally (exactly what `Save` accepts,
  making a round trip mechanically trivial), just never exposed through
  any route — this closes that. Only JS-authored pieces are covered:
  built-in Go pieces (`pkg/pieces.All()`) are native code, not data, so
  they have no `Definition` and never appear in the export; `GET
  /catalog`'s text still describes them alongside the JS-authored ones.
- **Declare a piece action's credential need** (`ActionDefinition.
  RequiresAuth`): sharing a catalog piece (see export above) surfaced a
  real gap once pieces started moving between agents — an action reading
  `ctx.auth` (`piece.AuthInputKey`) had no way to say WHAT it needs before
  a caller hit a runtime failure from a missing/wrong credential.
  `RequiresAuth` is free text on `ActionDefinition` (same "not a formal
  schema" reasoning as `InputSchema` — `ActionContext.Auth` is `any` with
  no engine-enforced shape, so there's nothing to validate this against),
  e.g. `"Slack Bot Token (string, starts with xoxb-)"`. It needed no new
  route: it rides along automatically wherever a `Definition` already
  does (`GET /pieces/export`, `goflow_export_catalog`, `POST /pieces`,
  `goflow_save_piece`) and now also renders as a `requires auth:` line in
  `catalog.DescribeCombined`'s text (`GET /catalog`,
  `goflow_describe_catalog`) — the tool an agent is already told to call
  FIRST before authoring a flow around a piece, so the credential need
  surfaces at the earliest possible read, not after a failed run.
- **On-failure notification** (`flowstore.FlowDefinition.OnFailureFlow`,
  `flowstore.TriggerOnFailure`): a webhook- or scheduler-fired run has no
  human watching it in real time — without this, the only way to learn a
  run failed was polling `GET /runs`. `OnFailureFlow` names ANOTHER saved
  flow to run whenever this one's `Verdict.Status` ends up `FAILED`; empty
  (the default) disables it entirely, same "off unless opted in" shape as
  `WebhookEnabled`. Rides along for free through every existing save path
  (`POST /flows`, `goflow_save_flow`) since `FlowDefinition` decodes
  generically. `TriggerOnFailure` itself needed no change to
  `RunWithHistory` — the one function every transport already shares — it's
  a small SEPARATE helper each of the four run-triggering call sites
  (`POST /flows/{name}/run`, `POST /webhooks/{name}`, MCP `tools/call`'s
  named-flow path, and `pkg/scheduler`'s tick loop) calls right after its
  own existing `RunWithHistory`, since all four already have the
  `FlowDefinition` in scope there. The on-failure flow runs through that
  same `RunWithHistory` path too — recorded in history under its own name,
  credentials resolved the same way as any other run — with a trigger
  payload of `{flowName, failedStepName, failedStepDisplayName,
  failedMessage}`. Deliberately synchronous (blocks the caller until the
  on-failure flow itself finishes — the alternative, firing it in a
  goroutine, would be untestable without a synchronization hook, and this
  project has no flaky/async tests anywhere) and capped at exactly one hop
  (the on-failure flow's own `OnFailureFlow`, if it has one, is never
  read) — a circular pair (A's on-failure is B, B's on-failure is A) can
  never loop. An ad-hoc run (`POST /flows/run`, MCP `goflow_run_flow`) has
  no `FlowDefinition` to read this from, so it never applies there — same
  scoping `WebhookEnabled` already has.
- **Sub-flow composition** (a new `CALL_FLOW` action type, `model.
  CallFlowSettings`, `engine.CallFlowFunc`): the piece-authoring story was
  always "the AI generates pieces and populates the catalog itself," so
  the real gap for a growing, AI-authored catalog wasn't more pieces — it
  was that every flow was an island; nothing let one flow call ANOTHER
  saved flow as a step, the way n8n's "Execute Workflow" node does.
  `pkg/engine` itself can't import `pkg/flowstore` (that would cycle —
  `flowstore` already imports `engine`), so `Engine.CallFlow` is a plain
  hook function (`engine.CallFlowFunc`) the engine calls for a `CALL_FLOW`
  action and otherwise knows nothing about; `pkg/flowstore` is the only
  place that actually builds a real one. `Run`/`RunWithCredentials` just
  pass whatever `CallFlowFunc` they're given straight through to the
  `Engine` unchanged; `RunWithHistory` is where the real implementation
  lives, since it's the one function with a flow `Store`, `credStore`,
  and `historyStore` all in scope together to build a working closure
  from. That closure resolves the target by name, recurses back into
  itself through the exact same `RunWithCredentials` path (so a sub-flow's
  own `$credential` markers resolve exactly like a top-level run's do —
  not a special case), and records EACH hop as its own separate history
  entry under its own flow name — a 3-deep `CALL_FLOW` chain produces 3
  independently inspectable `GET /runs/{id}` records, not one. A
  `CALL_FLOW` step's Output is the sub-flow's FULL `*model.
  ExecutionState` (Steps + Verdict), not just its last step — the same
  "a flow ran" shape `POST /flows/run` and every other transport already
  return, reachable from a later `{{ }}` template exactly like the
  trigger step's output already is. A FAILED sub-run fails the calling
  step too (with the sub-run's own `FailedStep.Message` as this step's
  `ErrorMessage`), so retry/`ContinueOnFailure` apply exactly like a
  thrown `PIECE`/`CODE` error would — a failed sub-flow call is never
  silently treated as success. Two safety nets, since A calling B calling
  A is a real risk `OnFailureFlow`'s "cap at one hop" trick can't solve
  here (legitimate composition genuinely needs more than one hop): true
  cycle detection (the call path — every flow name already running in
  this call stack — is checked before each recursive call, catching A
  re-entering itself through any number of intermediate flows, not just
  a direct pair) plus a hard depth backstop (`maxCallFlowDepth`, 10) for
  a long-but-non-cyclic chain. `flowvalidate.Validate` only checks
  `CALL_FLOW` structurally (settings present, a non-empty target name,
  well-formed `{{ }}` templates in `Input`) — same as it already leaves
  `$credential` targets and `OnFailureFlow` targets unchecked, since it
  only ever receives a `*piece.Registry`, never a flow store; calling a
  flow that doesn't exist surfaces as a clean FAILED step at run time
  instead. `pkg/exportjs` rejects `CALL_FLOW` the same way it already
  rejects `ROUTER`/`LOOP_ON_ITEMS`/`PIECE` — a generated standalone JS
  file has no flow store to look another flow up in either.

## Explicitly NOT in v1

No UI, no piece marketplace, no streaming progress, no distributed workers.
This started as a library + example, the same scope as the activepieces TS
extraction's `example-standalone.ts` — a proof the mechanism works, not a
deployable product — but two things originally listed here as absent no
longer are, and should be stated plainly rather than left stale:

- **"No server/API" is no longer true.** `pkg/httpapi` + `cmd/server` is a
  real HTTP server (`/health`, `/catalog`, `/pieces*`, `/flows*`
  (including `/flows/export/js` and `/flows/{name}/export/js`),
  `/credentials*`, `/runs*`, `/mcp`, `/oauth/*`, `/.well-known/oauth-*`,
  `/webhooks/{name}` (deliberately public — see "What's here" above),
  deployed and
  running on a real VPS as a systemd service. "No auth" is also no longer true, but
  stays narrow: every non-public route requires either the single shared
  bearer token (constant-time compared) or an access token minted by
  `pkg/oauth`'s single-tenant OAuth 2.1 authorization server (see its "What's
  here" entry above) — there is still no per-user auth and no accounts;
  OAuth here authenticates the one existing principal through a
  spec-compliant flow, it does not add new principals.
- **"No persistence" is no longer true in general.** `pkg/catalog` persists
  piece definitions, `pkg/credentials` persists secrets encrypted at rest,
  `pkg/flowstore` persists named flows, and `pkg/runstore` persists a record
  of every run — all real, cross-restart disk persistence. None of them is a
  general-purpose database (no query language, no indexes beyond "list
  everything and filter client-side"), but the specific gap this section used
  to name — a `Verdict`/`Steps` map existing only for the duration of one
  call and never stored — is closed; see the `pkg/runstore` entry above.

Still true: no UI, no piece marketplace, no streaming progress (a request
gets one JSON response at the end, not incremental updates), no distributed
workers (`cmd/server` is one process; scaling it is the caller's problem).

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
  to do. That external scheduler this entry describes as missing is no
  longer just simulated in a test for one specific trigger shape: see the
  `pkg/scheduler`/`pkg/pieces/schedule` entry above for the real one, still
  entirely outside `pkg/engine` — the boundary this entry describes didn't
  move, it just got a real caller on the other side of it.
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
- **Chained/refreshing dropdowns (`cloudDeployPiece`,
  `pkg/pieces/integration_test.go`) needed zero new mechanism — confirmed
  by testing it, not assumed from `Refreshers`' own doc comment.**
  `piece.DropdownProperty.Refreshers` is explicitly documentation-only
  ("nothing enforces it... `LoadOptions` decides what it actually reads
  from `propsValue`"); the actual chaining is entirely `LoadOptions`
  reading whatever's in `propsValue` itself — a `"zone"` dropdown reading
  `propsValue.region` and returning different options per region is just
  ordinary JS logic, nothing `jspiece`-specific. `TestJSDropdown_ChainedOptionsDependOnSiblingPropsValue`
  proves it returns genuinely different option sets for different regions
  (not a static list ignoring its input);
  `TestJSDropdown_ChainedDropdownDisabledWithoutParentSelected` covers the
  realistic UX case (no region picked yet → `Disabled: true` with a
  placeholder, not an empty unexplained list).
  `TestFlow_UsesChainedDropdownSelections` runs the full real editor
  sequence end to end: load `"region"` options, pick one, load `"zone"`
  options WITH that region in `propsValue` (what `Refreshers` tells a real
  UI to send), pick one of those, then actually execute a flow built from
  both selections. `TestFlow_ZoneFromWrongRegionFailsClearly` goes one
  level past `TestFlow_RegionOutsideDropdownFailsClearly`: nothing stops a
  flow's `Input` from pairing a zone with a *different* region than the
  one it actually belongs to (hand-edited JSON, a stale value) — the
  chaining is purely an editor-time convenience for populating the list,
  never enforced at run time; only `cloud_deploy.deploy`'s own validation
  catches the mismatch.
- **`sandbox.InterruptAfter` was attempted through this project's CCDD
  task-contract delegation tooling first (small-model implementation
  against a signed, gated contract), abandoned after six good-faith
  attempts, and implemented directly.** The task contract's `## Examples`
  section repeatedly failed `lint_task_contract`'s `tc-sections` rule
  ("debe tener ≥2 ejemplos resueltos") across six structurally distinct
  formats — prose with two fenced code blocks, a numbered list with
  `→` input/output arrows, `### Ejemplo N` subheadings (matching the
  Spanish the authoring rubric itself specifies), literal English
  `Input:`/`Output:` labels, and a doctest-style `>>> ... # =>` form —
  every other rule passing cleanly along the way (fixed real, separate
  issues first: `budget` needed to be a map with specific keys, not a
  bare number; `spec_version` and `test_command` were required and
  missing; `## Do / Don't` needs that exact spacing). Six varied,
  reasonable attempts failing on one identical rule points at something
  in the checker's actual matching logic this project has no visibility
  into, not a formatting mistake worth a seventh guess. Implemented
  `InterruptAfter` directly instead — small (10 lines), the exact
  `time.AfterFunc`/`vm.Interrupt`/`(*time.Timer).Stop` pattern already
  proven working in `pkg/expr` and `pkg/jspiece`, and every property test
  originally written for the abandoned contract (fires after the
  duration, `stop()` prevents it, `stop()` is idempotent, returns
  immediately) still exists in `pkg/sandbox/timeout_test.go` and passes
  against the real implementation. Worth recording plainly: the
  delegation tooling is real and was used in good faith, not skipped —
  this is what happened when it was actually tried.
- **`pkg/catalog` grew trigger and dropdown persistence, an atomic
  `FileStore` save, and a combined Go+JS discovery view in one tanda —
  all implemented by a delegated model (glm-5.2) under human/other-agent
  verification, not hand-written here.** Four concrete additions, each
  with its own test rather than assumed: `catalog.TriggerDefinition` and
  `catalog.DropdownDefinition` are the symmetric persisted forms for
  JS-authored triggers and dropdowns (the gap the jspiece package comment
  above used to call out as "not yet" — it now is), proven to survive a
  process restart and fire/resolve through a real `Engine` by
  `TestTrigger_PersistedAcrossProcessesAndFiresThroughRealEngine` and
  `TestDropdown_PersistedAcrossProcessesAndResolvesThroughRealEngine`
  (`pkg/catalog/trigger_test.go`, `dropdown_test.go`); `FileStore.Save`
  is now atomic — encode into a temp file in the store directory, then
  `os.Rename` into place, so a crash mid-save leaves no half-written
  piece file — covered by `TestFileStore_SaveLeavesNoTempFiles` and
  `TestFileStore_SaveOverwritesWithoutOrphans` (`file_store_test.go`);
  and `catalog.DescribeCombined(store, goCatalog)` renders the Go-native
  catalog and the persisted JS catalog as one plain-text block, so an
  agent's context sees both without a second call, covered by
  `TestDescribeCombined_BothPopulated` and
  `TestDescribeCombined_StableAcrossCalls` (`merged_describe.go`,
  `merged_describe_test.go`). Stated plainly on authorship: the code was
  written by glm-5.2 working against a signed task contract through this
  project's CCDD delegation tooling, then verified here — `go build`,
  `go vet`, and the full `go test ./...` suite were run after the fact to
  confirm nothing regressed, not assumed from the model's own report.
- **The `email` piece needed one decision before it could even be
  delegated: how to test an SMTP send without a real mail server, since
  the stdlib has no `httptest.Server` equivalent for SMTP.** `TICKETS.md`
  had deferred the piece for exactly this reason. Decided to keep the same
  "test against a real protocol server, never a mocked interface" stance
  as `http`'s `httptest.Server` tests: `pkg/pieces/email/email_test.go`
  hand-rolls a minimal SMTP server (`net`+`bufio` only) speaking just
  enough of RFC 5321 — `EHLO`/`AUTH PLAIN`/`MAIL FROM`/`RCPT TO`/`DATA`/
  `QUIT` — for `net/smtp.SendMail` (the real client, stdlib, already
  implements the full protocol) to complete a send against it. One real
  `net/smtp` gotcha worth recording: `smtp.PlainAuth` refuses to send
  credentials over a non-TLS connection unless the server is
  `127.0.0.1`/`localhost` — since the fake test server always binds there,
  this resolves itself, but would otherwise look like a mysterious auth
  failure. Implemented by glm-5.2 under delegation, verified here before
  commit.
- **`pkg/httpapi`, `pkg/credentials`, `pkg/flowstore`, and `pkg/mcpapi`
  were built and verified in the same session, each a separate delegated
  task, each independently checked before the next one started** (`go
  build`/`go vet`/`go test -race`/a `GOOS=linux GOARCH=amd64` cross build/
  direct code reading — never trusting the delegate's own "done" report)
  **before being committed and deployed.** Deployment itself is real, not
  simulated: the cross-compiled binary runs as a systemd service
  (`goflow.service`) on a VPS, bound to `127.0.0.1` only. Every layer was
  exercised against the live deployment, not just `go test`: a JS piece
  registered and immediately used in a flow over `POST /pieces` +
  `POST /flows/run`; a credential saved, listed, and deleted over
  `POST`/`GET`/`DELETE /credentials`, with the raw `.enc` file read
  directly off disk to confirm the plaintext secret never appears in it; a
  flow saved by name and run by name over `POST /flows` +
  `POST /flows/{name}/run`; and the MCP layer's
  `initialize`/`tools/list`/`tools/call` sequence run through the real
  `@modelcontextprotocol/inspector` CLI (installed via `npx` on the VPS),
  not hand-built JSON-RPC — including confirming what a real client does
  on a 401 that hand-built requests couldn't have shown: it attempts OAuth
  discovery rather than just failing, the concrete shape of the auth
  limitation noted above.
