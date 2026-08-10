// Package oauth implements a minimal, single-tenant OAuth 2.1 authorization
// server for pkg/httpapi — closing the gap README.md documents: a
// spec-compliant MCP client that only speaks the OAuth 2.1 flow the MCP spec
// defines for remote HTTP servers gets stuck retrying discovery against a
// bare 401 today, since every route (including /mcp) is gated by nothing but
// a single static bearer token compared in constant time.
//
// This is deliberately NOT general-purpose multi-user OAuth — goflow has no
// concept of accounts (see README's "Explicitly NOT in v1": no per-user
// auth). There is exactly one principal, whoever knows GOFLOW_API_TOKEN — the
// same secret that already gates every other route — and /oauth/authorize
// grants an authorization code to that principal and no one else: it
// requires the token presented either as a Bearer header (a machine/CLI
// client that already knows the token — e.g. the MCP Inspector's --header
// flag) or, for a real browser redirect that can't set a header, typed into
// a one-field HTML form served at the same URL. There is no separate
// login/account system behind that form; it checks the exact same token the
// rest of this server already trusts, via the same constant-time compare.
// Auto-approving every /authorize request with no credential check at all
// was considered and rejected: it would let anyone who can merely reach the
// server complete the OAuth dance and mint a fully valid access token
// without ever knowing GOFLOW_API_TOKEN — strictly weaker than today, not a
// neutral addition.
//
// What IS real: RFC 8414 authorization-server metadata, RFC 9728
// protected-resource metadata, RFC 7591 dynamic client registration,
// mandatory PKCE (S256 only — OAuth 2.1 drops "plain") on every
// authorization code, and short-lived, single-use authorization codes
// exchanged for opaque, expiring access/refresh tokens (refresh rotates on
// every use). A client that speaks this flow, rather than a static header,
// can now authenticate against goflow instead of getting stuck.
//
// Everything here is stdlib only — crypto/rand, crypto/sha256,
// crypto/subtle, encoding/base64, encoding/json, html/template, net/http,
// net/url, sync, time — matching the rest of this project's zero-new-
// dependency stance. All server-side state (clients, codes, tokens) is
// in-memory only and does not survive a restart, the same posture as
// piece.MemoryStore: this is a v1 default, not a promise of durability —
// see pkg/credentials and pkg/flowstore for how this project handles state
// that DOES need to survive a restart, if that turns out to matter here too.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	authCodeTTL     = 60 * time.Second
	accessTokenTTL  = 1 * time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
)

// Client is a dynamically registered OAuth client (RFC 7591). Public client
// only — no client_secret is ever issued. OAuth 2.1 treats a client unable
// to keep a secret confidential as public; every realistic MCP client this
// serves (a CLI tool, a desktop app, an editor extension — no server-side
// backend of its own) fits that description, so PKCE is the only client
// authentication mechanism, never a secret.
type Client struct {
	ID           string
	RedirectURIs []string
	CreatedAt    time.Time
}

type authCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string // base64url(SHA256(code_verifier)) — S256 only, see verifyPKCE
	ExpiresAt     time.Time
	Used          bool
}

type accessToken struct {
	ClientID  string
	ExpiresAt time.Time
}

type refreshToken struct {
	ClientID  string
	ExpiresAt time.Time
}

// Server issues and validates OAuth 2.1 credentials for one goflow
// deployment. Token is the same GOFLOW_API_TOKEN every other httpapi route
// already trusts — the one secret /oauth/authorize checks before granting a
// code. Issuer is this server's own externally-reachable base URL (e.g.
// "https://goflow.example.com" or "http://127.0.0.1:8080" for local/direct
// use), required to build the absolute endpoint URLs RFC 8414/9728 metadata
// advertise and to satisfy their "issuer" field.
type Server struct {
	Token  string
	Issuer string

	mu            sync.Mutex
	clients       map[string]*Client
	codes         map[string]*authCode
	accessTokens  map[string]*accessToken
	refreshTokens map[string]*refreshToken
}

// NewServer returns a Server for the given static token and issuer base URL
// (a trailing slash, if any, is trimmed so every built URL is unambiguous).
func NewServer(token, issuer string) *Server {
	return &Server{
		Token:         token,
		Issuer:        strings.TrimRight(issuer, "/"),
		clients:       map[string]*Client{},
		codes:         map[string]*authCode{},
		accessTokens:  map[string]*accessToken{},
		refreshTokens: map[string]*refreshToken{},
	}
}

// ValidateAccessToken reports whether tok is a live (issued, unexpired)
// access token. httpapi's auth middleware calls this as a fallback when a
// request's bearer token doesn't match the static GOFLOW_API_TOKEN, so
// either credential form grants the same access — this project has no
// scopes/permissions concept (see piece.Registry's doc comment on the
// absence of per-piece access control), so there is nothing narrower an
// OAuth-issued token could usefully be limited to yet.
//
// Not constant-time on purpose, unlike the static-token compare: an access
// token is a 256-bit random value drawn fresh per grant, not a long-lived
// operator-chosen secret, so a timing side channel on which prefix matched
// leaks nothing practically exploitable — the same reasoning any real OAuth
// resource server relies on for a bearer-token database lookup.
func (s *Server) ValidateAccessToken(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.accessTokens[tok]
	if !ok {
		return false
	}
	return time.Now().Before(at.ExpiresAt)
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: generating random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// verifyPKCE reports whether verifier is the code_verifier that produced
// challenge — base64url(SHA256(verifier)), no padding, the S256 method
// OAuth 2.1 requires (the older "plain" method, where challenge ==
// verifier, is never accepted here — see handleAuthorize).
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return computed == challenge
}

// --- RFC 8414 / RFC 9728 metadata -------------------------------------------

// HandleAuthServerMetadata serves GET /.well-known/oauth-authorization-server
// (RFC 8414). token_endpoint_auth_methods_supported is ["none"] — there is no
// client secret to authenticate with (see Client's doc comment); PKCE is the
// only client-side protection, hence code_challenge_methods_supported is
// ["S256"] only.
func (s *Server) HandleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.Issuer,
		"authorization_endpoint":                s.Issuer + "/oauth/authorize",
		"token_endpoint":                        s.Issuer + "/oauth/token",
		"registration_endpoint":                 s.Issuer + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// HandleProtectedResourceMetadata serves GET
// /.well-known/oauth-protected-resource (RFC 9728) — tells a client which
// authorization server(s) can mint a token /mcp accepts. goflow has exactly
// one resource (/mcp) and one authorization server (this one).
func (s *Server) HandleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              s.Issuer + "/mcp",
		"authorization_servers": []string{s.Issuer},
	})
}

// --- RFC 7591 dynamic client registration -----------------------------------

type registerRequest struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// HandleRegister serves POST /oauth/register. Left unauthenticated on
// purpose, matching typical open dynamic-client-registration deployments:
// registering a client identifies it (for redirect_uri matching) but grants
// no access by itself — completing /oauth/authorize still requires knowing
// GOFLOW_API_TOKEN, so an attacker registering throwaway clients gains
// nothing.
func (s *Server) HandleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required and must be non-empty")
		return
	}
	for _, ru := range req.RedirectURIs {
		u, err := url.Parse(ru)
		if err != nil || u.Scheme == "" || u.Host == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("redirect_uri %q must be an absolute URI", ru))
			return
		}
	}

	id, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	client := &Client{ID: id, RedirectURIs: req.RedirectURIs, CreatedAt: time.Now()}

	s.mu.Lock()
	s.clients[client.ID] = client
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// --- authorization endpoint --------------------------------------------------

type authorizeParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

func parseAuthorizeParams(values url.Values) authorizeParams {
	return authorizeParams{
		ResponseType:        values.Get("response_type"),
		ClientID:            values.Get("client_id"),
		RedirectURI:         values.Get("redirect_uri"),
		State:               values.Get("state"),
		CodeChallenge:       values.Get("code_challenge"),
		CodeChallengeMethod: values.Get("code_challenge_method"),
	}
}

var loginFormTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>goflow — Authorize</title></head>
<body style="font-family:sans-serif;max-width:28rem;margin:4rem auto">
<h1>Authorize MCP client</h1>
<p>A client is requesting access to this goflow server. Enter the server's
API token to approve.</p>
{{if .Error}}<p style="color:#b00020">{{.Error}}</p>{{end}}
<form method="POST" action="/oauth/authorize">
<input type="hidden" name="response_type" value="{{.P.ResponseType}}">
<input type="hidden" name="client_id" value="{{.P.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.P.RedirectURI}}">
<input type="hidden" name="state" value="{{.P.State}}">
<input type="hidden" name="code_challenge" value="{{.P.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="{{.P.CodeChallengeMethod}}">
<p><label>API token<br><input type="password" name="token" autofocus style="width:100%"></label></p>
<button type="submit">Authorize</button>
</form>
</body></html>`))

// HandleAuthorize serves both GET and POST /oauth/authorize. GET is the
// entry point a client's browser redirect (or a machine client bypassing the
// browser via a Bearer header) hits first; POST is where the login form
// above submits back to. Both paths funnel into the same validation and
// issuance logic below — only how the token is supplied differs.
func (s *Server) HandleAuthorize(w http.ResponseWriter, r *http.Request) {
	var values url.Values
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form body", http.StatusBadRequest)
			return
		}
		values = r.Form
	} else {
		values = r.URL.Query()
	}
	p := parseAuthorizeParams(values)

	client, redirectErr := s.validateAuthorizeRequest(p)
	if redirectErr != "" {
		// client_id/redirect_uri/PKCE shape problems can't safely redirect
		// (the redirect_uri itself may be unverified) — a plain error page,
		// same posture as activepieces has no equivalent of since it has no
		// OAuth server at all; this is new ground for this project, kept as
		// simple and honest as the rest of it.
		http.Error(w, redirectErr, http.StatusBadRequest)
		return
	}

	// Bearer header path — a machine/CLI client that already knows the
	// token (works for both GET, e.g. curl/Inspector --header, and POST).
	if authorized, checked := s.checkBearerHeader(r); checked {
		if !authorized {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		s.finishAuthorize(w, r, client, p)
		return
	}

	if r.Method == http.MethodPost {
		submitted := r.FormValue("token")
		if subtle.ConstantTimeCompare([]byte(submitted), []byte(s.Token)) != 1 {
			renderLoginForm(w, p, "Incorrect token — try again.")
			return
		}
		s.finishAuthorize(w, r, client, p)
		return
	}

	// GET, no bearer header: render the one-field form described in this
	// package's doc comment.
	renderLoginForm(w, p, "")
}

// checkBearerHeader reports (authorized, checked) — checked is false when
// there was no Authorization header at all (so the caller falls through to
// the form-based path), true otherwise (so an explicitly wrong header is
// rejected outright rather than silently falling back to the form).
func (s *Server) checkBearerHeader(r *http.Request) (authorized bool, checked bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return false, false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false, true
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1, true
}

func renderLoginForm(w http.ResponseWriter, p authorizeParams, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginFormTmpl.Execute(w, struct {
		P     authorizeParams
		Error string
	}{P: p, Error: errMsg})
}

// validateAuthorizeRequest checks everything that must be verified BEFORE
// it's safe to redirect anywhere: response_type, a known client_id, an
// EXACT (not prefix) match on redirect_uri against that client's registered
// list, and code_challenge_method == "S256" with a non-empty code_challenge.
// OAuth 2.1 requires exact redirect_uri matching precisely to prevent an
// attacker registering a broader URI and redirecting a real code/token to
// their own endpoint.
func (s *Server) validateAuthorizeRequest(p authorizeParams) (*Client, string) {
	if p.ClientID == "" {
		return nil, "client_id is required"
	}
	s.mu.Lock()
	client, ok := s.clients[p.ClientID]
	s.mu.Unlock()
	if !ok {
		return nil, "unknown client_id"
	}
	if p.RedirectURI == "" {
		return nil, "redirect_uri is required"
	}
	matched := false
	for _, ru := range client.RedirectURIs {
		if ru == p.RedirectURI {
			matched = true
			break
		}
	}
	if !matched {
		return nil, "redirect_uri does not match any URI registered for this client"
	}
	if p.ResponseType != "code" {
		return nil, `response_type must be "code" — OAuth 2.1 does not support the implicit grant`
	}
	if p.CodeChallengeMethod != "S256" {
		return nil, `code_challenge_method must be "S256" — OAuth 2.1 requires PKCE and drops the "plain" method`
	}
	if p.CodeChallenge == "" {
		return nil, "code_challenge is required"
	}
	return client, ""
}

// finishAuthorize issues a single-use authorization code and redirects to
// the caller's redirect_uri with ?code=...&state=....
func (s *Server) finishAuthorize(w http.ResponseWriter, r *http.Request, client *Client, p authorizeParams) {
	code, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.codes[code] = &authCode{
		ClientID:      client.ID,
		RedirectURI:   p.RedirectURI,
		CodeChallenge: p.CodeChallenge,
		ExpiresAt:     time.Now().Add(authCodeTTL),
	}
	s.mu.Unlock()

	dest, _ := url.Parse(p.RedirectURI) // already validated by validateAuthorizeRequest
	q := dest.Query()
	q.Set("code", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	dest.RawQuery = q.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// --- token endpoint -----------------------------------------------------------

// HandleToken serves POST /oauth/token (application/x-www-form-urlencoded,
// per RFC 6749 §4.1.3/§6). Both supported grant types (authorization_code,
// refresh_token) return the same {access_token, token_type, expires_in,
// refresh_token} shape; every rejection is {error, error_description} with
// the RFC 6749 §5.2 error codes, HTTP 400.
func (s *Server) HandleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}

	switch r.FormValue("grant_type") {
	case "authorization_code":
		s.handleAuthCodeGrant(w, r)
	case "refresh_token":
		s.handleRefreshGrant(w, r)
	case "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

func (s *Server) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	redirectURI := r.FormValue("redirect_uri")
	clientID := r.FormValue("client_id")
	verifier := r.FormValue("code_verifier")

	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}

	s.mu.Lock()
	ac, ok := s.codes[code]
	var alreadyUsed bool
	if ok {
		alreadyUsed = ac.Used
		// Burn the code on this first exchange attempt regardless of what
		// the checks below decide — a code must not stay retryable just
		// because THIS attempt fails PKCE or client/redirect matching,
		// otherwise it's still available for another guess.
		ac.Used = true
	}
	s.mu.Unlock()

	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
		return
	}
	if alreadyUsed {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code has already been used")
		return
	}
	if time.Now().After(ac.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code has expired")
		return
	}
	// client_id is required here, not just checked-if-present: a public
	// client (no secret) has no other way to authenticate itself at the
	// token endpoint, so OAuth 2.1 requires it explicitly rather than
	// letting the code alone stand in for client identity.
	if clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	if clientID != ac.ClientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match the code's client")
		return
	}
	// Required, not just checked-if-present: RFC 6749 §4.1.3 requires
	// redirect_uri here whenever it was included in the /authorize request,
	// and validateAuthorizeRequest never lets /authorize succeed without one.
	if redirectURI == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri is required")
		return
	}
	if redirectURI != ac.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the one used to obtain the code")
		return
	}
	if !verifyPKCE(verifier, ac.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match code_challenge")
		return
	}

	s.issueTokens(w, ac.ClientID)
}

func (s *Server) handleRefreshGrant(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("refresh_token")
	if token == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}

	s.mu.Lock()
	rt, ok := s.refreshTokens[token]
	if ok {
		delete(s.refreshTokens, token) // rotation: this refresh token is single-use
	}
	s.mu.Unlock()

	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	if time.Now().After(rt.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token has expired")
		return
	}

	s.issueTokens(w, rt.ClientID)
}

// issueTokens mints a fresh access token and a fresh (rotated) refresh
// token for clientID and writes the RFC 6749 §5.1 success response.
func (s *Server) issueTokens(w http.ResponseWriter, clientID string) {
	access, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	refresh, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	now := time.Now()
	s.mu.Lock()
	s.accessTokens[access] = &accessToken{ClientID: clientID, ExpiresAt: now.Add(accessTokenTTL)}
	s.refreshTokens[refresh] = &refreshToken{ClientID: clientID, ExpiresAt: now.Add(refreshTokenTTL)}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
	})
}

// --- wire helpers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeOAuthError writes the RFC 6749 §5.2 error shape: {"error":
// "<code>", "error_description": "<msg>"}.
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
