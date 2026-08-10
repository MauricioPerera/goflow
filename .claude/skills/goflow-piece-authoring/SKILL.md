---
name: goflow-piece-authoring
description: Write or modify a goflow Piece (Action/Trigger) correctly — the contract, the validator to run before registering, and the real pitfalls found while building this project's own test suite. Use before writing any piece.Piece{...} literal in the goflow repo, especially when generating one on the fly (no human review) rather than hand-writing it.
---

# Authoring a goflow Piece

goflow's piece SDK (`pkg/piece`) is a from-scratch Go interface, **not**
compatible with activepieces' TypeScript pieces. A `Piece` is a plain Go
struct with `Actions map[string]Action` and `Triggers map[string]Trigger`,
each backed by a `Run` closure. There is no compiler-enforced schema beyond
Go's own type system — the mistakes that matter here are structural
(nil funcs, name/key drift) and behavioral (auth handling, retry semantics),
not type errors the Go compiler already catches for you.

## Before you write anything: run the validator, always

```go
if err := registry.RegisterValidated(myPiece); err != nil {
    // handle — never register a piece that failed validation
}
```

`piece.RegisterValidated` (in `pkg/piece/validate.go`) runs `piece.Validate`
and only registers if it passes. Use this instead of the plain `Registry.Register`
for any piece you're generating rather than hand-writing — it never panics,
it returns an error you can act on. `Validate` catches:

- `Piece.Name` / `Piece.DisplayName` empty
- An `Action`/`Trigger`'s `Name` field not matching the map key it's
  registered under (confusing, not fatal today — `GetAction`/`GetTrigger`
  resolve by map key, never by the `Name` field — but exactly the kind of
  drift an agent writing a struct literal from scratch introduces first)
- `Run` (on an `Action` or `Trigger`) or `LoadOptions` (on a
  `DropdownProperty`) left `nil` — each of these **panics** the instant the
  engine calls it, not a graceful error

`Validate` is structural only. It cannot tell you whether your `Run`
function is actually *correct* — that's the rest of this document, and
ultimately what tests are for.

## The contract

```go
type Action struct {
    Name        string
    DisplayName string
    Run         func(ctx ActionContext) (any, error)
    Dropdowns   map[string]DropdownProperty // optional
}

type ActionContext struct {
    ExecutionType model.ExecutionType // BEGIN or RESUME
    Input         map[string]any      // already {{ }}-resolved
    Auth          any                 // Input["auth"], if present — see below
    ResumePayload any                 // set only when ExecutionType == RESUME
    Files         FileWriter          // ctx.Files.Write(name, data) -> (url, error)
    Run           RunHooks            // WaitForWaitpoint / Stop / Respond
}
```

```go
type Trigger struct {
    Name        string
    DisplayName string
    Run         func(ctx TriggerContext) ([]any, error)
}

type TriggerContext struct {
    Payload any
    Input   map[string]any
    Store   Store // nil unless the caller supplied one — see below
}
```

Return `(nil, err)` for a failure — never panic from inside `Run` for an
ordinary error condition (missing input, a downstream API failure, bad
auth). A real panic (nil pointer, index out of range) is NOT recovered by
the engine and will crash the calling goroutine.

## Pitfalls found while building this project (read before writing `Run`)

- **Auth is not a separate parameter — it's `Input["auth"]`.** Use the
  reserved key `piece.AuthInputKey` when building the input map, and read it
  back via `ctx.Auth` (a convenience alias for `ctx.Input[piece.AuthInputKey]`).
  The engine never requires it to be present or non-empty — if your action
  needs auth, check `ctx.Auth` yourself and return a clear error if it's
  missing or the wrong type. `piece.OAuth2Auth` is an optional typed
  convenience for the OAuth2 shape (`AccessToken`/`Data`/`Props`); still just
  a value behind `ctx.Auth`, still your job to type-assert and validate.

- **Non-string/map/slice values pass through `{{ }}` templating untouched.**
  `*piece.ApFile`, `*piece.OAuth2Auth`, `[]byte` (e.g. an encryption key) all
  work as `Input` values with zero special handling — `expr.Resolve` only
  ever recurses into strings/maps/slices; everything else is returned as-is.
  You can lean on this for any custom Go type you need to pass through.

- **A `LOOP_ON_ITEMS`'s `Items` field is evaluated as a raw expression, not
  string-templated** — it must resolve to an actual array, so `"{{ [1,2,3] }}"`
  works but `"count: {{ [1,2,3] }}"` (mixed text) does not. This only
  matters if your piece is used as a loop body or you're building
  `model.FlowAction` trees around it, not for the piece's own `Run` code.

- **Retry (`RetryOnFailure`) and `ContinueOnFailure` apply to PIECE/CODE
  actions ONLY — never to triggers.** `ExecuteBegin` calls a trigger's
  `Run` exactly once; if it fails, the run fails immediately, full stop. If
  your trigger talks to a flaky upstream, retry internally inside `Run`
  (an ordinary loop + backoff — nothing engine-specific needed). Same story
  for rate limiting: if your action/trigger needs to self-throttle, do it
  with ordinary Go state inside `Run` and lean on `RetryOnFailure` (actions
  only) to survive being throttled — the engine has no rate-limiting concept
  to hook into.

- **Pausing (`ctx.Run.WaitForWaitpoint`) is rejected outright in a
  standalone action run** (`Engine.ExecuteActionRun`, `ActionRunMode` on the
  execution state) — there's no flow to persist against, so the step fails
  with a clear error instead of hanging as PAUSED forever. Don't call
  `WaitForWaitpoint` from an action meant to also work standalone (e.g. a
  "test this action" code path) without checking whether that's even a
  flow context first — nothing in `ActionContext` currently exposes whether
  you're in `ActionRunMode`, so if this matters to your piece, have the
  *caller* avoid invoking that code path outside a real flow.

- **`Store`/`FileWriter` need explicit per-flow (or per-tenant) scoping if
  shared.** A trigger's `Store` is just a `map[string]any` under the hood
  (`piece.MemoryStore`) with no automatic namespacing — if two different
  flows (or two different tenants) end up sharing one `Store` instance for
  the same trigger and the same literal key (e.g. `"last_id"`, which most
  polling triggers naturally use), their cursors WILL clash. Wrap a shared
  backend with `&piece.ScopedStore{Underlying: shared, FlowID: someID}` per
  flow — folding a project ID into that same `FlowID` string
  (`"project-A/flow-1"`) gets you tenant isolation too, for free. Similarly,
  a `piece.MemoryFileWriter`'s returned URL (`memfile://f1/...`) is only
  unique **within that one writer instance** — its ID counter starts fresh
  per instance, so two separate writers can coincidentally mint the same
  URL string for two unrelated files. Never treat a `FileWriter` URL as
  globally unique across engines/tenants; use one `*engine.Engine` (and
  therefore one `Files`) per tenant if isolation actually matters.

- **CODE steps (not pieces, but adjacent if your piece's flow chains into
  one) are synchronous-only** — a `Source` function that returns a Promise
  is rejected with a clear error, not silently awaited. Not directly
  relevant to writing a `piece.Action`, but relevant if you're also
  generating the surrounding `model.FlowAction` tree.

## Minimal worked example

```go
registry := piece.NewRegistry()

myPiece := piece.Piece{
    Name: "weather", DisplayName: "Weather",
    Actions: map[string]piece.Action{
        "get_forecast": {
            Name: "get_forecast", DisplayName: "Get Forecast",
            Run: func(ctx piece.ActionContext) (any, error) {
                apiKey, ok := ctx.Auth.(string)
                if !ok || apiKey == "" {
                    return nil, fmt.Errorf("missing required auth: apiKey")
                }
                city, _ := ctx.Input["city"].(string)
                if city == "" {
                    return nil, fmt.Errorf("missing required input: city (string)")
                }
                // ... call the real API here ...
                return map[string]any{"city": city, "tempC": 21}, nil
            },
        },
    },
}

if err := registry.RegisterValidated(myPiece); err != nil {
    return fmt.Errorf("registering weather piece: %w", err)
}
```

## After writing it: verify, don't just assume

```bash
go build ./...
go vet ./...            # catches format-string arg mismatches, among other things
go test -race ./...     # -race matters the moment your piece might run under ExecuteBegin from multiple goroutines
```

If your piece has any internal shared mutable state (a cursor, a rate-limit
counter, anything captured by a closure across multiple `Run` calls),
protect it with a `sync.Mutex`/`atomic` — the engine itself may call the
same piece's actions from concurrent goroutines (see
`TestConcurrentFlowRuns_*` in `pkg/engine/engine_test.go` for what that
looks like and why it's a real, not hypothetical, concern for this project).
