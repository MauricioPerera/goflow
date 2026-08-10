# Catalog Triggers — Reporte

Soporte simétrico para **Triggers** en `pkg/catalog`, replicando exactamente el
patrón ya usado para **Actions**. No se tocaron `pkg/jspiece`, `pkg/piece`,
`pkg/engine`, `pkg/pieces` ni `README.md`. Todos los cambios viven en
`pkg/catalog/` (edits a `catalog.go` y `validate.go` + nuevo `trigger_test.go`).

## Qué agregué

### 1. `pkg/catalog/catalog.go`

- **`TriggerDefinition`** — tipo paralelo a `ActionDefinition` pero **sin
  `InputSchema`** (un trigger no tiene Input schema en el mismo sentido; misma
  justificación que ya documenta `ActionDefinition.InputSchema`: no hay shape
  engine-enforced del que derivar un schema real). Campos: `Name`,
  `DisplayName`, `Description`, `Source string`, `Examples []TriggerExample`.
- **Campo `Triggers []TriggerDefinition`** agregado a `Definition`.
- **`ToPiece()`** actualizado: además de armar `Actions` con `jspiece.New`,
  ahora arma `piece.Piece.Triggers` con
  `jspiece.NewTrigger(jspiece.TriggerSource{Name, DisplayName, Source})` por
  cada trigger — mismo patrón que ya usaba con Actions.

### 2. `pkg/catalog/validate.go`

- **`TriggerExample`** — tipo paralelo a `Example` para triggers: `Description`,
  `Payload any`, `Input map[string]any`, `WantError bool`, `CheckOutput bool`,
  `WantItems []any`. La comparación de items usa **`jsonEqual` existente** (no
  reescrito).
- **`Validate(def Definition)`** actualizado: además de exigir ≥1 `Example` por
  acción y correrlos, ahora **también exige ≥1 `TriggerExample` por cada
  trigger** y corre cada uno de verdad contra el trigger real
  (`p.ToPiece().Triggers[nombre].Run(piece.TriggerContext{Payload, Input})` —
  **sin Store**, porque `Engine.ExecuteBegin` tampoco pasa Store a un PIECE
  trigger; confirmado leyendo `engine.go` y consistente con el comentario de
  `TestCatalog_JSTriggerComposesWithRealCatalog`).
- **`runTriggerExample`** — función hermana de `runExample`, mismo patrón
  exacto (`{Path, Message}`, `WantError` primero, luego `CheckOutput` +
  `jsonEqual`).

### 3. `pkg/catalog/trigger_test.go` (nuevo)

8 tests reales (con `Run()` real, sin mocks):

1. `TestValidate_TriggerWithPassingExamplesSucceeds` — Definition con trigger
   válido + Examples que pasan → `Validate()` sin errores.
2. `TestValidate_TriggerWithoutExamplesFails` — trigger sin Examples →
   `Validate()` falla.
3. `TestValidate_TriggerExampleExpectedErrorButSucceededFails` —
   `WantError:true` pero el trigger no tira error → `Validate()` falla.
4. `TestValidate_TriggerExampleOutputMismatchFails` — `CheckOutput:true` con
   `WantItems` que no matchea → `Validate()` falla.
5. `TestDefinition_ToPiece_BuildsInvocableTrigger` — `ToPiece()` arma
   `piece.Piece.Triggers` con el trigger invocable de verdad (corre `Run` y
   verifica los items mapeados).
6. `TestGatedStore_RejectsTriggerWithoutExamples` — `GatedStore` rechaza
   guardar una Definition con un trigger sin Examples (y no delega al
   underlying).
7. `TestGatedStore_AcceptsValidTriggerDefinition` — `GatedStore` acepta una
   Definition con trigger válido y la persiste.
8. `TestTrigger_PersistedAcrossProcessesAndFiresThroughRealEngine` — punta a
   punta: `catalog.NewFileStore(t.TempDir())` envuelto en
   `&catalog.GatedStore{...}` → nueva instancia `FileStore` →
   `catalog.RegisterFromStore` en `*piece.Registry` nuevo →
   `engine.New(registry).ExecuteBegin` con `Trigger.Type=model.TriggerPiece`
   apuntando a esa pieza. Confirma que `ExecuteBegin` real dispara el trigger
   persistido y usa su **primer item** devuelto (igual que
   `TestCatalog_JSTriggerComposesWithRealCatalog`).

## Decisiones de diseño tomadas por mi cuenta

1. **`ToPiece()` inicializa `p.Triggers` como `make(map[string]piece.Trigger)`
   antes de poblarlo.** `jspiece.New` solo construye `Actions` y deja
   `Triggers` como mapa nil; asignar sobre nil panicaría. Lo hago solo cuando
   `len(d.Triggers) > 0` (si no hay triggers, `Triggers` queda nil, idéntico al
   comportamiento previo — ningún test existente depende de eso, y
   `piece.Validate` itera `p.Triggers` sin problemas sobre nil).

2. **`TriggerExample` NO lleva `Auth`** (a diferencia de `Example`, que sí
   tiene `Auth`). Un trigger no recibe auth en `piece.TriggerContext` (solo
   `Payload`, `Input`, `Store`), así que exponerlo no tendría adónde ir. Esto
   es lo que el enunciado pedía explícitamente (`Payload`, `Input`, `WantError`,
   `CheckOutput`, `WantItems`).

3. **`runTriggerExample` corre SIN Store** (Store nil), por la razón que el
   enunciado detalla: `Engine.ExecuteBegin` construye
   `piece.TriggerContext{Payload, Input}` y nunca setea Store para un PIECE
   trigger. Un ejemplo que necesite Store para comportarse correctamente no
   podría validarse acá — exactamente igual a como no puede correr por
   `ExecuteBegin`. Es el mismo límite documentado en
   `TestCatalog_JSTriggerComposesWithRealCatalog`.

4. **Test end-to-end también llama a `pieces.RegisterAll(registry)`** además de
   `catalog.RegisterFromStore`. El step `report` usa el piece `json` del
   catálogo Go real; `RegisterFromStore` solo registra el piece persistido
   (`order_trigger`). Esto combina ambos patrones de referencia: el
   `RegisterAll` de `TestCatalog_JSTriggerComposesWithRealCatalog` (para tener
   `json`) + el `FileStore → nueva instancia → RegisterFromStore → engine` de
   `TestRegisterFromStore_PieceSurvivesAcrossProcessesAndRunsForReal`. No hay
   colisión de nombres (`order_trigger` no existe en el catálogo Go).

5. **No toqué el doc-comment del package** en `catalog.go`. El enunciado pedía
   actualizarlo solo si decía que "Triggers" no está soportado. El comentario
   actual **no menciona** que triggers estén no-soportados (habla de "one worked
   Example per action" pero no afirma exclusión de triggers), así que por la
   instrucción explícita ("puede que ya no mencione esto explícitamente, en
   cuyo caso no toques el comentario") se dejó intacto.

## Verificación — salida REAL y completa

### `gofmt -l .`

```
--- gofmt -l . ---
[exit 0]
```
(salida vacía = ningún archivo necesita formateo)

### `go vet ./...`

```
--- go vet ./... ---
[vet exit 0]
```
(sin hallazgos)

### `go build ./...`

```
--- go build ./... ---
[build exit 0]
```

### `go test ./pkg/catalog/... -race -v`

```
=== RUN   TestRegisterFromStore_PieceSurvivesAcrossProcessesAndRunsForReal
--- PASS: TestRegisterFromStore_PieceSurvivesAcrossProcessesAndRunsForReal (0.02s)
=== RUN   TestRegisterFromStore_MultiplePiecesAlongsideRealCatalog
--- PASS: TestRegisterFromStore_MultiplePiecesAlongsideRealCatalog (0.00s)
=== RUN   TestMemoryStore_ImplementsStoreContract
--- PASS: TestMemoryStore_ImplementsStoreContract (0.00s)
=== RUN   TestFileStore_ImplementsStoreContract
--- PASS: TestFileStore_ImplementsStoreContract (0.04s)
=== RUN   TestFileStore_PersistsAcrossInstances
--- PASS: TestFileStore_PersistsAcrossInstances (0.03s)
=== RUN   TestFileStore_RejectsPathTraversalNames
--- PASS: TestFileStore_RejectsPathTraversalNames (0.00s)
=== RUN   TestDefinition_ToPiece
--- PASS: TestDefinition_ToPiece (0.00s)
=== RUN   TestDescribe_EmptyStore
--- PASS: TestDescribe_EmptyStore (0.00s)
=== RUN   TestDescribe_ListsNameDescriptionAndActions
--- PASS: TestDescribe_ListsNameDescriptionAndActions (0.00s)
=== RUN   TestGatedStore_RejectsInvalidDefinition
--- PASS: TestGatedStore_RejectsInvalidDefinition (0.00s)
=== RUN   TestGatedStore_AcceptsValidDefinition
--- PASS: TestGatedStore_AcceptsValidDefinition (0.00s)
=== RUN   TestGatedStore_ThenRegisterFromStoreRunsThroughRealEngine
--- PASS: TestGatedStore_ThenRegisterFromStoreRunsThroughRealEngine (0.02s)
=== RUN   TestValidate_TriggerWithPassingExamplesSucceeds
--- PASS: TestValidate_TriggerWithPassingExamplesSucceeds (0.00s)
=== RUN   TestValidate_TriggerWithoutExamplesFails
--- PASS: TestValidate_TriggerWithoutExamplesFails (0.00s)
=== RUN   TestValidate_TriggerExampleExpectedErrorButSucceededFails
--- PASS: TestValidate_TriggerExampleExpectedErrorButSucceededFails (0.00s)
=== RUN   TestValidate_TriggerExampleOutputMismatchFails
--- PASS: TestValidate_TriggerExampleOutputMismatchFails (0.00s)
=== RUN   TestDefinition_ToPiece_BuildsInvocableTrigger
--- PASS: TestDefinition_ToPiece_BuildsInvocableTrigger (0.00s)
=== RUN   TestGatedStore_RejectsTriggerWithoutExamples
--- PASS: TestGatedStore_RejectsTriggerWithoutExamples (0.00s)
=== RUN   TestGatedStore_AcceptsValidTriggerDefinition
--- PASS: TestGatedStore_AcceptsValidTriggerDefinition (0.00s)
=== RUN   TestTrigger_PersistedAcrossProcessesAndFiresThroughRealEngine
--- PASS: TestTrigger_PersistedAcrossProcessesAndFiresThroughRealEngine (0.03s)
=== RUN   TestValidate_PassingExamplesSucceed
--- PASS: TestValidate_PassingExamplesSucceed (0.00s)
=== RUN   TestValidate_NoExamplesFails
--- PASS: TestValidate_NoExamplesFails (0.00s)
=== RUN   TestValidate_UnexpectedErrorFails
--- PASS: TestValidate_UnexpectedErrorFails (0.00s)
=== RUN   TestValidate_ExpectedErrorButSucceededFails
--- PASS: TestValidate_ExpectedErrorButSucceededFails (0.00s)
=== RUN   TestValidate_OutputMismatchFails
--- PASS: TestValidate_OutputMismatchFails (0.00s)
=== RUN   TestValidate_OutputComparisonToleratesNumericTypeQuirks
--- PASS: TestValidate_OutputComparisonToleratesNumericTypeQuirks (0.00s)
=== RUN   TestValidate_StructuralErrorsAreAlsoCaught
--- PASS: TestValidate_StructuralErrorsAreAlsoCaught (0.00s)
=== RUN   TestValidate_CheckOutputFalseSkipsOutputComparison
--- PASS: TestValidate_CheckOutputFalseSkipsOutputComparison (0.00s)
PASS
ok  	goflow/pkg/catalog	1.355s
```

### `go test ./...` (suite completa)

```
?   	goflow/examples	[no test files]
ok  	goflow/pkg/catalog	2.670s
ok  	goflow/pkg/engine	3.279s
ok  	goflow/pkg/expr	0.506s
ok  	goflow/pkg/flowvalidate	2.649s
ok  	goflow/pkg/jspiece	10.135s
ok  	goflow/pkg/model	0.971s
ok  	goflow/pkg/piece	(cached)
ok  	goflow/pkg/pieces	3.032s
ok  	goflow/pkg/pieces/approval	0.251s
ok  	goflow/pkg/pieces/crypto	(cached)
ok  	goflow/pkg/pieces/csv	(cached)
ok  	goflow/pkg/pieces/datetime	(cached)
ok  	goflow/pkg/pieces/delay	(cached)
ok  	goflow/pkg/pieces/hash	(cached)
ok  	goflow/pkg/pieces/http	(cached)
ok  	goflow/pkg/pieces/json	(cached)
ok  	goflow/pkg/pieces/regex	(cached)
ok  	goflow/pkg/pieces/storage	(cached)
ok  	goflow/pkg/pieces/text	(cached)
ok  	goflow/pkg/pieces/webhook	(cached)
ok  	goflow/pkg/pieces/webhookreply	0.241s
ok  	goflow/pkg/sandbox	0.379s
```

Todo en verde. Nada más se rompió.