package odoo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeServer answers Odoo /jsonrpc calls with handle(service, method, args).
func fakeServer(t *testing.T, handle func(service, method string, args []any) (result any, rpcErr map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jsonrpc" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		result, rpcErr := handle(req.Params.Service, req.Params.Method, req.Params.Args)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExecuteAuthenticatesOnceAndPassesArgs(t *testing.T) {
	auths := 0
	srv := fakeServer(t, func(service, method string, args []any) (any, map[string]any) {
		switch {
		case service == "common" && method == "authenticate":
			auths++
			if args[0] != "db" || args[1] != "bot" || args[2] != "key" {
				t.Errorf("authenticate args = %v", args)
			}
			return 7, nil
		case service == "object" && method == "execute_kw":
			if args[1] != float64(7) || args[3] != "sale.order" || args[4] != "search_count" {
				t.Errorf("execute_kw args = %v", args)
			}
			return 42, nil
		}
		t.Errorf("unexpected %s.%s", service, method)
		return nil, nil
	})
	c := NewClient(srv.URL, "db", "bot", "key", 5*time.Second)
	for range 2 {
		var n int
		if err := c.Execute(context.Background(), "sale.order", "search_count", []any{[]any{}}, nil, &n); err != nil || n != 42 {
			t.Fatalf("Execute = %d, %v", n, err)
		}
	}
	if auths != 1 {
		t.Errorf("authenticated %d times, want 1", auths)
	}
}

func TestAuthenticationFailure(t *testing.T) {
	srv := fakeServer(t, func(string, string, []any) (any, map[string]any) { return false, nil })
	_, err := NewClient(srv.URL, "db", "bot", "wrong", 5*time.Second).UID(context.Background())
	if err == nil || err.Error() != "odoo authentication failed: check ODOO_DB, ODOO_USER and ODOO_API_KEY" {
		t.Errorf("err = %v", err)
	}
}

func TestOdooErrorUsesDataMessage(t *testing.T) {
	srv := fakeServer(t, func(service, method string, _ []any) (any, map[string]any) {
		if method == "authenticate" {
			return 2, nil
		}
		return nil, map[string]any{
			"code":    200,
			"message": "Odoo Server Error",
			"data": map[string]any{
				"name":    "odoo.exceptions.AccessError",
				"message": "You are not allowed to access 'Payslip' (hr.payslip) records.",
				"debug":   "Traceback (most recent call last): ...",
			},
		}
	})
	err := NewClient(srv.URL, "db", "bot", "key", 5*time.Second).Execute(context.Background(), "hr.payslip", "search_count", nil, nil, new(int))
	var oe *Error
	if !errors.As(err, &oe) || oe.Name != "odoo.exceptions.AccessError" || oe.Message != "You are not allowed to access 'Payslip' (hr.payslip) records." {
		t.Errorf("err = %#v", err)
	}
}

func TestUnreachable(t *testing.T) {
	_, err := NewClient("http://127.0.0.1:1", "db", "bot", "key", time.Second).UID(context.Background())
	var oe *Error
	if err == nil || errors.As(err, &oe) {
		t.Errorf("want transport error, got %v", err)
	}
}
