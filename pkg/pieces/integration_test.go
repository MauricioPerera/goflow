package pieces_test

// Proves the catalog composes in a real flow, not just in isolated
// per-piece unit tests: a webhook trigger kicks off a run, an HTTP request
// fetches JSON from a real (httptest) server, and the JSON piece parses the
// response body — three different catalog pieces chained through {{ }}
// templating exactly like any hand-authored piece in pkg/engine's own test
// suite.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"goflow/pkg/engine"
	"goflow/pkg/flowvalidate"
	"goflow/pkg/jspiece"
	"goflow/pkg/model"
	"goflow/pkg/piece"
	"goflow/pkg/pieces"
)

func TestCatalog_WebhookThenHTTPThenJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"greeting":"hello","count":2}`))
	}))
	defer server.Close()

	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	parseStep := &model.FlowAction{
		Name: "parsed", DisplayName: "Parse Response", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
			"text": "{{ fetch.output.body }}",
		}},
	}
	fetchStep := &model.FlowAction{
		Name: "fetch", DisplayName: "Fetch", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "request", Input: map[string]any{
			"url": server.URL,
		}},
		NextAction: parseStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-integration", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Catch Webhook", Type: model.TriggerPiece,
		PieceName: "webhook", TriggerName: "catch_hook", Input: map[string]any{},
		NextAction: fetchStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: map[string]any{"source": "test"},
		ExecuteTrigger: true,
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	fetchOut := state.Steps["fetch"].Output.(map[string]any)
	if fetchOut["status"] != 200 {
		t.Fatalf("fetch status = %v, want 200", fetchOut["status"])
	}

	parsedOut := state.Steps["parsed"].Output.(map[string]any)
	data := parsedOut["data"].(map[string]any)
	if data["greeting"] != "hello" || data["count"] != float64(2) {
		t.Fatalf("parsed data = %+v", data)
	}
}

// TestCatalog_HTTPRetriesAgainstFlakyServer is the catalog's own version of
// pkg/engine's TestRetryOnFailureSucceedsOnThirdAttempt — same mechanism
// (RetryOnFailure, backoff, a call counter proving the retry loop actually
// ran), but exercised through the REAL http piece against a REAL (if fake)
// server instead of a hand-rolled test piece. This also only works at all
// because of failOnErrorStatus: the http piece's default behavior (a 500 is
// just data, Run still succeeds) would never trigger a retry — RetryOnFailure
// only ever looks at whether Run returned an error, never at Output.
func TestCatalog_HTTPRetriesAgainstFlakyServer(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("temporarily unavailable"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"greeting":"recovered"}`))
	}))
	defer server.Close()

	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	parseStep := &model.FlowAction{
		Name: "parsed", DisplayName: "Parse Response", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
			"text": "{{ fetch.output.body }}",
		}},
	}
	fetchStep := &model.FlowAction{
		Name: "fetch", DisplayName: "Fetch", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "request", Input: map[string]any{
			"url":               server.URL,
			"failOnErrorStatus": true,
		}},
		Error:      &model.ErrorHandling{RetryOnFailure: true},
		NextAction: parseStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-retry", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: fetchStep,
	}}

	e := engine.New(registry)
	e.Retry = engine.RetryConstants{MaxAttempts: 5, ExponentialBase: 1, IntervalMs: 3}

	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED once the retry loop reaches the server's 3rd (successful) response", state.Verdict)
	}
	if got := atomic.LoadInt32(&requestCount); got != 3 {
		t.Fatalf("requestCount = %d, want exactly 3 (2 failures + 1 success) — proof the retry loop actually re-hit the server, not that it succeeded by luck", got)
	}

	fetchOut := state.Steps["fetch"].Output.(map[string]any)
	if fetchOut["status"] != 200 {
		t.Fatalf("fetch status = %v, want 200 (the final, successful attempt)", fetchOut["status"])
	}

	parsedOut := state.Steps["parsed"].Output.(map[string]any)
	data := parsedOut["data"].(map[string]any)
	if data["greeting"] != "recovered" {
		t.Fatalf("parsed data = %+v", data)
	}
}

// TestCatalog_HTTPRespectsRateLimitInRealFlow proves the catalog handles a
// rate-limited API end to end in a real flow, not via the engine's
// RetryOnFailure (which is what TestCatalog_HTTPRetriesAgainstFlakyServer
// exercises) but via the http piece's own respectRetryAfter: the server
// returns 429 + Retry-After for the first two requests, then succeeds, and
// the fetch step's Run itself waits and re-sends — no engine-level retry
// configured on this step at all.
func TestCatalog_HTTPRespectsRateLimitInRealFlow(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("rate limited"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"greeting":"finally"}`))
	}))
	defer server.Close()

	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	parseStep := &model.FlowAction{
		Name: "parsed", DisplayName: "Parse Response", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
			"text": "{{ fetch.output.body }}",
		}},
	}
	fetchStep := &model.FlowAction{
		Name: "fetch", DisplayName: "Fetch", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "request", Input: map[string]any{
			"url":               server.URL,
			"respectRetryAfter": true,
		}},
		NextAction: parseStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-rate-limit", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: fetchStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED once the piece's own rate-limit retry reaches the 3rd (successful) response", state.Verdict)
	}
	if got := atomic.LoadInt32(&requestCount); got != 3 {
		t.Fatalf("requestCount = %d, want exactly 3 (2 rate-limited + 1 success) — proof the http piece itself retried, no engine-level RetryOnFailure was configured on this step", got)
	}

	fetchOut := state.Steps["fetch"].Output.(map[string]any)
	if fetchOut["status"] != 200 {
		t.Fatalf("fetch status = %v, want 200 (the final, successful attempt)", fetchOut["status"])
	}

	parsedOut := state.Steps["parsed"].Output.(map[string]any)
	data := parsedOut["data"].(map[string]any)
	if data["greeting"] != "finally" {
		t.Fatalf("parsed data = %+v", data)
	}
}

// TestCatalog_HTTPSendsOAuth2AuthInRealFlow proves an OAuth2-authenticated
// call works through the catalog's own http piece in a real flow — not a
// hand-rolled piece like pkg/engine's TestFlow_OAuth2Auth_Succeeds, whose
// "crm" piece checked ctx.Auth itself and never actually built an HTTP
// request. The auth value here flows the exact same way as everywhere else
// in goflow: a *piece.OAuth2Auth placed under piece.AuthInputKey in the
// step's Input, resolved untouched (expr.Resolve's default case), and
// surfaced as ctx.Auth — no OAuth2-specific engine code involved.
func TestCatalog_HTTPSendsOAuth2AuthInRealFlow(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"whoami":"authenticated"}`))
	}))
	defer server.Close()

	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	parseStep := &model.FlowAction{
		Name: "parsed", DisplayName: "Parse Response", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
			"text": "{{ fetch.output.body }}",
		}},
	}
	fetchStep := &model.FlowAction{
		Name: "fetch", DisplayName: "Fetch", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "http", ActionName: "request", Input: map[string]any{
			"url": server.URL,
			piece.AuthInputKey: &piece.OAuth2Auth{
				AccessToken: "oauth-token-xyz",
				Data:        map[string]any{"token_type": "Bearer", "expires_in": 3600},
			},
		}},
		NextAction: parseStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-oauth2", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: fetchStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	if gotAuth != "Bearer oauth-token-xyz" {
		t.Fatalf("server saw Authorization = %q, want %q", gotAuth, "Bearer oauth-token-xyz")
	}

	fetchOut := state.Steps["fetch"].Output.(map[string]any)
	if fetchOut["status"] != 200 {
		t.Fatalf("fetch status = %v, want 200", fetchOut["status"])
	}

	parsedOut := state.Steps["parsed"].Output.(map[string]any)
	data := parsedOut["data"].(map[string]any)
	if data["whoami"] != "authenticated" {
		t.Fatalf("parsed data = %+v", data)
	}
}

// TestCatalog_MultiTenancy_ConcurrentTenantsSharingOneRegistryDontLeak asks a
// different question than pkg/engine's TestMultiTenancy_SeparateEnginesFullyIsolated
// and TestMultiTenancy_ComposedProjectAndFlowScoping: those prove isolation
// via separate engines and ScopedStore. None of the four catalog pieces
// touch Store or FileWriter at all — http/json/delay/webhook are pure
// per-call logic — so there is no per-tenant STATE for the catalog itself to
// isolate the way those tests needed. What's actually worth proving here:
// the realistic deployment shape (ONE process, ONE shared registry/engine,
// many tenants' flows running concurrently — nobody spins up a fresh Engine
// per request) never lets one tenant's OAuth2 token or response leak into
// another's, since the http piece allocates a fresh *http.Request and
// *http.Client per call with no shared mutable state to race on. Run with
// -race; each tenant's server also rejects any request not carrying ITS OWN
// token, so a leaked/crossed auth header is impossible to miss.
func TestCatalog_MultiTenancy_ConcurrentTenantsSharingOneRegistryDontLeak(t *testing.T) {
	const tenantCount = 8

	type tenant struct {
		name   string
		token  string
		server *httptest.Server
	}
	tenants := make([]*tenant, tenantCount)
	for i := range tenants {
		te := &tenant{name: fmt.Sprintf("tenant-%d", i), token: fmt.Sprintf("token-%d", i)}
		te.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+te.token {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"tenant":"WRONG-AUTH-LEAKED"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tenant":%q}`, te.name)
		}))
		tenants[i] = te
	}
	defer func() {
		for _, te := range tenants {
			te.server.Close()
		}
	}()

	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	e := engine.New(registry) // ONE engine/registry shared by every tenant below

	results := make([]*model.ExecutionState, tenantCount)
	var wg sync.WaitGroup
	for i, te := range tenants {
		wg.Add(1)
		go func(i int, te *tenant) {
			defer wg.Done()
			parseStep := &model.FlowAction{
				Name: "parsed", DisplayName: "Parse Response", Type: model.ActionPiece,
				Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
					"text": "{{ fetch.output.body }}",
				}},
			}
			fetchStep := &model.FlowAction{
				Name: "fetch", DisplayName: "Fetch", Type: model.ActionPiece,
				Piece: &model.PieceSettings{PieceName: "http", ActionName: "request", Input: map[string]any{
					"url":              te.server.URL,
					piece.AuthInputKey: &piece.OAuth2Auth{AccessToken: te.token},
				}},
				NextAction: parseStep,
			}
			fv := &model.FlowVersion{ID: fmt.Sprintf("fv-tenant-%d", i), Trigger: &model.FlowTrigger{
				Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
				NextAction: fetchStep,
			}}
			results[i] = e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
		}(i, te)
	}
	wg.Wait()

	for i, te := range tenants {
		state := results[i]
		if state.Verdict.Status != model.FlowRunSucceeded {
			t.Fatalf("tenant %d verdict = %+v", i, state.Verdict)
		}
		parsedOut := state.Steps["parsed"].Output.(map[string]any)
		data := parsedOut["data"].(map[string]any)
		if data["tenant"] != te.name {
			t.Fatalf("tenant %d got %+v, want tenant %q — cross-tenant auth/response leak", i, data, te.name)
		}
	}
}

// TestCatalog_EncryptDecryptRoundTripThroughRealFlow is the catalog's own
// version of pkg/engine's TestFlow_EncryptDecryptRoundTrip — same mechanism
// (AES-GCM, key via ctx.Auth), but through the real crypto piece instead of
// the hand-rolled "vault" test fixture, chained with the real json piece to
// prove the ciphertext composes as an ordinary string value through {{ }}
// templating like anything else.
func TestCatalog_EncryptDecryptRoundTripThroughRealFlow(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	key := []byte("0123456789abcdef") // AES-128

	decryptStep := &model.FlowAction{
		Name: "decrypt", DisplayName: "Decrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "crypto", ActionName: "decrypt", Input: map[string]any{
			piece.AuthInputKey: key,
			"ciphertext":       "{{ envelope.output.data.ciphertext }}",
		}},
	}
	// Round-trips the ciphertext through the json piece too (stringify then
	// parse) — proving it survives as an ordinary opaque string value
	// through the rest of the catalog, not just through direct step output.
	envelopeStep := &model.FlowAction{
		Name: "envelope", DisplayName: "Envelope", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "parse", Input: map[string]any{
			"text": "{{ envelope_text.output.text }}",
		}},
		NextAction: decryptStep,
	}
	envelopeTextStep := &model.FlowAction{
		Name: "envelope_text", DisplayName: "Envelope Text", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "stringify", Input: map[string]any{
			"data": map[string]any{"ciphertext": "{{ encrypt.output.ciphertext }}"},
		}},
		NextAction: envelopeStep,
	}
	encryptStep := &model.FlowAction{
		Name: "encrypt", DisplayName: "Encrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "crypto", ActionName: "encrypt", Input: map[string]any{
			piece.AuthInputKey: key,
			"plaintext":        "the launch codes are 12345",
		}},
		NextAction: envelopeTextStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-crypto", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: encryptStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	ciphertext, _ := state.Steps["encrypt"].Output.(map[string]any)["ciphertext"].(string)
	if ciphertext == "" || ciphertext == "the launch codes are 12345" {
		t.Fatalf("ciphertext = %q, want a real (non-empty, non-plaintext) encrypted value", ciphertext)
	}

	decryptOut := state.Steps["decrypt"].Output.(map[string]any)
	if decryptOut["plaintext"] != "the launch codes are 12345" {
		t.Fatalf("decrypt output = %+v", decryptOut)
	}
}

// TestCatalog_DecryptWithWrongKeyFailsClearly is the catalog's own version
// of pkg/engine's TestFlow_DecryptWithWrongKeyFailsClearly.
func TestCatalog_DecryptWithWrongKeyFailsClearly(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	rightKey := []byte("0123456789abcdef")
	wrongKey := []byte("fedcba9876543210")

	decryptStep := &model.FlowAction{
		Name: "decrypt", DisplayName: "Decrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "crypto", ActionName: "decrypt", Input: map[string]any{
			piece.AuthInputKey: wrongKey, // deliberately not the key encrypt used
			"ciphertext":       "{{ encrypt.output.ciphertext }}",
		}},
	}
	encryptStep := &model.FlowAction{
		Name: "encrypt", DisplayName: "Encrypt", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "crypto", ActionName: "encrypt", Input: map[string]any{
			piece.AuthInputKey: rightKey,
			"plaintext":        "top secret",
		}},
		NextAction: decryptStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-crypto-wrong-key", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: encryptStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — GCM authentication must reject the wrong key", state.Verdict)
	}
}

// TestCatalog_StorageWriteThroughRealFlow proves storage.write works through
// the real catalog (pieces.RegisterAll), not just the piece's own isolated
// unit tests — the first catalog piece to touch ctx.Files at all.
func TestCatalog_StorageWriteThroughRealFlow(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	writeStep := &model.FlowAction{
		Name: "write", DisplayName: "Write", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "storage", ActionName: "write", Input: map[string]any{
			"fileName": "greeting.txt",
			"content":  "hello from the catalog",
			"format":   "text",
		}},
	}
	fv := &model.FlowVersion{ID: "fv-catalog-storage", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: writeStep,
	}}

	e := engine.New(registry)
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	url, _ := state.Steps["write"].Output.(map[string]any)["fileURL"].(string)
	if url == "" {
		t.Fatal("fileURL is empty")
	}
	writer := e.Files.(*piece.MemoryFileWriter)
	data, ok := writer.Get(url)
	if !ok || string(data) != "hello from the catalog" {
		t.Fatalf("stored data = %q, ok=%v", data, ok)
	}
}

// TestCatalog_ApprovalPauseResumeThroughRealFlow proves approval.request
// works through the real catalog: a flow pauses on it, resumes with a
// decision, and a chained step reads the decision via {{ }} templating —
// the first catalog piece to actually pause a flow.
func TestCatalog_ApprovalPauseResumeThroughRealFlow(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	summarizeStep := &model.FlowAction{
		Name: "summarize", DisplayName: "Summarize", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "stringify", Input: map[string]any{
			"data": map[string]any{"wasApproved": "{{ approve.output.approved }}"},
		}},
	}
	approveStep := &model.FlowAction{
		Name: "approve", DisplayName: "Approve", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "approval", ActionName: "request", Input: map[string]any{
			"message": "ship it?",
		}},
		NextAction: summarizeStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-approval", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: approveStep,
	}}

	e := engine.New(registry)
	begun := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if begun.Verdict.Status != model.FlowRunPaused {
		t.Fatalf("verdict = %+v, want PAUSED", begun.Verdict)
	}

	resumed := e.ExecuteResume(fv, engine.ResumeInput{
		PriorState:    begun,
		ResumePayload: map[string]any{"approved": true},
	})
	if resumed.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v, want SUCCEEDED", resumed.Verdict)
	}
	text, _ := resumed.Steps["summarize"].Output.(map[string]any)["text"].(string)
	if text != `{"wasApproved":true}` {
		t.Fatalf("summarize text = %q", text)
	}
}

// TestCatalog_NewPiecesComposeInRealOrderFlow chains all eight pieces added
// in this batch (storage, approval is pause-specific so excluded here,
// webhook_reply, text, datetime, hash, regex, csv) plus the pre-existing
// webhook trigger into one realistic flow: a webhook delivers a raw order
// line and its HMAC signature, the flow verifies the signature, extracts
// fields via regex, uppercases the order id, stamps a timestamp, builds a
// CSV log line, persists it, and replies synchronously with what it did.
// Every step's input comes from a PRIOR step's output via {{ }} templating
// (including array-index access, {{ x.output.groups[0] }}) — this is the
// piece-to-piece composition unit tests can't catch, since each piece's own
// _test.go calls Run directly with hand-built Input maps.
func TestCatalog_NewPiecesComposeInRealOrderFlow(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	secretKey := []byte("order-webhook-secret-key-123456")
	body := "order_id=ord-4521;amount=99.50"
	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(body))
	wantSignature := hex.EncodeToString(mac.Sum(nil))

	respondStep := &model.FlowAction{
		Name: "respond", DisplayName: "Respond", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "webhook_reply", ActionName: "respond", Input: map[string]any{
			"status": int64(200),
			"body": map[string]any{
				"orderId": "{{ upper_id.output.text }}",
				"fileURL": "{{ persist.output.fileURL }}",
			},
		}},
	}
	persistStep := &model.FlowAction{
		Name: "persist", DisplayName: "Persist", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "storage", ActionName: "write", Input: map[string]any{
			"fileName": "orders.csv",
			"content":  "{{ csv_line.output.text }}",
			"format":   "text",
		}},
		NextAction: respondStep,
	}
	csvLineStep := &model.FlowAction{
		Name: "csv_line", DisplayName: "CSV Line", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "csv", ActionName: "stringify", Input: map[string]any{
			"headers": []any{"orderId", "amount", "status", "timestamp"},
			"rows": []any{
				map[string]any{
					"orderId":   "{{ upper_id.output.text }}",
					"amount":    "{{ extract.output.groups[1] }}",
					"status":    "PROCESSED",
					"timestamp": "{{ now_step.output.iso }}",
				},
			},
		}},
		NextAction: persistStep,
	}
	nowStep := &model.FlowAction{
		Name: "now_step", DisplayName: "Now", Type: model.ActionPiece,
		Piece:      &model.PieceSettings{PieceName: "datetime", ActionName: "now", Input: map[string]any{}},
		NextAction: csvLineStep,
	}
	upperIDStep := &model.FlowAction{
		Name: "upper_id", DisplayName: "Uppercase Order ID", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "text", ActionName: "case", Input: map[string]any{
			"text": "{{ extract.output.groups[0] }}",
			"mode": "upper",
		}},
		NextAction: nowStep,
	}
	extractStep := &model.FlowAction{
		Name: "extract", DisplayName: "Extract Order Fields", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "regex", ActionName: "extract_groups", Input: map[string]any{
			"text":    "{{ trigger_1.output.body }}",
			"pattern": `order_id=([a-z0-9-]+);amount=([0-9.]+)`,
		}},
		NextAction: upperIDStep,
	}
	verifyStep := &model.FlowAction{
		Name: "verify", DisplayName: "Verify Signature", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "hash", ActionName: "hmac", Input: map[string]any{
			piece.AuthInputKey: secretKey,
			"text":             "{{ trigger_1.output.body }}",
			"algorithm":        "sha256",
		}},
		NextAction: extractStep,
	}
	fv := &model.FlowVersion{ID: "fv-new-pieces-compose", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Catch Webhook", Type: model.TriggerPiece,
		PieceName: "webhook", TriggerName: "catch_hook", Input: map[string]any{},
		NextAction: verifyStep,
	}}

	e := engine.New(registry)
	state := e.ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: map[string]any{"body": body, "signature": wantSignature},
		ExecuteTrigger: true,
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	verifyOut := state.Steps["verify"].Output.(map[string]any)
	if verifyOut["hex"] != wantSignature {
		t.Fatalf("hmac = %v, want %v", verifyOut["hex"], wantSignature)
	}

	extractOut := state.Steps["extract"].Output.(map[string]any)
	groups := extractOut["groups"].([]string)
	if len(groups) != 2 || groups[0] != "ord-4521" || groups[1] != "99.50" {
		t.Fatalf("groups = %#v", groups)
	}

	upperOut := state.Steps["upper_id"].Output.(map[string]any)
	if upperOut["text"] != "ORD-4521" {
		t.Fatalf("upper_id text = %q", upperOut["text"])
	}

	nowOut := state.Steps["now_step"].Output.(map[string]any)
	iso, _ := nowOut["iso"].(string)
	if iso == "" {
		t.Fatal("now_step iso is empty")
	}

	csvOut := state.Steps["csv_line"].Output.(map[string]any)
	wantCSVLine := fmt.Sprintf("orderId,amount,status,timestamp\nORD-4521,99.50,PROCESSED,%s\n", iso)
	if csvOut["text"] != wantCSVLine {
		t.Fatalf("csv_line text = %q, want %q", csvOut["text"], wantCSVLine)
	}

	persistOut := state.Steps["persist"].Output.(map[string]any)
	fileURL, _ := persistOut["fileURL"].(string)
	if fileURL == "" {
		t.Fatal("persist fileURL is empty")
	}
	writer := e.Files.(*piece.MemoryFileWriter)
	stored, ok := writer.Get(fileURL)
	if !ok || string(stored) != wantCSVLine {
		t.Fatalf("stored file = %q, ok=%v, want %q", stored, ok, wantCSVLine)
	}

	if state.RespondedEarly == nil || state.RespondedEarly.Status != 200 {
		t.Fatalf("RespondedEarly = %+v", state.RespondedEarly)
	}
	respondedBody := state.RespondedEarly.Body.(map[string]any)
	if respondedBody["orderId"] != "ORD-4521" || respondedBody["fileURL"] != fileURL {
		t.Fatalf("respondedBody = %+v", respondedBody)
	}
}

// TestJSONDefinedFlow_ExecutesThroughRealCatalog is the decisive proof for
// model.ParseFlowVersion: this flow was never built as a Go struct literal
// at all — it exists ONLY as the JSON string below, exactly the shape an
// external caller (a human, or an AI agent) would produce. It's parsed,
// registered against the real catalog, and executed exactly like every
// other integration test in this file. Also exercises hash.hmac's string
// auth path (added specifically because a JSON-defined flow can never
// produce a []byte value for Input[piece.AuthInputKey]).
func TestJSONDefinedFlow_ExecutesThroughRealCatalog(t *testing.T) {
	flowJSON := `{
		"id": "fv-json-defined",
		"trigger": {
			"name": "trigger_1",
			"displayName": "Trigger",
			"type": "EMPTY",
			"nextAction": {
				"name": "sign",
				"displayName": "Sign",
				"type": "PIECE",
				"piece": {
					"pieceName": "hash",
					"actionName": "hmac",
					"input": {
						"text": "{{ trigger_1.output.body }}",
						"algorithm": "sha256",
						"auth": "topsecret"
					}
				},
				"nextAction": {
					"name": "shout",
					"displayName": "Shout",
					"type": "PIECE",
					"piece": {
						"pieceName": "text",
						"actionName": "case",
						"input": {
							"text": "{{ sign.output.hex }}",
							"mode": "upper"
						}
					}
				}
			}
		}
	}`

	fv, err := model.ParseFlowVersion([]byte(flowJSON))
	if err != nil {
		t.Fatalf("ParseFlowVersion: %v", err)
	}

	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: map[string]any{"body": "hello world"},
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write([]byte("hello world"))
	wantHex := hex.EncodeToString(mac.Sum(nil))

	signOut := state.Steps["sign"].Output.(map[string]any)
	if signOut["hex"] != wantHex {
		t.Fatalf("sign hex = %v, want %v", signOut["hex"], wantHex)
	}

	shoutOut := state.Steps["shout"].Output.(map[string]any)
	if shoutOut["text"] != strings.ToUpper(wantHex) {
		t.Fatalf("shout text = %v, want %v", shoutOut["text"], strings.ToUpper(wantHex))
	}
}

// TestCatalog_JSPieceComposesWithRealCatalog proves a JS-authored piece
// (pkg/jspiece) coexists in the same registry as the full Go catalog with
// no special-casing — registered alongside pieces.RegisterAll's thirteen
// Go pieces, chained through {{ }} templating both directions (a real
// catalog trigger feeds it, its own output feeds a real catalog piece),
// and persisted through pkg/pieces/storage. The pure-logic risk scoring
// this JS piece does (no equivalent exists as a Go catalog piece) is
// exactly the kind of one-off, flow-specific logic Phase 2 exists for:
// nobody should have to write and ship a Go piece for it.
func TestCatalog_JSPieceComposesWithRealCatalog(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	riskPiece := jspiece.New("risk_score", "Risk Score", []jspiece.ActionSource{
		{
			Name: "classify", DisplayName: "Classify",
			Source: `(ctx) => {
				const amount = Number(ctx.input.amount);
				let level;
				if (amount > 1000) level = "high";
				else if (amount > 100) level = "medium";
				else level = "low";
				return { level: level, amount: amount };
			}`,
		},
	})
	if err := registry.RegisterValidated(riskPiece); err != nil {
		t.Fatalf("RegisterValidated(risk_score): %v", err)
	}

	persistStep := &model.FlowAction{
		Name: "persist", DisplayName: "Persist", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "storage", ActionName: "write", Input: map[string]any{
			"fileName": "risk.json",
			"content":  "{{ report.output.text }}",
			"format":   "text",
		}},
	}
	reportStep := &model.FlowAction{
		Name: "report", DisplayName: "Report", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "stringify", Input: map[string]any{
			"data": map[string]any{
				"level":  "{{ score.output.level }}",
				"amount": "{{ score.output.amount }}",
			},
		}},
		NextAction: persistStep,
	}
	scoreStep := &model.FlowAction{
		Name: "score", DisplayName: "Score", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "risk_score", ActionName: "classify", Input: map[string]any{
			"amount": "{{ trigger_1.output.amount }}",
		}},
		NextAction: reportStep,
	}
	fv := &model.FlowVersion{ID: "fv-catalog-js", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: scoreStep,
	}}

	e := engine.New(registry)
	state := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{"amount": int64(750)}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	scoreOut := state.Steps["score"].Output.(map[string]any)
	if scoreOut["level"] != "medium" {
		t.Fatalf("score.level = %v, want medium", scoreOut["level"])
	}

	reportOut := state.Steps["report"].Output.(map[string]any)
	reportText, _ := reportOut["text"].(string)
	if reportText == "" {
		t.Fatal("report text is empty")
	}

	persistOut := state.Steps["persist"].Output.(map[string]any)
	fileURL, _ := persistOut["fileURL"].(string)
	writer := e.Files.(*piece.MemoryFileWriter)
	stored, ok := writer.Get(fileURL)
	if !ok || string(stored) != reportText {
		t.Fatalf("stored file = %q, ok=%v, want %q", stored, ok, reportText)
	}
}

// TestCatalog_JSTriggerComposesWithRealCatalog proves a JS-authored
// TRIGGER (jspiece.NewTrigger) works as a real flow's entry point,
// registered alongside the full Go catalog, feeding real catalog pieces
// through {{ }} templating exactly like pkg/pieces/webhook's Go trigger
// does elsewhere in this file.
//
// Real, worth-stating limitation confirmed by reading Engine.ExecuteBegin
// itself: it constructs the trigger's piece.TriggerContext as
// {Payload, Input} only — Store is never set. A JS trigger's ctx.store is
// therefore always unavailable when invoked THIS way; the polling-cursor
// pattern (see pkg/jspiece's own TestJSTrigger_PollingCursorFiltersOnlyNewItems)
// only works when something else calls trig.Run() directly and reuses one
// Store across repeated calls itself — matching piece.MemoryStore's own
// doc comment about simulating a polling scheduler. This trigger is
// deliberately Store-free to stay within what ExecuteBegin actually
// supports.
func TestCatalog_JSTriggerComposesWithRealCatalog(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	orderTrigger := piece.Piece{
		Name: "order_trigger", DisplayName: "Order Trigger",
		Triggers: map[string]piece.Trigger{
			"new_orders": jspiece.NewTrigger(jspiece.TriggerSource{
				Name: "new_orders", DisplayName: "New Orders",
				Source: `(ctx) => ctx.payload.map(item => ({
					id: item.id, amount: item.amount, source: "js-trigger"
				}))`,
			}),
		},
	}
	if err := registry.RegisterValidated(orderTrigger); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}

	reportStep := &model.FlowAction{
		Name: "report", DisplayName: "Report", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "json", ActionName: "stringify", Input: map[string]any{
			"data": map[string]any{
				"id":     "{{ trigger_1.output.id }}",
				"source": "{{ trigger_1.output.source }}",
			},
		}},
	}
	fv := &model.FlowVersion{ID: "fv-catalog-js-trigger", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "New Orders", Type: model.TriggerPiece,
		PieceName: "order_trigger", TriggerName: "new_orders", Input: map[string]any{},
		NextAction: reportStep,
	}}

	payload := []any{
		map[string]any{"id": int64(1), "amount": int64(100)},
		map[string]any{"id": int64(2), "amount": int64(250)},
	}
	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{
		TriggerPayload: payload,
		ExecuteTrigger: true,
	})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}

	// ExecuteBegin only ever uses items[0] of what a PIECE trigger
	// returns — confirmed by reading engine.go before relying on it, not
	// assumed. The second payload item is never seen by this flow run.
	triggerOut := state.Steps["trigger_1"].Output.(map[string]any)
	if triggerOut["id"] != int64(1) || triggerOut["source"] != "js-trigger" {
		t.Fatalf("trigger_1 output = %+v, want the FIRST mapped item", triggerOut)
	}

	reportOut := state.Steps["report"].Output.(map[string]any)
	reportText, _ := reportOut["text"].(string)
	if reportText != `{"id":1,"source":"js-trigger"}` {
		t.Fatalf("report text = %q", reportText)
	}
}

// regionPickerPiece is a JS-authored piece with one Dropdown ("region")
// offering two valid values, and one action ("deploy") that validates its
// "region" Input against that same list itself — the engine has no
// concept of a Dropdown's options being enforced at runtime, so a piece
// that cares has to check its own Input, same as any other validation.
func regionPickerPiece() piece.Piece {
	return piece.Piece{
		Name: "region_picker", DisplayName: "Region Picker",
		Actions: map[string]piece.Action{
			"deploy": jspiece.NewAction(jspiece.ActionSource{
				Name: "deploy", DisplayName: "Deploy",
				Source: `(ctx) => {
					const valid = ["us-east-1", "eu-west-1"];
					if (valid.indexOf(ctx.input.region) === -1) {
						throw new Error("invalid region: " + ctx.input.region);
					}
					return { deployedTo: ctx.input.region, status: "deployed" };
				}`,
				Dropdowns: map[string]jspiece.DropdownSource{
					"region": {
						Source: `(propsValue, ctx) => ({
							options: [
								{ label: "US East", value: "us-east-1" },
								{ label: "EU West", value: "eu-west-1" },
							]
						})`,
					},
				},
			}),
		},
	}
}

// TestCatalog_JSDropdownComposesWithRealCatalog proves a JS-authored
// Dropdown (jspiece.NewDropdown) resolves through the real engine's
// public LoadOptions API — the same call path a real editor UI would use
// to populate a dropdown, not just piece.DropdownProperty.LoadOptions
// called directly — registered alongside the full Go catalog.
func TestCatalog_JSDropdownComposesWithRealCatalog(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := registry.RegisterValidated(regionPickerPiece()); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}

	state, err := engine.New(registry).LoadOptions(engine.LoadOptionsInput{
		PieceName: "region_picker", ActionName: "deploy", PropertyName: "region",
	})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 2 || state.Options[0].Value != "us-east-1" || state.Options[1].Value != "eu-west-1" {
		t.Fatalf("state.Options = %+v", state.Options)
	}
}

// TestFlow_UsesValueSelectedFromJSDropdown simulates the real editor
// workflow end to end: call LoadOptions to discover what values are
// valid, pick one from what came back (not hardcoded), bake that exact
// value into a flow's Input (JSON, same as any Phase-1 flow), validate
// the flow structurally, then actually run it — proving a value a
// Dropdown offered really does work when used for real.
func TestFlow_UsesValueSelectedFromJSDropdown(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := registry.RegisterValidated(regionPickerPiece()); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}

	e := engine.New(registry)
	state, err := e.LoadOptions(engine.LoadOptionsInput{
		PieceName: "region_picker", ActionName: "deploy", PropertyName: "region",
	})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 2 {
		t.Fatalf("state.Options = %+v, want 2", state.Options)
	}
	selected := state.Options[1].Value.(string) // "eu-west-1" — picked from the dropdown's own data, not hardcoded

	flowJSON := fmt.Sprintf(`{
		"id": "fv-dropdown-selected-region",
		"trigger": {
			"name": "trigger_1",
			"displayName": "Trigger",
			"type": "EMPTY",
			"nextAction": {
				"name": "deploy",
				"displayName": "Deploy",
				"type": "PIECE",
				"piece": {
					"pieceName": "region_picker",
					"actionName": "deploy",
					"input": { "region": %q }
				}
			}
		}
	}`, selected)

	fv, err := model.ParseFlowVersion([]byte(flowJSON))
	if err != nil {
		t.Fatalf("ParseFlowVersion: %v", err)
	}
	if errs := flowvalidate.Validate(fv, registry); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}

	result := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if result.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", result.Verdict)
	}
	out := result.Steps["deploy"].Output.(map[string]any)
	if out["deployedTo"] != selected || out["status"] != "deployed" {
		t.Fatalf("out = %+v, want deployedTo=%q", out, selected)
	}
}

// TestFlow_RegionOutsideDropdownFailsClearly confirms, directly rather
// than assuming, that goflow's Dropdowns are advisory metadata only — the
// engine never checks a PIECE action's Input against what a Dropdown's
// LoadOptions offers. A value never returned by the dropdown still
// reaches the action's Run unchanged; only regionPickerPiece's OWN
// validation (thrown from JS) is what rejects it here.
func TestFlow_RegionOutsideDropdownFailsClearly(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := registry.RegisterValidated(regionPickerPiece()); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}

	deployStep := &model.FlowAction{
		Name: "deploy", DisplayName: "Deploy", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "region_picker", ActionName: "deploy", Input: map[string]any{
			"region": "ap-south-1", // never offered by the dropdown
		}},
	}
	fv := &model.FlowVersion{ID: "fv-region-outside-dropdown", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: deployStep,
	}}

	// flowvalidate has no way to know "ap-south-1" is invalid either — it
	// only checks structure/syntax, confirming this really is purely the
	// piece's own runtime concern.
	if errs := flowvalidate.Validate(fv, registry); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors — an out-of-dropdown value is not a structural problem", errs)
	}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — region_picker.deploy rejects any region outside its own valid list", state.Verdict)
	}
}

// zonesByRegion is the shared source of truth both the "zone" dropdown and
// the "deploy" action's own validation read from — kept in one Go map so
// the JS embedded below and this test's own assertions can't drift apart.
var zonesByRegion = map[string][]string{
	"us-east-1": {"us-east-1a", "us-east-1b"},
	"eu-west-1": {"eu-west-1a", "eu-west-1b"},
}

// cloudDeployPiece has two chained Dropdowns: "region" is independent,
// "zone" declares Refreshers: ["region"] and returns a DIFFERENT option
// set depending on propsValue.region — Refreshers itself is purely
// documentation (see piece.DropdownProperty's doc comment: "nothing
// enforces it"); the actual chaining is entirely LoadOptions reading
// propsValue itself, same as any other Dropdown. Its "deploy" action
// validates region+zone belong together, the same "engine doesn't
// enforce Dropdowns, the piece validates its own Input" pattern
// TestFlow_RegionOutsideDropdownFailsClearly already established.
func cloudDeployPiece() piece.Piece {
	return piece.Piece{
		Name: "cloud_deploy", DisplayName: "Cloud Deploy",
		Actions: map[string]piece.Action{
			"deploy": jspiece.NewAction(jspiece.ActionSource{
				Name: "deploy", DisplayName: "Deploy",
				Source: `(ctx) => {
					const zonesByRegion = {
						"us-east-1": ["us-east-1a", "us-east-1b"],
						"eu-west-1": ["eu-west-1a", "eu-west-1b"]
					};
					const region = ctx.input.region;
					const zone = ctx.input.zone;
					const validZones = zonesByRegion[region];
					if (!validZones || validZones.indexOf(zone) === -1) {
						throw new Error("invalid zone " + zone + " for region " + region);
					}
					return { deployedTo: zone, region: region, status: "deployed" };
				}`,
				Dropdowns: map[string]jspiece.DropdownSource{
					"region": {
						Source: `(propsValue, ctx) => ({
							options: [
								{ label: "US East", value: "us-east-1" },
								{ label: "EU West", value: "eu-west-1" },
							]
						})`,
					},
					"zone": {
						Refreshers: []string{"region"},
						Source: `(propsValue, ctx) => {
							const zonesByRegion = {
								"us-east-1": ["us-east-1a", "us-east-1b"],
								"eu-west-1": ["eu-west-1a", "eu-west-1b"]
							};
							const region = propsValue.region;
							const zones = zonesByRegion[region];
							if (!zones) {
								return { disabled: true, placeholder: "select a region first", options: [] };
							}
							return { options: zones.map(z => ({ label: z, value: z })) };
						}`,
					},
				},
			}),
		},
	}
}

// TestJSDropdown_RefreshersDeclarationIsPreserved confirms Refreshers
// round-trips through NewDropdown into the real piece.DropdownProperty —
// it's documentation only (nothing reads it to auto-trigger a reload),
// but a piece author or an editor UI still needs it to actually be there.
func TestJSDropdown_RefreshersDeclarationIsPreserved(t *testing.T) {
	p := cloudDeployPiece()
	dd := p.Actions["deploy"].Dropdowns["zone"]
	if len(dd.Refreshers) != 1 || dd.Refreshers[0] != "region" {
		t.Fatalf("Refreshers = %+v, want [\"region\"]", dd.Refreshers)
	}
}

// TestJSDropdown_ChainedOptionsDependOnSiblingPropsValue is the core proof
// for chained dropdowns: calling LoadOptions for "zone" with a DIFFERENT
// propsValue.region each time returns genuinely different option sets —
// not a static list ignoring its input.
func TestJSDropdown_ChainedOptionsDependOnSiblingPropsValue(t *testing.T) {
	dd := cloudDeployPiece().Actions["deploy"].Dropdowns["zone"]

	usEast, err := dd.LoadOptions(map[string]any{"region": "us-east-1"}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions(us-east-1) error = %v", err)
	}
	if len(usEast.Options) != 2 || usEast.Options[0].Value != "us-east-1a" {
		t.Fatalf("us-east-1 options = %+v", usEast.Options)
	}

	euWest, err := dd.LoadOptions(map[string]any{"region": "eu-west-1"}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions(eu-west-1) error = %v", err)
	}
	if len(euWest.Options) != 2 || euWest.Options[0].Value != "eu-west-1a" {
		t.Fatalf("eu-west-1 options = %+v", euWest.Options)
	}
}

// TestJSDropdown_ChainedDropdownDisabledWithoutParentSelected covers the
// realistic UX case: before "region" has been picked at all, "zone"
// should come back disabled with a helpful placeholder instead of an
// empty (and unexplained) options list.
func TestJSDropdown_ChainedDropdownDisabledWithoutParentSelected(t *testing.T) {
	dd := cloudDeployPiece().Actions["deploy"].Dropdowns["zone"]

	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if !state.Disabled || state.Placeholder == "" {
		t.Fatalf("state = %+v, want Disabled=true with a placeholder", state)
	}
	if len(state.Options) != 0 {
		t.Fatalf("state.Options = %+v, want empty", state.Options)
	}
}

// TestFlow_UsesChainedDropdownSelections simulates the full real editor
// sequence for chained dropdowns: load "region" options, pick one, load
// "zone" options WITH that region in propsValue (what Refreshers tells a
// real UI to do), pick one of THOSE, then actually run a flow built from
// both selections — registered alongside the full Go catalog.
func TestFlow_UsesChainedDropdownSelections(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := registry.RegisterValidated(cloudDeployPiece()); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}
	e := engine.New(registry)

	regionState, err := e.LoadOptions(engine.LoadOptionsInput{
		PieceName: "cloud_deploy", ActionName: "deploy", PropertyName: "region",
	})
	if err != nil {
		t.Fatalf("LoadOptions(region) error = %v", err)
	}
	selectedRegion := regionState.Options[1].Value.(string) // "eu-west-1"

	zoneState, err := e.LoadOptions(engine.LoadOptionsInput{
		PieceName: "cloud_deploy", ActionName: "deploy", PropertyName: "zone",
		Input: map[string]any{"region": selectedRegion},
	})
	if err != nil {
		t.Fatalf("LoadOptions(zone) error = %v", err)
	}
	if len(zoneState.Options) != len(zonesByRegion[selectedRegion]) {
		t.Fatalf("zoneState.Options = %+v, want %d options for %s", zoneState.Options, len(zonesByRegion[selectedRegion]), selectedRegion)
	}
	selectedZone := zoneState.Options[0].Value.(string) // "eu-west-1a"

	flowJSON := fmt.Sprintf(`{
		"id": "fv-chained-dropdowns",
		"trigger": {
			"name": "trigger_1",
			"displayName": "Trigger",
			"type": "EMPTY",
			"nextAction": {
				"name": "deploy",
				"displayName": "Deploy",
				"type": "PIECE",
				"piece": {
					"pieceName": "cloud_deploy",
					"actionName": "deploy",
					"input": { "region": %q, "zone": %q }
				}
			}
		}
	}`, selectedRegion, selectedZone)

	fv, err := model.ParseFlowVersion([]byte(flowJSON))
	if err != nil {
		t.Fatalf("ParseFlowVersion: %v", err)
	}
	if errs := flowvalidate.Validate(fv, registry); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}

	result := e.ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if result.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", result.Verdict)
	}
	out := result.Steps["deploy"].Output.(map[string]any)
	if out["deployedTo"] != selectedZone || out["region"] != selectedRegion {
		t.Fatalf("out = %+v, want deployedTo=%q region=%q", out, selectedZone, selectedRegion)
	}
}

// TestFlow_ZoneFromWrongRegionFailsClearly proves the chaining is purely
// an editor-time convenience — nothing stops a flow's Input from pairing
// a zone with a DIFFERENT region than the one it actually belongs to
// (e.g. hand-edited JSON, or a stale value from before the region
// changed). Same "the piece validates its own Input" story as
// TestFlow_RegionOutsideDropdownFailsClearly, one level deeper.
func TestFlow_ZoneFromWrongRegionFailsClearly(t *testing.T) {
	registry := piece.NewRegistry()
	if err := pieces.RegisterAll(registry); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if err := registry.RegisterValidated(cloudDeployPiece()); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}

	deployStep := &model.FlowAction{
		Name: "deploy", DisplayName: "Deploy", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "cloud_deploy", ActionName: "deploy", Input: map[string]any{
			"region": "us-east-1",
			"zone":   "eu-west-1a", // belongs to eu-west-1, not us-east-1
		}},
	}
	fv := &model.FlowVersion{ID: "fv-zone-wrong-region", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: deployStep,
	}}

	if errs := flowvalidate.Validate(fv, registry); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors — a mismatched region/zone pair is not a structural problem", errs)
	}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})
	if state.Verdict.Status != model.FlowRunFailed {
		t.Fatalf("verdict = %+v, want FAILED — cloud_deploy.deploy rejects a zone that doesn't belong to its region", state.Verdict)
	}
}
