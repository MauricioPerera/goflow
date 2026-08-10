package oauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testToken = "secret-token"

func newTestServer() *Server {
	return NewServer(testToken, "http://issuer.example")
}

// fixedPKCEPair returns a deterministic verifier/S256-challenge pair — fine
// for tests, since PKCE's security property (a challenge, once sent, can't
// be reversed to the verifier) doesn't depend on any single pair's secrecy.
func fixedPKCEPair() (verifier, challenge string) {
	verifier = "test-verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func registerClient(t *testing.T, s *Server, redirectURI string) string {
	t.Helper()
	body, _ := json.Marshal(registerRequest{ClientName: "test client", RedirectURIs: []string{redirectURI}})
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("HandleRegister status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding register response: %v", err)
	}
	if resp.ClientID == "" {
		t.Fatalf("register response has no client_id: %s", rec.Body.String())
	}
	return resp.ClientID
}

// authorizeParamsValues builds the query/form values HandleAuthorize expects.
func authorizeParamsValues(clientID, redirectURI, state, challenge string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
}

// authorizeWithBearer drives GET /oauth/authorize with a correct Bearer
// header — the machine/CLI path, no login form involved — and returns the
// issued code.
func authorizeWithBearer(t *testing.T, s *Server, clientID, redirectURI, state, challenge string) string {
	t.Helper()
	q := authorizeParamsValues(clientID, redirectURI, state, challenge)
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.HandleAuthorize(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("HandleAuthorize status = %d, want %d, body = %s", rec.Code, http.StatusFound, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing Location %q: %v", rec.Header().Get("Location"), err)
	}
	if got := loc.Query().Get("state"); got != state {
		t.Fatalf("redirect state = %q, want %q", got, state)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect Location %q carries no code", loc)
	}
	return code
}

func tokenRequest(t *testing.T, s *Server, form url.Values) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleToken(rec, req)
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding token response: %v (body: %s)", err, rec.Body.String())
		}
	}
	return rec, body
}

func exchangeCode(t *testing.T, s *Server, code, redirectURI, clientID, verifier string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return tokenRequest(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
}

// TestFullFlow_RegisterAuthorizeExchangeValidateRefresh exercises the whole
// OAuth 2.1 dance end to end: dynamic registration, an authorization-code
// grant with PKCE, using the resulting access token, and rotating it via
// refresh_token — proving the pieces actually compose, not just that each
// endpoint works in isolation.
func TestFullFlow_RegisterAuthorizeExchangeValidateRefresh(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	verifier, challenge := fixedPKCEPair()

	code := authorizeWithBearer(t, s, clientID, redirectURI, "xyz-state", challenge)

	rec, body := exchangeCode(t, s, code, redirectURI, clientID, verifier)
	if rec.Code != http.StatusOK {
		t.Fatalf("token exchange status = %d, want 200, body = %v", rec.Code, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("token response missing access_token/refresh_token: %v", body)
	}
	if body["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", body["token_type"])
	}

	if !s.ValidateAccessToken(access) {
		t.Fatal("ValidateAccessToken(access) = false, want true right after issuance")
	}

	// Refresh: mint a new pair, and the OLD refresh token must be dead
	// (rotation, not reuse).
	rec2, body2 := tokenRequest(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if rec2.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200, body = %v", rec2.Code, body2)
	}
	newAccess, _ := body2["access_token"].(string)
	newRefresh, _ := body2["refresh_token"].(string)
	if newAccess == "" || newRefresh == "" {
		t.Fatalf("refresh response missing tokens: %v", body2)
	}
	if newAccess == access || newRefresh == refresh {
		t.Fatal("refresh must mint a NEW access token and a NEW (rotated) refresh token, not reuse the old ones")
	}
	if !s.ValidateAccessToken(newAccess) {
		t.Fatal("ValidateAccessToken(newAccess) = false, want true")
	}

	rec3, body3 := tokenRequest(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if rec3.Code != http.StatusBadRequest || body3["error"] != "invalid_grant" {
		t.Fatalf("reusing a rotated-away refresh token = %d/%v, want 400/invalid_grant", rec3.Code, body3)
	}
}

func TestValidateAccessToken_UnknownOrEmptyRejected(t *testing.T) {
	s := newTestServer()
	if s.ValidateAccessToken("") {
		t.Fatal("ValidateAccessToken(\"\") = true, want false")
	}
	if s.ValidateAccessToken("never-issued") {
		t.Fatal("ValidateAccessToken(unknown) = true, want false")
	}
}

func TestValidateAccessToken_ExpiredTokenRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	verifier, challenge := fixedPKCEPair()
	code := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)
	_, body := exchangeCode(t, s, code, redirectURI, clientID, verifier)
	access := body["access_token"].(string)

	// Backdate it directly — the deterministic alternative to sleeping past
	// the real 1h TTL.
	s.mu.Lock()
	s.accessTokens[access].ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()

	if s.ValidateAccessToken(access) {
		t.Fatal("ValidateAccessToken(expired) = true, want false")
	}
}

func TestHandleRegister_RequiresNonEmptyRedirectURIs(t *testing.T) {
	s := newTestServer()
	body, _ := json.Marshal(registerRequest{ClientName: "x"})
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRegister_RejectsNonAbsoluteRedirectURI(t *testing.T) {
	s := newTestServer()
	body, _ := json.Marshal(registerRequest{ClientName: "x", RedirectURIs: []string{"/just/a/path"}})
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthorize_WrongBearerTokenRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	_, challenge := fixedPKCEPair()

	q := authorizeParamsValues(clientID, redirectURI, "s", challenge)
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	s.HandleAuthorize(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthorize_NoHeaderRendersLoginFormWithEscapedParams(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	_, challenge := fixedPKCEPair()

	// An attacker-controlled state value crafted to break out of the hidden
	// input's value="" attribute if the template didn't escape it.
	maliciousState := `"><script>alert(1)</script>`
	q := authorizeParamsValues(clientID, redirectURI, maliciousState, challenge)
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorize(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (login form), body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	html := rec.Body.String()
	if !strings.Contains(html, `name="token"`) {
		t.Fatalf("login form missing the token input: %s", html)
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("login form reflects an unescaped <script> tag from state — XSS via html/template auto-escaping failure")
	}
}

func TestHandleAuthorize_FormPostWithCorrectTokenIssuesCode(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	_, challenge := fixedPKCEPair()

	form := authorizeParamsValues(clientID, redirectURI, "s", challenge)
	form.Set("token", testToken)
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleAuthorize(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302, body = %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("code") == "" {
		t.Fatalf("Location %q carries no code", loc)
	}
}

func TestHandleAuthorize_FormPostWithWrongTokenReRendersForm(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	_, challenge := fixedPKCEPair()

	form := authorizeParamsValues(clientID, redirectURI, "s", challenge)
	form.Set("token", "wrong-token")
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.HandleAuthorize(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered form), body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Incorrect token") {
		t.Fatalf("re-rendered form missing the error message: %s", rec.Body.String())
	}
}

func TestHandleAuthorize_UnknownClientRejected(t *testing.T) {
	s := newTestServer()
	_, challenge := fixedPKCEPair()
	q := authorizeParamsValues("no-such-client", "http://cb.example/callback", "s", challenge)
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorize(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthorize_RedirectURIMustMatchExactly(t *testing.T) {
	s := newTestServer()
	clientID := registerClient(t, s, "http://cb.example/callback")
	_, challenge := fixedPKCEPair()

	for _, badURI := range []string{
		"http://cb.example/callback/",    // trailing slash
		"http://cb.example/callback?x=1", // extra query
		"http://evil.example/callback",   // different host entirely
	} {
		q := authorizeParamsValues(clientID, badURI, "s", challenge)
		req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
		rec := httptest.NewRecorder()
		s.HandleAuthorize(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("redirect_uri=%q: status = %d, want 400 — OAuth 2.1 requires EXACT redirect_uri matching", badURI, rec.Code)
		}
	}
}

func TestHandleAuthorize_NonCodeResponseTypeRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	_, challenge := fixedPKCEPair()

	q := authorizeParamsValues(clientID, redirectURI, "s", challenge)
	q.Set("response_type", "token") // the implicit grant OAuth 2.1 removed
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	s.HandleAuthorize(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthorize_PlainOrMissingCodeChallengeMethodRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	_, challenge := fixedPKCEPair()

	for _, method := range []string{"plain", "", "S1"} {
		q := authorizeParamsValues(clientID, redirectURI, "s", challenge)
		q.Set("code_challenge_method", method)
		req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
		rec := httptest.NewRecorder()
		s.HandleAuthorize(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code_challenge_method=%q: status = %d, want 400 — OAuth 2.1 requires S256 only", method, rec.Code)
		}
	}
}

func TestHandleToken_WrongVerifierRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	_, challenge := fixedPKCEPair()
	code := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)

	rec, body := exchangeCode(t, s, code, redirectURI, clientID, "not-the-real-verifier")
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("status/error = %d/%v, want 400/invalid_grant", rec.Code, body)
	}
}

func TestHandleToken_CodeIsSingleUse(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	verifier, challenge := fixedPKCEPair()
	code := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)

	rec1, _ := exchangeCode(t, s, code, redirectURI, clientID, verifier)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first exchange status = %d, want 200", rec1.Code)
	}
	rec2, body2 := exchangeCode(t, s, code, redirectURI, clientID, verifier)
	if rec2.Code != http.StatusBadRequest || body2["error"] != "invalid_grant" {
		t.Fatalf("replayed code: status/error = %d/%v, want 400/invalid_grant", rec2.Code, body2)
	}
}

func TestHandleToken_ExpiredCodeRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	verifier, challenge := fixedPKCEPair()
	code := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)

	s.mu.Lock()
	s.codes[code].ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()

	rec, body := exchangeCode(t, s, code, redirectURI, clientID, verifier)
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("expired code: status/error = %d/%v, want 400/invalid_grant", rec.Code, body)
	}
}

func TestHandleToken_MismatchedClientOrRedirectRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	otherClientID := registerClient(t, s, "http://other.example/cb")
	verifier, challenge := fixedPKCEPair()

	code1 := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)
	rec, body := exchangeCode(t, s, code1, redirectURI, otherClientID, verifier)
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("wrong client_id: status/error = %d/%v, want 400/invalid_grant", rec.Code, body)
	}

	code2 := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)
	rec2, body2 := exchangeCode(t, s, code2, "http://other.example/cb", clientID, verifier)
	if rec2.Code != http.StatusBadRequest || body2["error"] != "invalid_grant" {
		t.Fatalf("wrong redirect_uri: status/error = %d/%v, want 400/invalid_grant", rec2.Code, body2)
	}
}

func TestHandleToken_MissingClientIDOrRedirectURIRejected(t *testing.T) {
	s := newTestServer()
	const redirectURI = "http://cb.example/callback"
	clientID := registerClient(t, s, redirectURI)
	verifier, challenge := fixedPKCEPair()

	code1 := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)
	rec, body := tokenRequest(t, s, url.Values{
		"grant_type": {"authorization_code"}, "code": {code1},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier}, // client_id omitted
	})
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("missing client_id: status/error = %d/%v, want 400/invalid_request", rec.Code, body)
	}

	code2 := authorizeWithBearer(t, s, clientID, redirectURI, "s", challenge)
	rec2, body2 := tokenRequest(t, s, url.Values{
		"grant_type": {"authorization_code"}, "code": {code2},
		"client_id": {clientID}, "code_verifier": {verifier}, // redirect_uri omitted
	})
	if rec2.Code != http.StatusBadRequest || body2["error"] != "invalid_request" {
		t.Fatalf("missing redirect_uri: status/error = %d/%v, want 400/invalid_request", rec2.Code, body2)
	}
}

func TestHandleToken_UnsupportedOrMissingGrantTypeRejected(t *testing.T) {
	s := newTestServer()
	rec, body := tokenRequest(t, s, url.Values{"grant_type": {"password"}})
	if rec.Code != http.StatusBadRequest || body["error"] != "unsupported_grant_type" {
		t.Fatalf("grant_type=password: status/error = %d/%v, want 400/unsupported_grant_type", rec.Code, body)
	}
	rec2, body2 := tokenRequest(t, s, url.Values{})
	if rec2.Code != http.StatusBadRequest || body2["error"] != "invalid_request" {
		t.Fatalf("missing grant_type: status/error = %d/%v, want 400/invalid_request", rec2.Code, body2)
	}
}

func TestHandleToken_UnknownRefreshTokenRejected(t *testing.T) {
	s := newTestServer()
	rec, body := tokenRequest(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"never-issued"}})
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("status/error = %d/%v, want 400/invalid_grant", rec.Code, body)
	}
}

func TestMetadata_AuthorizationServer(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	s.HandleAuthServerMetadata(rec, req)

	var meta map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decoding metadata: %v", err)
	}
	if meta["issuer"] != s.Issuer {
		t.Fatalf("issuer = %v, want %q", meta["issuer"], s.Issuer)
	}
	if meta["authorization_endpoint"] != s.Issuer+"/oauth/authorize" {
		t.Fatalf("authorization_endpoint = %v", meta["authorization_endpoint"])
	}
	if meta["token_endpoint"] != s.Issuer+"/oauth/token" {
		t.Fatalf("token_endpoint = %v", meta["token_endpoint"])
	}
	if meta["registration_endpoint"] != s.Issuer+"/oauth/register" {
		t.Fatalf("registration_endpoint = %v", meta["registration_endpoint"])
	}
	methods, _ := meta["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Fatalf("code_challenge_methods_supported = %v, want [S256] only", methods)
	}
}

func TestMetadata_ProtectedResource(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	rec := httptest.NewRecorder()
	s.HandleProtectedResourceMetadata(rec, req)

	var meta map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decoding metadata: %v", err)
	}
	if meta["resource"] != s.Issuer+"/mcp" {
		t.Fatalf("resource = %v, want %q", meta["resource"], s.Issuer+"/mcp")
	}
	servers, _ := meta["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != s.Issuer {
		t.Fatalf("authorization_servers = %v, want [%q]", servers, s.Issuer)
	}
}

func TestVerifyPKCE(t *testing.T) {
	verifier, challenge := fixedPKCEPair()
	if !verifyPKCE(verifier, challenge) {
		t.Fatal("verifyPKCE(matching pair) = false, want true")
	}
	if verifyPKCE("wrong-verifier", challenge) {
		t.Fatal("verifyPKCE(wrong verifier) = true, want false")
	}
	if verifyPKCE(verifier, verifier) {
		t.Fatal(`verifyPKCE must not accept the "plain" method (challenge == verifier)`)
	}
}
