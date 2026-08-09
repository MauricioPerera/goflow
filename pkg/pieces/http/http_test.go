package http_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"goflow/pkg/piece"
	httppiece "goflow/pkg/pieces/http"
)

// newMux builds a single-handler httptest server mux — the tests below each
// only need one endpoint, so this avoids repeating the http.NewServeMux
// boilerplate per test.
func newMux(handler http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	return mux
}

func TestHTTP_GetRequest(t *testing.T) {
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello world"))
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"url": server.URL + "/hello",
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["status"] != 200 {
		t.Fatalf("status = %v, want 200", m["status"])
	}
	if m["body"] != "hello world" {
		t.Fatalf("body = %v", m["body"])
	}
	headers := m["headers"].(map[string]any)
	if headers["X-Custom"] != "yes" {
		t.Fatalf("headers = %+v, want X-Custom=yes", headers)
	}
}

func TestHTTP_PostRequestWithBodyAndHeaders(t *testing.T) {
	var gotMethod, gotBody, gotHeader string
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotHeader = r.Header.Get("X-Trace")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("created"))
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"method":  "post",
		"url":     server.URL + "/things",
		"body":    `{"name":"x"}`,
		"headers": map[string]any{"X-Trace": "abc-123"},
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gotMethod != "POST" {
		t.Fatalf("server saw method = %q, want POST (lowercase input must be normalized)", gotMethod)
	}
	if gotBody != `{"name":"x"}` {
		t.Fatalf("server saw body = %q", gotBody)
	}
	if gotHeader != "abc-123" {
		t.Fatalf("server saw X-Trace = %q", gotHeader)
	}
	m := out.(map[string]any)
	if m["status"] != 201 || m["body"] != "created" {
		t.Fatalf("out = %+v", m)
	}
}

func TestHTTP_AuthSentAsAuthorizationHeaderVerbatim(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"url": server.URL},
		Auth:  "Bearer test-token-123",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gotAuth != "Bearer test-token-123" {
		t.Fatalf("server saw Authorization = %q, want the auth value passed through verbatim (no assumed scheme)", gotAuth)
	}
}

func TestHTTP_OAuth2AuthSentAsBearerToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"url": server.URL},
		Auth: &piece.OAuth2Auth{
			AccessToken: "at-abc-123",
			Data:        map[string]any{"token_type": "Bearer"},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gotAuth != "Bearer at-abc-123" {
		t.Fatalf("server saw Authorization = %q, want %q", gotAuth, "Bearer at-abc-123")
	}
}

func TestHTTP_NilOAuth2AuthSendsNoAuthorizationHeader(t *testing.T) {
	var gotAuth string
	var sawHeader bool
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, sawHeader = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"url": server.URL},
		Auth:  (*piece.OAuth2Auth)(nil),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if sawHeader {
		t.Fatalf("server saw Authorization = %q, want no header at all for a nil OAuth2Auth", gotAuth)
	}
}

func TestHTTP_ErrorStatusDoesNotFailByDefault(t *testing.T) {
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — failOnErrorStatus defaults to false, a 500 is just data", err)
	}
	m := out.(map[string]any)
	if m["status"] != 500 || m["body"] != "boom" {
		t.Fatalf("out = %+v", m)
	}
}

func TestHTTP_FailOnErrorStatusOptedIn(t *testing.T) {
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"url":               server.URL,
		"failOnErrorStatus": true,
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want a failure — failOnErrorStatus was set and the server returned 500")
	}
}

func TestHTTP_MissingURLFailsClearly(t *testing.T) {
	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-url error")
	}
}

func TestHTTP_MalformedURLFailsClearly(t *testing.T) {
	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"url": "http://[::1]:namedport", // invalid: named port instead of numeric
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want a request-building failure for a malformed URL")
	}
}

func TestHTTP_MethodDefaultsToGET(t *testing.T) {
	var gotMethod string
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("server saw method = %q, want GET when \"method\" input is omitted", gotMethod)
	}
}

func TestHTTP_RespectsRetryAfterHeader(t *testing.T) {
	var requestCount int
	var firstRequestAt, secondRequestAt time.Time
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			firstRequestAt = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("slow down"))
			return
		}
		secondRequestAt = time.Now()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok now"))
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"url":               server.URL,
		"respectRetryAfter": true,
	}})
	if err != nil {
		t.Fatalf("Run() error = %v, want the action to wait out the Retry-After and succeed on retry", err)
	}
	if requestCount != 2 {
		t.Fatalf("requestCount = %d, want exactly 2 (1 rate-limited + 1 retry)", requestCount)
	}
	if elapsed := secondRequestAt.Sub(firstRequestAt); elapsed < 1*time.Second {
		t.Fatalf("retry fired after %v, want it to actually wait out the 1s Retry-After instead of retrying immediately", elapsed)
	}
	m := out.(map[string]any)
	if m["status"] != 200 || m["body"] != "ok now" {
		t.Fatalf("out = %+v", m)
	}
}

func TestHTTP_RetryAfterIgnoredWhenNotOptedIn(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{"url": server.URL}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — respectRetryAfter defaults to false, a 429 is just data", err)
	}
	if requestCount != 1 {
		t.Fatalf("requestCount = %d, want exactly 1 — no internal retry without opting in", requestCount)
	}
	m := out.(map[string]any)
	if m["status"] != 429 {
		t.Fatalf("status = %v, want 429", m["status"])
	}
}

func TestHTTP_RetryAfterExceedingCapIsNotWaited(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Retry-After", "999")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	start := time.Now()
	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"url":                 server.URL,
		"respectRetryAfter":   true,
		"maxRetryAfterWaitMs": int64(50),
	}})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — failOnErrorStatus wasn't set, so the un-waited 429 is just data", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Run() took %v, want it to give up fast instead of waiting the server's 999s Retry-After", elapsed)
	}
	if requestCount != 1 {
		t.Fatalf("requestCount = %d, want exactly 1 — a wait past the cap must not be attempted at all", requestCount)
	}
	m := out.(map[string]any)
	if m["status"] != 429 {
		t.Fatalf("status = %v, want 429", m["status"])
	}
}

func TestHTTP_MaxRateLimitRetriesBoundsInternalRetries(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"url":                 server.URL,
		"respectRetryAfter":   true,
		"maxRateLimitRetries": int64(2),
		"failOnErrorStatus":   true,
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want an error — the server never stops rate-limiting")
	}
	if requestCount != 3 {
		t.Fatalf("requestCount = %d, want exactly 3 (1 initial + 2 retries, then give up)", requestCount)
	}
}

func TestHTTP_TimeoutIsEnforced(t *testing.T) {
	server := httptest.NewServer(newMux(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := httppiece.New()
	act := p.Actions["request"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"url":       server.URL,
		"timeoutMs": int64(5),
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want a timeout error — the server deliberately took 50ms against a 5ms timeout")
	}
}
