// Package oauth is a minimal, storage-free OAuth 2.1 authorization server that lets each
// ChatGPT user connect with their own Odoo account.
//
// Users sign in once with their Odoo login and API key. Every artifact the server hands
// out (client IDs, authorization codes, access and refresh tokens) is an AES-GCM sealed
// blob carrying what the server needs to know, so nothing is stored server-side.
// Revoking the API key in Odoo cuts access at the next token refresh (at most AccessTTL).
package oauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/rizgust/odoo-gpt-mcp/internal/odoo"
)

const (
	CodeTTL    = 5 * time.Minute
	AccessTTL  = time.Hour
	RefreshTTL = 90 * 24 * time.Hour // sliding: every refresh issues a new 90-day token
)

// Redirect hosts accepted for client IDs that are neither registered nor metadata documents,
// e.g. ChatGPT configured with a manually entered client ID.
var knownRedirectHosts = []string{"chatgpt.com", "chat.openai.com", "claude.ai", "claude.com"}

// Identity is the Odoo account a token acts as.
type Identity struct {
	Login  string
	APIKey string
	UID    int
}

// Authenticator checks Odoo credentials; it returns the user ID of an internal (non-portal) user.
type Authenticator func(ctx context.Context, login, apiKey string) (int, error)

type Server struct {
	publicURL    string
	aead         cipher.AEAD
	authenticate Authenticator
	now          func() time.Time

	// fetchDoc retrieves client ID metadata documents; overridable for tests.
	fetchDoc func(ctx context.Context, url string) ([]byte, error)

	mu        sync.Mutex
	usedCodes map[string]time.Time
	docs      map[string]clientDoc
}

// New creates the authorization server. secret should be at least 32 random bytes;
// changing it signs everyone out.
func New(publicURL, secret string, authenticate Authenticator) (*Server, error) {
	if len(secret) < 32 {
		return nil, errors.New("OAUTH_SECRET must be at least 32 characters")
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Server{
		publicURL:    strings.TrimRight(publicURL, "/"),
		aead:         aead,
		authenticate: authenticate,
		now:          time.Now,
		fetchDoc:     fetchDocument,
		usedCodes:    map[string]time.Time{},
		docs:         map[string]clientDoc{},
	}, nil
}

// OdooAuthenticator verifies credentials against Odoo and rejects portal users.
func OdooAuthenticator(odooURL, db string, timeout time.Duration) Authenticator {
	return func(ctx context.Context, login, apiKey string) (int, error) {
		c := odoo.NewClient(odooURL, db, login, apiKey, timeout)
		uid, err := c.UID(ctx)
		if err != nil {
			return 0, err
		}
		var users []struct {
			Share bool `json:"share"`
		}
		if err := c.Execute(ctx, "res.users", "read", []any{[]int{uid}}, map[string]any{"fields": []string{"share"}}, &users); err != nil {
			return 0, err
		}
		if len(users) == 0 || users[0].Share {
			return 0, errors.New("only internal Odoo users can connect")
		}
		return uid, nil
	}
}

// ---- sealed payloads

type payload struct {
	Kind      string   `json:"k"` // client | code | access | refresh
	Exp       int64    `json:"e,omitempty"`
	Login     string   `json:"l,omitempty"`
	APIKey    string   `json:"a,omitempty"`
	UID       int      `json:"u,omitempty"`
	ClientID  string   `json:"c,omitempty"`
	Redirect  string   `json:"r,omitempty"`
	Challenge string   `json:"p,omitempty"`
	Redirects []string `json:"rs,omitempty"`
}

func (s *Server) seal(p payload) string {
	plain, _ := json.Marshal(p)
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, []byte(p.Kind)))
}

func (s *Server) open(token, kind string) (*payload, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return nil, false
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(kind)) // kind as associated data: no token type confusion
	if err != nil {
		return nil, false
	}
	var p payload
	if json.Unmarshal(plain, &p) != nil || p.Kind != kind {
		return nil, false
	}
	if p.Exp != 0 && s.now().Unix() > p.Exp {
		return nil, false
	}
	return &p, true
}

// ---- resource side

// Verifier validates access tokens on the MCP endpoint.
func (s *Server) Verifier() auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		p, ok := s.open(token, "access")
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{
			UserID:     strconv.Itoa(p.UID),
			Expiration: time.Unix(p.Exp, 0),
			Extra:      map[string]any{"identity": Identity{Login: p.Login, APIKey: p.APIKey, UID: p.UID}},
		}, nil
	}
}

// IdentityFrom returns the Odoo identity of an authenticated MCP request.
func IdentityFrom(r *http.Request) (Identity, bool) {
	info := auth.TokenInfoFromContext(r.Context())
	if info == nil {
		return Identity{}, false
	}
	id, ok := info.Extra["identity"].(Identity)
	return id, ok
}

// ResourceMetadataURL is advertised in 401 responses so clients can discover how to sign in.
func (s *Server) ResourceMetadataURL() string {
	return s.publicURL + "/.well-known/oauth-protected-resource"
}

// ---- HTTP endpoints

// Register mounts the discovery, registration, authorization and token endpoints.
func (s *Server) Register(mux *http.ServeMux, mcpPath string) {
	resource := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.publicURL + mcpPath,
		AuthorizationServers:   []string{s.publicURL},
		BearerMethodsSupported: []string{"header"},
	})
	mux.Handle("/.well-known/oauth-protected-resource", resource)
	mux.Handle("/.well-known/oauth-protected-resource"+mcpPath, resource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.metadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.metadata)
	mux.HandleFunc("POST /register", s.register)
	mux.HandleFunc("GET /authorize", s.authorizePage)
	mux.HandleFunc("POST /authorize", s.authorizeSubmit)
	mux.HandleFunc("POST /token", s.token)
}

func (s *Server) metadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.publicURL,
		"authorization_endpoint":                s.publicURL + "/authorize",
		"token_endpoint":                        s.publicURL + "/token",
		"registration_endpoint":                 s.publicURL + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"odoo", "offline_access"},
		// Client IDs may be metadata document URLs (Claude's and Claude Code's published identity).
		"client_id_metadata_document_supported": true,
	})
}

// register implements RFC 7591 dynamic client registration. The client ID is a sealed list
// of the client's redirect URIs, so later requests can be checked without storage.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be https (or http on localhost): "+u)
			return
		}
	}
	clientID := s.seal(payload{Kind: "client", Redirects: req.RedirectURIs})
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        s.now().Unix(),
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1"))
}

// redirectAllowed checks redirectURI against a registered client, or the known ChatGPT
// hosts for clients configured manually with an arbitrary client ID.
func (s *Server) redirectAllowed(ctx context.Context, clientID, redirectURI string) bool {
	if !validRedirect(redirectURI) {
		return false
	}
	if p, ok := s.open(clientID, "client"); ok {
		return redirectMatches(p.Redirects, redirectURI)
	}
	if redirects, ok := s.documentRedirects(ctx, clientID); ok {
		return redirectMatches(redirects, redirectURI)
	}
	u, _ := url.Parse(redirectURI)
	return u.Scheme == "https" && slices.Contains(knownRedirectHosts, u.Hostname())
}

type authRequest struct {
	ClientID, RedirectURI, State, Challenge string
}

func parseAuthRequest(v url.Values) (authRequest, string) {
	ar := authRequest{
		ClientID:    v.Get("client_id"),
		RedirectURI: v.Get("redirect_uri"),
		State:       v.Get("state"),
		Challenge:   v.Get("code_challenge"),
	}
	switch {
	case ar.ClientID == "" || ar.RedirectURI == "":
		return ar, "client_id and redirect_uri are required"
	case v.Get("response_type") != "code":
		return ar, "response_type must be code"
	case ar.Challenge == "" || v.Get("code_challenge_method") != "S256":
		return ar, "PKCE with code_challenge_method=S256 is required"
	}
	return ar, ""
}

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	ar, problem := parseAuthRequest(r.URL.Query())
	if problem == "" && !s.redirectAllowed(r.Context(), ar.ClientID, ar.RedirectURI) {
		problem = "redirect_uri is not registered for this client"
	}
	if problem != "" {
		renderError(w, problem)
		return
	}
	renderLogin(w, ar, "", "")
}

func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		renderError(w, "invalid form")
		return
	}
	ar, problem := parseAuthRequest(r.PostForm)
	if problem == "" && !s.redirectAllowed(r.Context(), ar.ClientID, ar.RedirectURI) {
		problem = "redirect_uri is not registered for this client"
	}
	if problem != "" {
		renderError(w, problem)
		return
	}
	login := strings.TrimSpace(r.PostForm.Get("login"))
	apiKey := strings.TrimSpace(r.PostForm.Get("api_key"))
	if login == "" || apiKey == "" {
		renderLogin(w, ar, login, "Enter your Odoo email and API key.")
		return
	}
	uid, err := s.authenticate(r.Context(), login, apiKey)
	if err != nil {
		renderLogin(w, ar, login, signInError(err))
		return
	}
	code := s.seal(payload{
		Kind: "code", Exp: s.now().Add(CodeTTL).Unix(),
		Login: login, APIKey: apiKey, UID: uid,
		ClientID: ar.ClientID, Redirect: ar.RedirectURI, Challenge: ar.Challenge,
	})
	target, _ := url.Parse(ar.RedirectURI)
	q := target.Query()
	q.Set("code", code)
	if ar.State != "" {
		q.Set("state", ar.State)
	}
	q.Set("iss", s.publicURL)
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func signInError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "authentication failed"):
		return "Odoo didn't accept that email and API key. Check both and try again."
	case strings.Contains(msg, "only internal"):
		return "Only internal Odoo users can connect."
	case strings.Contains(msg, "cannot reach"):
		return "Odoo can't be reached right now. Try again in a moment."
	}
	return "Sign-in failed: " + msg
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "refresh_token":
		s.refresh(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	code := f.Get("code")
	p, ok := s.open(code, "code")
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if f.Get("client_id") != "" && f.Get("client_id") != p.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if f.Get("redirect_uri") != p.Redirect {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(p.Challenge)) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	if !s.markCodeUsed(code, time.Unix(p.Exp, 0)) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code already used")
		return
	}
	s.issueTokens(w, Identity{Login: p.Login, APIKey: p.APIKey, UID: p.UID})
}

// markCodeUsed enforces single use of authorization codes (in memory; codes live 5 minutes).
func (s *Server) markCodeUsed(code string, exp time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for c, e := range s.usedCodes {
		if now.After(e) {
			delete(s.usedCodes, c)
		}
	}
	if _, used := s.usedCodes[code]; used {
		return false
	}
	s.usedCodes[code] = exp
	return true
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	p, ok := s.open(r.PostForm.Get("refresh_token"), "refresh")
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
		return
	}
	// Re-check with Odoo so a revoked API key or deactivated user stops working.
	uid, err := s.authenticate(r.Context(), p.Login, p.APIKey)
	if err != nil {
		if strings.Contains(err.Error(), "cannot reach") {
			oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "Odoo is unreachable")
			return
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Odoo access was revoked; sign in again")
		return
	}
	s.issueTokens(w, Identity{Login: p.Login, APIKey: p.APIKey, UID: uid})
}

func (s *Server) issueTokens(w http.ResponseWriter, id Identity) {
	now := s.now()
	access := s.seal(payload{Kind: "access", Exp: now.Add(AccessTTL).Unix(), Login: id.Login, APIKey: id.APIKey, UID: id.UID})
	refresh := s.seal(payload{Kind: "refresh", Exp: now.Add(RefreshTTL).Unix(), Login: id.Login, APIKey: id.APIKey, UID: id.UID})
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(AccessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         "odoo offline_access",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
