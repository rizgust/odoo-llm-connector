package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rizgust/odoo-gpt-mcp/internal/config"
	"github.com/rizgust/odoo-gpt-mcp/internal/oauth"
	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
	"github.com/rizgust/odoo-gpt-mcp/internal/tools"
)

type stubOdoo struct{}

func (stubOdoo) UID(context.Context) (int, error)              { return 2, nil }
func (stubOdoo) ServerVersion(context.Context) (string, error) { return "16.0", nil }
func (stubOdoo) Execute(_ context.Context, _, _ string, _ []any, _ map[string]any, out any) error {
	return json.Unmarshal([]byte(`5`), out)
}

const token = "abcdefghijklmnopqrstuvwxyz012345-_"

func newTestHandler(publicHost string) http.Handler {
	cfg := &config.Config{AccessToken: token, PublicHost: publicHost}
	server := tools.NewServer(stubOdoo{}, tools.Options{
		Policy:       &policy.Policy{Blocked: config.DefaultBlockedModels, MaxTextLength: 500},
		DefaultLimit: 80,
		MaxLimit:     500,
	}, "test")
	return newHandler(cfg, server, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func post(h http.Handler, path, host, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
const countCall = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"count_records","arguments":{"model":"sale.order"}}}`

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler("").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz = %d %s", rec.Code, rec.Body)
	}
}

func TestTokenPath(t *testing.T) {
	h := newTestHandler("")
	for _, path := range []string{"/mcp", "/mcp/", "/mcp/wrong", "/mcp/" + token + "x", "/mcp/" + token[:len(token)-1]} {
		if rec := post(h, path, "example.com", initialize); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
	rec := post(h, "/mcp/"+token, "example.com", initialize)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"odoo"`) {
		t.Fatalf("initialize = %d %s", rec.Code, rec.Body)
	}
	rec = post(h, "/mcp/"+token, "example.com", countCall)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":5`) {
		t.Errorf("tools/call = %d %s", rec.Code, rec.Body)
	}
}

func TestPublicHostCheck(t *testing.T) {
	h := newTestHandler("odoo-mcp.example.com")
	if rec := post(h, "/mcp/"+token, "evil.example.net", initialize); rec.Code != http.StatusForbidden {
		t.Errorf("foreign host: status %d, want 403", rec.Code)
	}
	for _, host := range []string{"odoo-mcp.example.com", "odoo-mcp.example.com:443", "localhost:8000"} {
		if rec := post(h, "/mcp/"+token, host, initialize); rec.Code != 200 {
			t.Errorf("%s: status %d, want 200 (%s)", host, rec.Code, rec.Body)
		}
	}
}

func TestOAuthMode(t *testing.T) {
	cfg := &config.Config{PublicURL: "https://odoo.mcp.example.com", PublicHost: "odoo.mcp.example.com"}
	as, err := oauth.New(cfg.PublicURL, strings.Repeat("s", 32), func(context.Context, string, string) (int, error) { return 7, nil })
	if err != nil {
		t.Fatal(err)
	}
	var served []oauth.Identity
	server := tools.NewServer(stubOdoo{}, tools.Options{Policy: &policy.Policy{MaxTextLength: 500}, DefaultLimit: 80, MaxLimit: 500}, "test")
	h := newOAuthHandler(cfg, as, func(id oauth.Identity) *mcp.Server {
		served = append(served, id)
		return server
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// No token: 401 pointing at the resource metadata, which is how ChatGPT discovers sign-in.
	rec := post(h, "/mcp", "odoo.mcp.example.com", initialize)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"),
		`resource_metadata="https://odoo.mcp.example.com/.well-known/oauth-protected-resource"`) {
		t.Fatalf("unauthenticated = %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	// Get a real token through the flow.
	reg := httptest.NewRecorder()
	h.ServeHTTP(reg, httptest.NewRequest("POST", "/register", strings.NewReader(`{"redirect_uris":["https://chatgpt.com/cb"]}`)))
	var client struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(reg.Body.Bytes(), &client)
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	form := url.Values{"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {"https://chatgpt.com/cb"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"login": {"ana@nuanu.com"}, "api_key": {"k"}}
	authz := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(authz, req)
	loc, _ := url.Parse(authz.Header().Get("Location"))
	tokenForm := url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {"https://chatgpt.com/cb"}, "code_verifier": {verifier}}
	tokRec := httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/token", strings.NewReader(tokenForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(tokRec, req)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	json.Unmarshal(tokRec.Body.Bytes(), &tok)
	if tok.AccessToken == "" {
		t.Fatalf("no token: %s", tokRec.Body)
	}

	call := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(countCall))
	call.Host = "odoo.mcp.example.com"
	call.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", "application/json, text/event-stream")
	call.Header.Set("MCP-Protocol-Version", "2025-06-18")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, call)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":5`) {
		t.Fatalf("authenticated call = %d %s", rec.Code, rec.Body)
	}
	if len(served) == 0 || served[0].Login != "ana@nuanu.com" || served[0].UID != 7 {
		t.Errorf("request not routed to the signed-in user's server: %+v", served)
	}

	// Wrong host is still refused even with a valid token.
	call.Host = "evil.example.net"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, call)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign host = %d", rec.Code)
	}
}

func TestPluginDownload(t *testing.T) {
	zip := t.TempDir() + "/plugin.zip"
	if err := os.WriteFile(zip, []byte("PK-fake-zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{PublicURL: "https://odoo.mcp.example.com", PublicHost: "odoo.mcp.example.com", PluginZip: zip}
	as, _ := oauth.New(cfg.PublicURL, strings.Repeat("s", 32), nil)
	h := newOAuthHandler(cfg, as, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nuanu-odoo-plugin.zip", nil))
	if rec.Code != 200 || rec.Body.String() != "PK-fake-zip" || !strings.Contains(rec.Header().Get("Content-Disposition"), "nuanu-odoo-plugin.zip") {
		t.Errorf("download = %d %q %q", rec.Code, rec.Body, rec.Header().Get("Content-Disposition"))
	}

	cfg.PluginZip = ""
	h = newOAuthHandler(cfg, as, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nuanu-odoo-plugin.zip", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("download without PLUGIN_ZIP = %d", rec.Code)
	}
}
