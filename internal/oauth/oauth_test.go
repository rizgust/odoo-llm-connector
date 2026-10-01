package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	publicURL   = "https://odoo.mcp.example.com"
	redirectURI = "https://chatgpt.com/connector_platform_oauth_redirect"
	verifier    = "a-sufficiently-long-pkce-code-verifier-0123456789"
)

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type env struct {
	t       *testing.T
	srv     *Server
	mux     *http.ServeMux
	now     time.Time
	revoked bool
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	srv, err := New(publicURL, strings.Repeat("s", 32), func(_ context.Context, login, key string) (int, error) {
		if e.revoked || login != "ana@nuanu.com" || key != "good-key" {
			return 0, errors.New("odoo authentication failed: check ODOO_DB, ODOO_USER and ODOO_API_KEY")
		}
		return 42, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return e.now }
	e.srv, e.mux = srv, http.NewServeMux()
	srv.Register(e.mux, "/mcp")
	return e
}

func (e *env) do(method, target string, body url.Values, jsonBody string) *httptest.ResponseRecorder {
	var req *http.Request
	switch {
	case jsonBody != "":
		req = httptest.NewRequest(method, target, strings.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
	case body != nil:
		req = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	default:
		req = httptest.NewRequest(method, target, nil)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body)
	}
	return out
}

func (e *env) registerClient() string {
	rec := e.do("POST", "/register", nil, `{"client_name":"ChatGPT","redirect_uris":["`+redirectURI+`"]}`)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("register = %d %s", rec.Code, rec.Body)
	}
	return decode(e.t, rec)["client_id"].(string)
}

func authForm(clientID, redirect string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"xyz"},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
	}
}

// signIn runs the browser part of the flow and returns the authorization code.
func (e *env) signIn(clientID, login, key string) (string, *httptest.ResponseRecorder) {
	form := authForm(clientID, redirectURI)
	form.Set("login", login)
	form.Set("api_key", key)
	rec := e.do("POST", "/authorize", form, "")
	if rec.Code != http.StatusFound {
		return "", rec
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Scheme+"://"+loc.Host+loc.Path != redirectURI || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != publicURL {
		e.t.Fatalf("redirect = %s", loc)
	}
	return loc.Query().Get("code"), rec
}

func (e *env) exchange(clientID, code, codeVerifier string) *httptest.ResponseRecorder {
	return e.do("POST", "/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {redirectURI}, "code_verifier": {codeVerifier},
	}, "")
}

func TestDiscovery(t *testing.T) {
	e := newEnv(t)
	meta := decode(t, e.do("GET", "/.well-known/oauth-authorization-server", nil, ""))
	if meta["issuer"] != publicURL || meta["registration_endpoint"] != publicURL+"/register" ||
		meta["token_endpoint"] != publicURL+"/token" || meta["authorization_endpoint"] != publicURL+"/authorize" {
		t.Errorf("metadata = %v", meta)
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		res := decode(t, e.do("GET", path, nil, ""))
		if res["resource"] != publicURL+"/mcp" || res["authorization_servers"].([]any)[0] != publicURL {
			t.Errorf("%s = %v", path, res)
		}
	}
}

func TestFullFlowAndRefresh(t *testing.T) {
	e := newEnv(t)
	clientID := e.registerClient()

	page := e.do("GET", "/authorize?"+authForm(clientID, redirectURI).Encode(), nil, "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `name="api_key"`) {
		t.Fatalf("login page = %d", page.Code)
	}
	if page.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("login page must not be frameable")
	}

	code, rec := e.signIn(clientID, "ana@nuanu.com", "good-key")
	if code == "" {
		t.Fatalf("sign-in failed: %d %s", rec.Code, rec.Body)
	}
	tok := decode(t, e.exchange(clientID, code, verifier))
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	if tok["token_type"] != "Bearer" || tok["expires_in"] != float64(3600) || access == "" || refresh == "" {
		t.Fatalf("token response = %v", tok)
	}

	info, err := e.srv.Verifier()(context.Background(), access, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := info.Extra["identity"].(Identity)
	if id.Login != "ana@nuanu.com" || id.APIKey != "good-key" || id.UID != 42 || info.UserID != "42" {
		t.Errorf("identity = %+v", id)
	}

	// Access token expires after an hour; refresh gives a new pair.
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.srv.Verifier()(context.Background(), access, nil); err == nil {
		t.Error("expired access token accepted")
	}
	rec = e.do("POST", "/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, "")
	if rec.Code != 200 {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body)
	}
	if _, err := e.srv.Verifier()(context.Background(), decode(t, rec)["access_token"].(string), nil); err != nil {
		t.Errorf("refreshed access token rejected: %v", err)
	}

	// Revoking the key in Odoo stops refresh.
	e.revoked = true
	rec = e.do("POST", "/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, "")
	if rec.Code != 400 || decode(t, rec)["error"] != "invalid_grant" {
		t.Errorf("refresh after revoke = %d %s", rec.Code, rec.Body)
	}
}

func TestWrongCredentialsReRenderForm(t *testing.T) {
	e := newEnv(t)
	code, rec := e.signIn(e.registerClient(), "ana@nuanu.com", "wrong")
	if code != "" || rec.Code != 200 || !strings.Contains(rec.Body.String(), "didn&#39;t accept") {
		t.Errorf("bad credentials: code=%q status=%d", code, rec.Code)
	}
	if strings.Contains(rec.Body.String(), "wrong") {
		t.Error("API key echoed back into the page")
	}
}

func TestCodeExchangeRejections(t *testing.T) {
	e := newEnv(t)
	clientID := e.registerClient()

	code, _ := e.signIn(clientID, "ana@nuanu.com", "good-key")
	if rec := e.exchange(clientID, code, "wrong-verifier"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "PKCE") {
		t.Errorf("bad PKCE = %d %s", rec.Code, rec.Body)
	}
	if rec := e.exchange(clientID, code, verifier); rec.Code != 200 {
		t.Fatalf("first exchange = %d %s", rec.Code, rec.Body)
	}
	if rec := e.exchange(clientID, code, verifier); rec.Code != 400 || !strings.Contains(rec.Body.String(), "already used") {
		t.Errorf("code replay = %d %s", rec.Code, rec.Body)
	}

	code, _ = e.signIn(clientID, "ana@nuanu.com", "good-key")
	e.now = e.now.Add(6 * time.Minute)
	if rec := e.exchange(clientID, code, verifier); rec.Code != 400 {
		t.Errorf("expired code = %d", rec.Code)
	}

	// An access token can't be used as a code or refresh token, and vice versa.
	e.now = e.now.Add(-6 * time.Minute)
	code, _ = e.signIn(clientID, "ana@nuanu.com", "good-key")
	tok := decode(t, e.exchange(clientID, code, verifier))
	if rec := e.do("POST", "/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["access_token"].(string)}}, ""); rec.Code != 400 {
		t.Errorf("access token accepted as refresh token: %d", rec.Code)
	}
	if _, err := e.srv.Verifier()(context.Background(), tok["refresh_token"].(string), nil); err == nil {
		t.Error("refresh token accepted as access token")
	}
	if _, err := e.srv.Verifier()(context.Background(), "garbage", nil); err == nil {
		t.Error("garbage token accepted")
	}
}

func TestRedirectValidation(t *testing.T) {
	e := newEnv(t)
	clientID := e.registerClient()

	// Registered client, unregistered redirect: error page, never a redirect to the attacker.
	rec := e.do("GET", "/authorize?"+authForm(clientID, "https://evil.example/cb").Encode(), nil, "")
	if rec.Code != 400 || rec.Header().Get("Location") != "" {
		t.Errorf("foreign redirect = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	form := authForm(clientID, "https://evil.example/cb")
	form.Set("login", "ana@nuanu.com")
	form.Set("api_key", "good-key")
	if rec := e.do("POST", "/authorize", form, ""); rec.Code != 400 || rec.Header().Get("Location") != "" {
		t.Errorf("foreign redirect on submit = %d", rec.Code)
	}

	// Unregistered (manually configured) clients may only redirect to ChatGPT.
	if rec := e.do("GET", "/authorize?"+authForm("manual-client", redirectURI).Encode(), nil, ""); rec.Code != 200 {
		t.Errorf("manual client to chatgpt.com = %d", rec.Code)
	}
	if rec := e.do("GET", "/authorize?"+authForm("manual-client", "https://evil.example/cb").Encode(), nil, ""); rec.Code != 400 {
		t.Errorf("manual client to evil = %d", rec.Code)
	}
	// Claude's published identity: the client ID is a metadata document URL.
	claude := authForm("https://claude.ai/oauth/claude-code-client-metadata", "https://claude.ai/api/mcp/auth_callback")
	if rec := e.do("GET", "/authorize?"+claude.Encode(), nil, ""); rec.Code != 200 {
		t.Errorf("Claude published identity = %d", rec.Code)
	}

	// PKCE is mandatory.
	noPKCE := authForm(clientID, redirectURI)
	noPKCE.Del("code_challenge")
	if rec := e.do("GET", "/authorize?"+noPKCE.Encode(), nil, ""); rec.Code != 400 {
		t.Errorf("missing PKCE = %d", rec.Code)
	}

	// Registration rejects non-https redirect URIs.
	if rec := e.do("POST", "/register", nil, `{"redirect_uris":["http://evil.example/cb"]}`); rec.Code != 400 {
		t.Errorf("http redirect registered: %d", rec.Code)
	}
}

func TestTokensFromAnotherSecretRejected(t *testing.T) {
	e := newEnv(t)
	clientID := e.registerClient()
	code, _ := e.signIn(clientID, "ana@nuanu.com", "good-key")
	if rec := e.exchange(e.registerClient(), code, verifier); rec.Code != 400 || !strings.Contains(rec.Body.String(), "client_id mismatch") {
		t.Errorf("code redeemed by another client: %d %s", rec.Code, rec.Body)
	}
	tok := decode(t, e.exchange(clientID, code, verifier))
	other, _ := New(publicURL, strings.Repeat("x", 32), nil)
	if _, err := other.Verifier()(context.Background(), tok["access_token"].(string), nil); err == nil {
		t.Error("token sealed with another secret was accepted")
	}
}
