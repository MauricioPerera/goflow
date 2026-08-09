// Package http provides goflow's "HTTP" catalog piece: send a real HTTP
// request via Go's net/http and return the response. Auth (ctx.Auth) becomes
// the Authorization header two ways: a plain string is sent verbatim —
// deliberately not assuming a scheme (Bearer/Basic/etc.), the caller decides
// the full header value, same "auth is just a value, this engine doesn't
// interpret it" stance as everywhere else in goflow — while a
// *piece.OAuth2Auth is formatted as "Bearer <AccessToken>", the de facto
// universal convention for OAuth2 bearer tokens (what practically every
// activepieces piece using OAuth2 auth does by hand when building its own
// request). Any other ctx.Auth type/shape is not sent at all.
package http

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"goflow/pkg/piece"
)

const PieceName = "http"

// DefaultTimeout applies when Input["timeoutMs"] isn't set. A piece making
// real network calls with no timeout at all can hang a flow run forever —
// this bounds that, same reasoning as any production HTTP client.
var DefaultTimeout = 10 * time.Second

// DefaultMaxRateLimitRetries applies when respectRetryAfter is set but
// Input["maxRateLimitRetries"] isn't.
const DefaultMaxRateLimitRetries = 3

// DefaultMaxRetryAfterWait caps how long a single respected Retry-After delay
// is allowed to be. A misconfigured or hostile server could send a huge
// Retry-After value; without a cap this action would block the flow run for
// however long the server asked, which is a self-inflicted denial of
// service. Past this cap the action treats the 429 as a normal failure
// instead of waiting.
var DefaultMaxRetryAfterWait = 30 * time.Second

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "HTTP",
		Actions: map[string]piece.Action{
			"request": requestAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func requestAction() piece.Action {
	return piece.Action{
		Name: "request", DisplayName: "Send HTTP Request",
		Run: func(ctx piece.ActionContext) (any, error) {
			url, _ := ctx.Input["url"].(string)
			if url == "" {
				return nil, fmt.Errorf("missing required input: url")
			}
			method, _ := ctx.Input["method"].(string)
			if method == "" {
				method = http.MethodGet
			}
			method = strings.ToUpper(method)

			buildRequest := func() (*http.Request, error) {
				var bodyReader io.Reader
				if body, ok := ctx.Input["body"].(string); ok && body != "" {
					bodyReader = strings.NewReader(body)
				}
				req, err := http.NewRequest(method, url, bodyReader)
				if err != nil {
					return nil, fmt.Errorf("building request: %w", err)
				}
				if headers, ok := ctx.Input["headers"].(map[string]any); ok {
					for k, v := range headers {
						if s, ok := v.(string); ok {
							req.Header.Set(k, s)
						}
					}
				}
				switch auth := ctx.Auth.(type) {
				case string:
					if auth != "" {
						req.Header.Set("Authorization", auth)
					}
				case *piece.OAuth2Auth:
					if auth != nil && auth.AccessToken != "" {
						req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
					}
				}
				return req, nil
			}

			timeout := DefaultTimeout
			if ms, ok := toMilliseconds(ctx.Input["timeoutMs"]); ok {
				timeout = time.Duration(ms) * time.Millisecond
			}
			client := &http.Client{Timeout: timeout}

			// Off by default: many flows legitimately want to inspect a 404/500
			// themselves (e.g. branch on status via a ROUTER) rather than have the
			// step fail. Set failOnErrorStatus:true to make a >=400 response an
			// actual error instead — the only way for this action to ever
			// participate in the engine's RetryOnFailure/ContinueOnFailure, since
			// neither ever look at Output, only at whether Run returned an error.
			failOnErrorStatus, _ := ctx.Input["failOnErrorStatus"].(bool)

			// Off by default: when set, a 429 with a Retry-After header is handled
			// INSIDE this action (sleep, then re-send) instead of surfacing
			// immediately. This is deliberately separate from the engine's
			// RetryOnFailure — the engine's backoff is fixed and knows nothing
			// about the response, so it can never wait the exact duration a server
			// asked for. Only this piece can, since only it sees the header.
			respectRetryAfter, _ := ctx.Input["respectRetryAfter"].(bool)
			maxRateLimitRetries := DefaultMaxRateLimitRetries
			if n, ok := toMilliseconds(ctx.Input["maxRateLimitRetries"]); ok {
				maxRateLimitRetries = int(n)
			}
			maxRetryAfterWait := DefaultMaxRetryAfterWait
			if ms, ok := toMilliseconds(ctx.Input["maxRetryAfterWaitMs"]); ok {
				maxRetryAfterWait = time.Duration(ms) * time.Millisecond
			}

			var resp *http.Response
			var respBody []byte
			for attempt := 0; ; attempt++ {
				req, err := buildRequest()
				if err != nil {
					return nil, err
				}
				resp, err = client.Do(req)
				if err != nil {
					return nil, fmt.Errorf("request failed: %w", err)
				}
				respBody, err = io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return nil, fmt.Errorf("reading response body: %w", err)
				}

				if !respectRetryAfter || resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRateLimitRetries {
					break
				}
				wait, ok := parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
				if !ok || wait > maxRetryAfterWait {
					break
				}
				time.Sleep(wait)
			}

			headers := map[string]any{}
			for k, v := range resp.Header {
				if len(v) > 0 {
					headers[k] = v[0]
				}
			}

			if failOnErrorStatus && resp.StatusCode >= 400 {
				return nil, fmt.Errorf("request to %s failed with status %d: %s", url, resp.StatusCode, string(respBody))
			}

			return map[string]any{
				"status":  resp.StatusCode,
				"headers": headers,
				"body":    string(respBody),
			}, nil
		},
	}
}

func toMilliseconds(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// parseRetryAfterSeconds supports only the simple "delay-seconds" form of the
// Retry-After header (a non-negative integer), which is what rate-limited
// JSON APIs overwhelmingly send in practice. The alternative HTTP-date form
// is not supported — documented as a v1 scope limitation, not silently
// mishandled: an unparseable header just means the wait is skipped and the
// response is returned as-is (or fails normally via failOnErrorStatus).
func parseRetryAfterSeconds(header string) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	n, err := strconv.Atoi(header)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}
