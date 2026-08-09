package jspiece_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"goflow/pkg/engine"
	"goflow/pkg/jspiece"
	"goflow/pkg/model"
	"goflow/pkg/piece"
)

func TestJSPiece_BasicReturnValue(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "greet", DisplayName: "Greet",
		Source: `(ctx) => ({ greeting: "hello " + ctx.input.name })`,
	})

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"name": "world"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.(map[string]any)["greeting"] != "hello world" {
		t.Fatalf("out = %#v", out)
	}
}

func TestJSPiece_AuthPlainString(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ({ auth: ctx.auth })`,
	})
	out, err := act.Run(piece.ActionContext{Auth: "bearer-token"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.(map[string]any)["auth"] != "bearer-token" {
		t.Fatalf("out = %#v", out)
	}
}

func TestJSPiece_AuthBytesBecomeString(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ({ auth: ctx.auth, len: ctx.auth.length })`,
	})
	out, err := act.Run(piece.ActionContext{Auth: []byte("secretkey")})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["auth"] != "secretkey" {
		t.Fatalf("out = %#v", m)
	}
}

func TestJSPiece_AuthOAuth2BecomesObject(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ({ token: ctx.auth.accessToken, tokenType: ctx.auth.data.token_type })`,
	})
	out, err := act.Run(piece.ActionContext{Auth: &piece.OAuth2Auth{
		AccessToken: "at-123", Data: map[string]any{"token_type": "Bearer"},
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["token"] != "at-123" || m["tokenType"] != "Bearer" {
		t.Fatalf("out = %#v", m)
	}
}

func TestJSPiece_ExecutionTypeAndResumePayload(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ({ type: ctx.executionType, resume: ctx.resumePayload })`,
	})
	out, err := act.Run(piece.ActionContext{
		ExecutionType: model.ExecutionResume,
		ResumePayload: map[string]any{"approved": true},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["type"] != "RESUME" {
		t.Fatalf("type = %v", m["type"])
	}
	resume := m["resume"].(map[string]any)
	if resume["approved"] != true {
		t.Fatalf("resume = %#v", resume)
	}
}

func TestJSPiece_FilesWrite(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ({ url: ctx.files.write("out.txt", "hello from js") })`,
	})
	writer := piece.NewMemoryFileWriter()
	out, err := act.Run(piece.ActionContext{Files: writer})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	url, _ := out.(map[string]any)["url"].(string)
	if url == "" {
		t.Fatal("url is empty")
	}
	data, ok := writer.Get(url)
	if !ok || string(data) != "hello from js" {
		t.Fatalf("stored data = %q, ok=%v", data, ok)
	}
}

func TestJSPiece_RunStop(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => { ctx.run.stop({status: 201, body: {ok: true}}); return "done"; }`,
	})
	var got *model.WebhookResponse
	out, err := act.Run(piece.ActionContext{
		Run: piece.RunHooks{Stop: func(resp *model.WebhookResponse) { got = resp }},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out != "done" {
		t.Fatalf("out = %#v", out)
	}
	if got == nil || got.Status != 201 {
		t.Fatalf("got = %+v", got)
	}
	body := got.Body.(map[string]any)
	if body["ok"] != true {
		t.Fatalf("body = %#v", body)
	}
}

func TestJSPiece_RunRespondAndWaitForWaitpoint(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => { ctx.run.respond({status: 200}); ctx.run.waitForWaitpoint("wp-1"); return null; }`,
	})
	var respondCalled, pauseCalled bool
	var pauseID string
	_, err := act.Run(piece.ActionContext{
		Run: piece.RunHooks{
			Respond:          func(resp *model.WebhookResponse) { respondCalled = true },
			WaitForWaitpoint: func(id string) { pauseCalled = true; pauseID = id },
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !respondCalled || !pauseCalled || pauseID != "wp-1" {
		t.Fatalf("respondCalled=%v pauseCalled=%v pauseID=%q", respondCalled, pauseCalled, pauseID)
	}
}

func TestJSPiece_FetchGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello world"))
	}))
	defer server.Close()

	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ctx.fetch({url: ctx.input.url})`,
	})
	out, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["status"] != 200 {
		t.Fatalf("status = %v (%T), want 200", m["status"], m["status"])
	}
	if m["body"] != "hello world" {
		t.Fatalf("body = %v", m["body"])
	}
	headers := m["headers"].(map[string]any)
	if headers["X-Custom"] != "yes" {
		t.Fatalf("headers = %+v", headers)
	}
}

func TestJSPiece_FetchPostWithBodyAndHeaders(t *testing.T) {
	var gotMethod, gotBody, gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		gotHeader = r.Header.Get("X-Trace")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ctx.fetch({
			url: ctx.input.url, method: "POST", body: "payload",
			headers: {"X-Trace": "abc-123"}
		})`,
	})
	out, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gotMethod != "POST" || gotBody != "payload" || gotHeader != "abc-123" {
		t.Fatalf("gotMethod=%q gotBody=%q gotHeader=%q", gotMethod, gotBody, gotHeader)
	}
	if out.(map[string]any)["status"] != 201 {
		t.Fatalf("out = %#v", out)
	}
}

func TestJSPiece_FetchMissingURLFailsClearly(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => ctx.fetch({})`,
	})
	_, err := act.Run(piece.ActionContext{})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-url error")
	}
}

func TestJSPiece_ThrownExceptionBecomesError(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => { throw new Error("boom"); }`,
	})
	_, err := act.Run(piece.ActionContext{})
	if err == nil {
		t.Fatal("Run() error = nil, want the thrown error surfaced")
	}
}

func TestJSPiece_NonFunctionSourceFailsClearly(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `{ notAFunction: true }`,
	})
	_, err := act.Run(piece.ActionContext{})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection for non-function source")
	}
}

func TestJSPiece_PromiseReturnFailsClearly(t *testing.T) {
	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => Promise.resolve(42)`,
	})
	_, err := act.Run(piece.ActionContext{})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection — async/await is not supported")
	}
}

func TestJSPiece_InfiniteLoopIsInterrupted(t *testing.T) {
	original := jspiece.DefaultTimeout
	jspiece.DefaultTimeout = 50 * time.Millisecond
	defer func() { jspiece.DefaultTimeout = original }()

	act := jspiece.NewAction(jspiece.ActionSource{
		Name: "a", Source: `(ctx) => { while (true) {} }`,
	})
	start := time.Now()
	_, err := act.Run(piece.ActionContext{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run() error = nil, want a timeout error for an infinite loop")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run() took %v, want it to be interrupted quickly after the 50ms timeout", elapsed)
	}
}

func TestJSPiece_BuildsAValidatablePiece(t *testing.T) {
	p := jspiece.New("greeter", "Greeter", []jspiece.ActionSource{
		{Name: "greet", DisplayName: "Greet", Source: `(ctx) => ({ greeting: "hi" })`},
	})
	if errs := piece.Validate(p); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
	registry := piece.NewRegistry()
	if err := registry.RegisterValidated(p); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}
	act, ok := registry.GetAction("greeter", "greet")
	if !ok {
		t.Fatal("greeter.greet not resolvable after registration")
	}
	out, err := act.Run(piece.ActionContext{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.(map[string]any)["greeting"] != "hi" {
		t.Fatalf("out = %#v", out)
	}
}

// TestJSPiece_RunsThroughRealEngineFlow is the decisive proof for this
// whole package: a piece that was never written as Go code — only as JS
// source strings, exactly what an agent registering a genuinely new piece
// at runtime would produce — chained through a real two-step flow via
// {{ }} templating, fetching from a real (httptest) server, exactly like
// any hand-authored Go piece elsewhere in this project. The engine never
// finds out this piece is JS-backed.
func TestJSPiece_RunsThroughRealEngineFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("42"))
	}))
	defer server.Close()

	jsPiece := jspiece.New("doubler", "Doubler", []jspiece.ActionSource{
		{
			Name: "fetch_and_double", DisplayName: "Fetch and Double",
			Source: `(ctx) => {
				const res = ctx.fetch({url: ctx.input.url});
				return { doubled: Number(res.body) * 2 };
			}`,
		},
		{
			Name: "shout", DisplayName: "Shout",
			Source: `(ctx) => ({ text: "RESULT: " + ctx.input.n })`,
		},
	})

	registry := piece.NewRegistry()
	if err := registry.RegisterValidated(jsPiece); err != nil {
		t.Fatalf("RegisterValidated: %v", err)
	}

	shoutStep := &model.FlowAction{
		Name: "shout", DisplayName: "Shout", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "doubler", ActionName: "shout", Input: map[string]any{
			"n": "{{ fetch_and_double.output.doubled }}",
		}},
	}
	fetchStep := &model.FlowAction{
		Name: "fetch_and_double", DisplayName: "Fetch and Double", Type: model.ActionPiece,
		Piece: &model.PieceSettings{PieceName: "doubler", ActionName: "fetch_and_double", Input: map[string]any{
			"url": server.URL,
		}},
		NextAction: shoutStep,
	}
	fv := &model.FlowVersion{ID: "fv-jspiece", Trigger: &model.FlowTrigger{
		Name: "trigger_1", DisplayName: "Trigger", Type: model.TriggerEmpty,
		NextAction: fetchStep,
	}}

	state := engine.New(registry).ExecuteBegin(fv, engine.BeginInput{TriggerPayload: map[string]any{}})

	if state.Verdict.Status != model.FlowRunSucceeded {
		t.Fatalf("verdict = %+v", state.Verdict)
	}
	fetchOut := state.Steps["fetch_and_double"].Output.(map[string]any)
	if fetchOut["doubled"] != int64(84) {
		t.Fatalf("doubled = %v (%T), want 84", fetchOut["doubled"], fetchOut["doubled"])
	}
	shoutOut := state.Steps["shout"].Output.(map[string]any)
	if shoutOut["text"] != "RESULT: 84" {
		t.Fatalf("text = %v", shoutOut["text"])
	}
}
