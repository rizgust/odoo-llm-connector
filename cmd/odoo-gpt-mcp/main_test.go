package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rizgust/odoo-gpt-mcp/internal/config"
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
