# Catalog expansion — tickets for parallel implementation

Eight independent tickets, one new catalog piece each. No engine changes in
any of them — every gap here is closable entirely inside `pkg/pieces/<name>`.
Designed to be handed out one-per-implementer with zero cross-ticket
dependencies (Tier 2 tickets don't depend on Tier 1, and none of the Tier 1
tickets depend on each other).

## Shared instructions (read once, applies to every ticket below)

1. **Load `.claude/skills/goflow-piece-authoring/SKILL.md` first.** It has
   the full `Action`/`Trigger`/`ActionContext`/`TriggerContext` contract and
   every known pitfall (auth-via-`Input["auth"]`, templating pass-through,
   retry/rate-limiting being the piece's own job, pause rejected in
   standalone action runs, `Store`/`FileWriter` scoping). Don't reinvent
   what it already documents.
2. **Follow the existing package layout exactly**, using
   `pkg/pieces/crypto/crypto.go` + `crypto_test.go` as the template:
   - `const PieceName = "<name>"`
   - `func New() piece.Piece { ...; piece.MustValidate(p); return p }`
   - A package doc comment explaining the piece's scope and any deliberate
     simplification (see existing pieces for tone/style).
   - Every `Run` returns `(nil, err)` for ordinary failures — never panics,
     never uses `regexp.MustCompile`/similar panicking constructors on
     user-supplied input.
   - Every "missing required input" error follows the existing convention:
     `fmt.Errorf("missing required input: <name> (<type>)")`.
3. **Register it**: add the import + one line to `All()` in
   `pkg/pieces/pieces.go`; add its actions/triggers to the
   `TestRegisterAll_EveryCatalogPieceBecomesResolvable` cases list and the
   `TestAll_ReturnsOneEntryPerCatalogPiece` name list in
   `pkg/pieces/pieces_test.go`.
4. **Verify before calling it done**:
   ```
   gofmt -l .
   go vet ./...
   go build ./...
   go test -race ./...
   ```
   All four must be clean. Run the new package's tests with `-race` at
   least once explicitly if the piece has any shared/mutable state (none of
   the nine below should, but double-check).
5. **Write real unit tests**, not just happy-path — every ticket below
   lists the specific cases to cover. Follow the existing test style (see
   any `pkg/pieces/*/*_test.go`): table-driven where there are >3 similar
   cases, one function per behavior otherwise, error messages in `t.Fatalf`
   explain *why* the expected behavior is expected, not just what failed.
6. **Only touch `pkg/pieces/<name>/`, `pkg/pieces/pieces.go`, and
   `pkg/pieces/pieces_test.go`.** Nothing in `pkg/engine`, `pkg/model`,
   `pkg/piece`, or `pkg/expr` should need to change for any of these nine
   pieces — if you find yourself needing an engine change, stop and flag it
   instead of making it.
7. If you discover a genuinely surprising/non-obvious behavior while
   building (not just "I wrote a feature"), add one bullet to `README.md`'s
   "Design decisions" section describing it, following the existing
   entries' tone — honest findings, not marketing copy. Most of these nine
   are simple enough that there may be nothing to report; that's fine, don't
   invent a finding.

---

## Tier 1 — fill real, currently-untouched parts of the piece contract

No catalog piece today uses `ctx.Files`, `ctx.Run.WaitForWaitpoint`, or
`ctx.Run.Stop`/`Respond`. These three tickets are the first real exercise of
those paths through an actual reusable piece instead of a hand-rolled test
fixture.

### Ticket 1 — `storage` (exercises `ctx.Files`)

**Package**: `pkg/pieces/storage` · **PieceName**: `"storage"`

One action: **`write`**.

| | |
|---|---|
| Input | `fileName` (string, required) — passed straight to `ctx.Files.Write`.<br>`content` (string, required) — the data to write.<br>`format` (string, required, **Dropdown**) — `"text"` or `"base64"`. Selects how `content` is interpreted: `"text"` writes `content` as UTF-8 bytes verbatim; `"base64"` decodes it first (`encoding/base64.StdEncoding`). |
| Output | `map[string]any{"fileURL": string}` — whatever `ctx.Files.Write` returned. |
| Errors | missing `fileName` / `content`; `format` not one of the two valid values; `format == "base64"` and `content` isn't valid base64 (propagate the decode error). |

**Dropdown**: this is the catalog's first use of `Action.Dropdowns`. Add:
```go
Dropdowns: map[string]piece.DropdownProperty{
    "format": {
        LoadOptions: func(propsValue map[string]any, ctx piece.PropertyContext) (piece.DropdownState, error) {
            return piece.DropdownState{Options: []piece.DropdownOption{
                {Label: "Text", Value: "text"},
                {Label: "Base64", Value: "base64"},
            }}, nil
        },
    },
},
```

**Tests**:
- `write` with `format:"text"` returns a `"memfile://"`-prefixed URL; verify
  the actual bytes stored match `content` exactly (cast `ctx.Files` to
  `*piece.MemoryFileWriter` in the test and call `.Get(url)` — see
  `TestMultiTenancy_SeparateEnginesFullyIsolated` in
  `pkg/engine/engine_test.go` for the pattern).
- `write` with `format:"base64"` decodes correctly — verify actual decoded
  bytes via the same `.Get(url)` mechanism.
- Invalid base64 with `format:"base64"` fails clearly.
- Unknown `format` value (e.g. `"yaml"`) fails clearly.
- Missing `fileName` and missing `content` each fail clearly (separate
  tests).
- The `format` dropdown's `LoadOptions` returns exactly the two expected
  options (call it directly, not through the engine).
- One integration test in `pkg/pieces/integration_test.go`: a real
  `ExecuteBegin` flow with a `storage.write` step, asserting the returned
  `fileURL` and — using the engine's `*piece.MemoryFileWriter` (accessible
  as `engine.New(registry).Files.(*piece.MemoryFileWriter)`) — the actual
  stored bytes.

---

### Ticket 2 — `approval` (exercises `ctx.Run.WaitForWaitpoint`)

**Package**: `pkg/pieces/approval` · **PieceName**: `"approval"`

One action: **`request`**.

Behavior branches on `ctx.ExecutionType`:

```go
Run: func(ctx piece.ActionContext) (any, error) {
    if ctx.ExecutionType == model.ExecutionResume {
        resume, ok := ctx.ResumePayload.(map[string]any)
        if !ok {
            return nil, fmt.Errorf("expected resume payload to be a map with an \"approved\" bool")
        }
        approved, _ := resume["approved"].(bool)
        comment, _ := resume["comment"].(string)
        return map[string]any{"approved": approved, "comment": comment}, nil
    }
    message, _ := ctx.Input["message"].(string)
    ctx.Run.WaitForWaitpoint("approval")
    return map[string]any{"status": "pending", "message": message}, nil
}
```

(This is a spec, not code to copy verbatim — write it idiomatically, but
keep this exact behavior: BEGIN always pauses, RESUME reads `approved`/
`comment` out of `ResumePayload`.)

| | |
|---|---|
| Input (BEGIN) | `message` (string, optional) — echoed back in the pending output, for the caller's own bookkeeping (e.g. what's shown to the human approving it). |
| Output (BEGIN) | `map[string]any{"status": "pending", "message": message}` — the step ends up `PAUSED`, not `SUCCEEDED` (that's the engine's doing once `WaitForWaitpoint` fires — nothing your `Run` code needs to do beyond calling the hook). |
| Output (RESUME) | `map[string]any{"approved": bool, "comment": string}` |
| Errors | RESUME with a `ResumePayload` that isn't a `map[string]any` fails clearly. |

**Depends on `goflow-piece-authoring`'s pause pitfall**: standalone action
runs (`Engine.ExecuteActionRun`) reject `WaitForWaitpoint` outright — that's
already engine-enforced, no special-casing needed in this piece, but write a
test proving it end-to-end through this real piece (not just trusting the
engine test suite covers it).

**Tests**:
- BEGIN via `ExecuteBegin`: step ends `PAUSED`, output has
  `status:"pending"` and the given `message`.
- RESUME via `ExecuteResume` with `ResumePayload: map[string]any{"approved": true, "comment": "looks good"}`:
  step ends `SUCCEEDED`, output matches.
- RESUME with `approved: false`.
- RESUME with a non-map `ResumePayload` (e.g. a bare string) fails clearly.
- `ExecuteActionRun` (standalone) with this action fails clearly — proves
  the "no flow to resume in" rejection through a real catalog piece.
- One integration test in `pkg/pieces/integration_test.go`: full flow —
  BEGIN pauses on `approval.request`, `ExecuteResume` with an approval,
  chained `NextAction` runs afterward and can read
  `{{ approve.output.approved }}`.

---

### Ticket 3 — `webhook_reply` (exercises `ctx.Run.Stop` / `Respond`)

**Package**: `pkg/pieces/webhookreply` · **PieceName**: `"webhook_reply"`

Two actions: **`respond`** and **`stop`**, sharing the same input/output
shape and a private helper that builds a `*model.WebhookResponse`.

| | |
|---|---|
| Input | `status` (number, optional, default `200`) — reuse the existing `toMilliseconds`-style numeric-coercion helper pattern from `pkg/pieces/http` or `pkg/pieces/delay` (accept `int64`/`int`/`float64`).<br>`body` (any, optional) — passed through untouched as `WebhookResponse.Body`.<br>`headers` (`map[string]any`, optional) — non-string values are skipped, same convention as `pkg/pieces/http`'s header handling. |
| Output | `map[string]any{"status": int}` — just enough for a chained step to know what was sent. |
| Behavior | `respond` calls `ctx.Run.Respond(&model.WebhookResponse{...})` — the run keeps going afterward (`NextAction` still executes). `stop` calls `ctx.Run.Stop(&model.WebhookResponse{...})` — the run ends right there with `SUCCEEDED`; `NextAction` never runs. |

**Tests**:
- `respond`: `state.RespondedEarly` matches the built `WebhookResponse`
  exactly (status/body/headers); a chained `NextAction` after the
  `respond` step DID run (check `state.Steps` has the next step's output).
- `stop`: `state.Verdict.Status == model.FlowRunSucceeded` and
  `state.Verdict.StopResponse` matches; a chained `NextAction` after the
  `stop` step did NOT run (assert `state.Steps["next"]` is absent/nil).
- `status` omitted defaults to `200` for both actions.
- Non-string header values are dropped, not included in the built
  `WebhookResponse.Headers`.
- One integration test in `pkg/pieces/integration_test.go` per action,
  through a real `ExecuteBegin` flow.

---

## Tier 2 — pure-logic utilities (no I/O, no external deps, stdlib only)

These five have zero contract-gap motivation — they're just common,
self-contained automation building blocks. Lowest-risk tickets, good for
filling out the catalog in parallel with Tier 1.

### Ticket 4 — `text`

**Package**: `pkg/pieces/text` · **PieceName**: `"text"`

| Action | Input | Output | Notes |
|---|---|---|---|
| `split` | `text` (string, req), `separator` (string, req) | `{"parts": []string}` | `strings.Split`. Empty `separator` splits into individual runes (Go's own `strings.Split` behavior) — document it, don't special-case it. |
| `join` | `parts` (`[]any`, req — elements coerced via `fmt.Sprint` if not already strings), `separator` (string, req) | `{"text": string}` | |
| `replace` | `text` (string, req), `old` (string, req), `new` (string, req), `all` (bool, optional, default `true`) | `{"text": string}` | `all:false` → `strings.Replace(text, old, new, 1)` (first occurrence only). |
| `trim` | `text` (string, req), `cutset` (string, optional) | `{"text": string}` | `cutset` omitted/empty → `strings.TrimSpace`; otherwise `strings.Trim(text, cutset)`. |
| `case` | `text` (string, req), `mode` (string, req, **Dropdown**: `"upper"` / `"lower"` / `"title"`) | `{"text": string}` | `"title"`: capitalize the first rune of each whitespace-separated word — implement by hand with `unicode`/`strings.Fields`, do **not** use the deprecated `strings.Title`. |

**Tests**: one success case per action, plus: empty-separator `split`,
`replace` with `all:false` vs default, `trim` with an explicit `cutset` vs
default whitespace-only, all three `case` modes, `join` with a mix of
string and non-string elements (e.g. an `int64` in the slice), and a
missing-required-input test per action (can be table-driven).

---

### Ticket 5 — `datetime`

**Package**: `pkg/pieces/datetime` · **PieceName**: `"datetime"`

All timestamps in/out are RFC3339 strings (`time.RFC3339`) unless noted.

| Action | Input | Output |
|---|---|---|
| `now` | — | `{"iso": string}` (`time.Now().UTC().Format(time.RFC3339)`) |
| `parse` | `text` (string, req), `layout` (string, req — a Go time layout, e.g. `"2006-01-02"`) | `{"iso": string, "unixMs": int64}` |
| `format` | `iso` (string, req), `layout` (string, req) | `{"text": string}` |
| `add` | `iso` (string, req), `amountMs` (number, req — accept `int64`/`int`/`float64`, same coercion pattern as `pkg/pieces/delay`) | `{"iso": string}` |
| `diff` | `a` (string, req), `b` (string, req) | `{"diffMs": int64}` (`a - b` in milliseconds; negative if `a` is before `b`) |

**Errors**: invalid RFC3339 in `iso`/`a`/`b`, or a `text`/`layout` pair that
`time.Parse` rejects — propagate the stdlib error, don't swallow it.

**Tests**: `now` returns a timestamp within a generous tolerance window
(e.g. ±5s) of `time.Now()`, and parses back as valid RFC3339; `parse`/
`format` round-trip through a custom layout; `add` with a positive and a
negative `amountMs`; `diff` sign correctness both directions (`a` after
`b` → positive, `a` before `b` → negative) and the zero case; invalid `iso`
and invalid `text`/`layout` combination each fail clearly.

---

### Ticket 6 — `hash`

**Package**: `pkg/pieces/hash` · **PieceName**: `"hash"`

| Action | Input | Output |
|---|---|---|
| `digest` | `text` (string, req), `algorithm` (string, req, **Dropdown**: `"md5"` / `"sha1"` / `"sha256"` / `"sha512"`) | `{"hex": string}` |
| `hmac` | `text` (string, req), `algorithm` (string, req, **Dropdown**: `"sha1"` / `"sha256"` / `"sha512"`), key via `ctx.Auth` (`[]byte`, required — same auth-as-secret convention as `pkg/pieces/crypto`) | `{"hex": string}` |

Use `crypto/md5`, `crypto/sha1`, `crypto/sha256`, `crypto/sha512`,
`crypto/hmac`, `encoding/hex`. `hmac`'s implementation needs
`hmac.New(func() hash.Hash {...}, key)` — importing stdlib `hash` inside a
package that's itself named `hash` is fine in Go (no self-reference
collision); don't work around it, just import it normally.

**Real use case worth noting in the piece's doc comment**: `hmac` is what a
flow would use to verify an incoming webhook's signature (e.g. GitHub's
`X-Hub-Signature-256` header) — ties directly to the `webhook` catalog
piece, verifying payloads it received.

**Errors**: unsupported `algorithm` value (list the supported set in the
error message) for both actions; missing/empty `ctx.Auth` key for `hmac`.

**Tests**: `digest` for all four algorithms against **known, published test
vectors** (e.g. `sha256("")` = `e3b0c442...b855`, a standard MD5/SHA1 test
string) — not just internal round-trip consistency, actual known-correct
output. `hmac-sha256` against a published RFC 4231 test vector. Unsupported
`algorithm` for both actions. Missing `ctx.Auth` for `hmac`.

---

### Ticket 7 — `regex`

**Package**: `pkg/pieces/regex` · **PieceName**: `"regex"`

Use `regexp.Compile` (never `MustCompile` — a bad pattern from flow input
must return an error, not panic).

| Action | Input | Output |
|---|---|---|
| `match` | `text` (string, req), `pattern` (string, req) | `{"matched": bool, "match": string}` — `match` is `""` when `matched` is `false`. |
| `find_all` | `text` (string, req), `pattern` (string, req) | `{"matches": []string}` — empty slice, not an error, when nothing matches. |
| `replace` | `text` (string, req), `pattern` (string, req), `replacement` (string, req) | `{"text": string}` — `regexp.ReplaceAllString`; Go's `$1`-style backreferences in `replacement` work as-is, no special handling needed. |
| `extract_groups` | `text` (string, req), `pattern` (string, req) | `{"fullMatch": string, "groups": []string}` — first match only (`FindStringSubmatch`); `groups` excludes index 0 (the full match itself). Empty/no-match → `fullMatch:""`, `groups: []string{}`. |

**Design decision to bake in**: "no match" is a normal, valid outcome for
`match`/`find_all`/`extract_groups` — never an error. Only an invalid regex
`pattern` (fails to compile) is an error. This mirrors `http`'s
`failOnErrorStatus` philosophy: let the flow branch on the result itself
(e.g. via a ROUTER checking `matched`) rather than force a failure.

**Tests**: `match` found and not-found; `find_all` with multiple matches
and zero matches; `replace` using a `$1` backreference; `extract_groups`
with groups present and with no match at all; an invalid pattern (e.g.
unbalanced `(`) fails clearly for every one of the four actions (table-driven
across actions is fine here).

---

### Ticket 8 — `csv`

**Package**: `pkg/pieces/csv` · **PieceName**: `"csv"`

Use `encoding/csv`.

| Action | Input | Output |
|---|---|---|
| `parse` | `text` (string, req), `delimiter` (string, optional, default `","` — must be exactly one rune if provided, error otherwise), `hasHeader` (bool, optional, default `false`) | `hasHeader:false` → `{"rows": [][]string}`. `hasHeader:true` → `{"rows": []map[string]any}`, keyed by the first row's values. |
| `stringify` | `delimiter` (string, optional, default `","`), and either: `rows` (`[]any` of `[]any` — plain rows, no header) **or** `headers` ([]string, req when using header mode) + `rows` (`[]map[string]any`) | `{"text": string}` |

**Important correctness pitfall to flag explicitly in the piece's doc
comment**: Go map iteration order is randomized. `stringify`'s header mode
MUST use the explicit `headers` input to determine column order — never
derive column order from a `map[string]any`'s own key iteration, or output
column order will be nondeterministic between calls.

**Edge cases**: `text == ""` for `parse` → `{"rows": []}` (or the
appropriately-typed empty slice), not an error, regardless of `hasHeader`.
`delimiter` longer than one rune → clear error. Malformed CSV (e.g. an
unterminated quoted field) → propagate `encoding/csv`'s parse error, don't
swallow it.

**Tests**: `parse` without header; `parse` with header (verify correct
keys/values, not just row count); custom delimiter (e.g. `;` or a literal
tab) for both `parse` and `stringify`; malformed CSV fails clearly;
`stringify` without header round-trips through `parse`; `stringify` with
explicit `headers` round-trips through `parse` with header, **run at least
twice in the same test to catch any accidental reliance on map iteration
order**; delimiter-length validation error; empty-text edge case for
`parse`.

---

## Not included (deliberately, for now)

- ~~**`email` (SMTP send)** — considered and dropped from this batch.~~
  Done (`pkg/pieces/email`): sends via `net/smtp.SendMail`, `smtp.PlainAuth`
  when `ctx.Auth` is a `"user:password"` string, unauthenticated otherwise.
  The blocker noted here — no stdlib equivalent to `httptest.Server` for
  SMTP — was resolved with a hand-rolled, stdlib-only fake SMTP server in
  `email_test.go`, keeping the same "real I/O against a real (if fake)
  protocol server" stance as every other catalog piece.
- ~~**A `Store`-using action piece** — not possible today: `ActionContext`
  has no `Store` field at all (only `TriggerContext` does).~~ Done: this WAS
  an engine question, as this note said — `ActionContext.Store` now exists
  (`pkg/piece/piece.go`), and `*Engine` wires it (`Engine.Store`, defaulting
  to a fresh `MemoryStore` in `New()`) into every PIECE action through both
  `ExecuteBegin` and `ExecuteActionRun`. Unlike `TriggerContext.Store`
  (still nil unless a caller supplies one directly — the engine never
  builds a `TriggerContext` with one), an action's `ctx.Store` is always
  non-nil when run through `*Engine`, since an action's cross-run memory
  doesn't depend on an external polling scheduler the way a trigger's does.
  Not scoped per flow automatically — one `Engine.Store` is shared by every
  flow that Engine runs, same convention as `Engine.Files`; wrap it in a
  `*piece.ScopedStore` or use one `*Engine` per tenant for isolation. See
  `TestExecutePiece_ActionContextGetsEngineStore`,
  `TestExecuteActionRun_ActionContextGetsEngineStore`, and
  `TestEngineStore_SharedAcrossFlowsUnlessScoped` in `pkg/engine/engine_test.go`.
