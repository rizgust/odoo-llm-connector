package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rizgust/odoo-gpt-mcp/internal/config"
	"github.com/rizgust/odoo-gpt-mcp/internal/odoo"
	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
)

const saleFields = `{
	"name": {"string": "Order Reference", "type": "char", "store": true},
	"partner_id": {"string": "Customer", "type": "many2one", "relation": "res.partner", "store": true},
	"amount_total": {"string": "Total", "type": "monetary", "store": true},
	"date_order": {"string": "Order Date", "type": "datetime", "store": true},
	"state": {"string": "Status", "type": "selection", "selection": [["draft", "Quotation"], ["sale", "Sales Order"]], "store": true},
	"note": {"string": "Terms", "type": "html", "store": true},
	"client_order_ref": {"string": "Customer Ref", "type": "char", "store": true},
	"access_token": {"string": "Security Token", "type": "char", "store": true},
	"image": {"string": "Image", "type": "binary", "store": true},
	"amount_display": {"string": "Computed", "type": "monetary", "store": false}
}`

type call struct {
	Model, Method string
	Args          []any
	Kwargs        map[string]any
}

type fakeOdoo struct {
	calls []call
	err   error
}

func (f *fakeOdoo) UID(context.Context) (int, error)              { return 2, nil }
func (f *fakeOdoo) ServerVersion(context.Context) (string, error) { return "16.0", nil }

func (f *fakeOdoo) Execute(_ context.Context, model, method string, args []any, kwargs map[string]any, out any) error {
	f.calls = append(f.calls, call{model, method, args, kwargs})
	if f.err != nil {
		return f.err
	}
	var resp string
	switch {
	case method == "fields_get":
		resp = saleFields
	case method == "search_count":
		resp = `3`
	case model == "ir.model":
		resp = `[{"model": "ir.config_parameter", "name": "System Parameter", "modules": "base"},
		         {"model": "sale.order", "name": "Sales Order", "modules": "sale"}]`
	case model == "res.users":
		resp = `[{"name": "ChatGPT Reports", "tz": "Asia/Makassar", "lang": "en_US", "company_id": [1, "Nuanu"], "company_ids": [1]}]`
	case model == "res.company":
		resp = `[{"id": 1, "name": "Nuanu", "currency_id": [12, "IDR"]}]`
	case method == "search_read":
		resp = `[{"id": 1, "name": "S001", "partner_id": [7, "Acme"], "client_order_ref": false, "access_token": "x"}]`
	case method == "read_group":
		resp = `[{"date_order:month": "January 2026", "amount_total": 100.5, "__count": 2, "__domain": []}]`
	default:
		return fmt.Errorf("unexpected call %s.%s", model, method)
	}
	return json.Unmarshal([]byte(resp), out)
}

func (f *fakeOdoo) find(method string) *call {
	for i := range f.calls {
		if f.calls[i].Method == method && f.calls[i].Model != "ir.model" {
			return &f.calls[i]
		}
	}
	return nil
}

func connect(t *testing.T, exec odoo.Executor) *mcp.ClientSession {
	t.Helper()
	server := NewServer(exec, Options{
		Policy:       &policy.Policy{Blocked: config.DefaultBlockedModels, MaxTextLength: 500},
		DefaultLimit: 80,
		MaxLimit:     500,
		Now:          func() time.Time { return time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC) },
	}, "test")
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// callTool returns the structured result, or the error text when the tool reported an error.
func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if res.IsError {
		return nil, res.Content[0].(*mcp.TextContent).Text
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out, ""
}

func TestToolsAreRegisteredReadOnly(t *testing.T) {
	s := connect(t, &fakeOdoo{})
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
	slices.Sort(names)
	want := []string{"aggregate_records", "count_records", "describe_model", "list_models", "odoo_context", "search_records"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestOdooContext(t *testing.T) {
	out, errText := callTool(t, connect(t, &fakeOdoo{}), "odoo_context", nil)
	if errText != "" {
		t.Fatal(errText)
	}
	// 20:00 UTC on Sep 30 is already Oct 1 in Bali.
	if out["today"] != "2026-10-01" || out["timezone"] != "Asia/Makassar" || out["default_company"] != "Nuanu" {
		t.Errorf("context = %v", out)
	}
	if c := out["companies"].([]any)[0].(map[string]any); c["currency"] != "IDR" {
		t.Errorf("companies = %v", out["companies"])
	}
}

func TestListModelsHidesBlocked(t *testing.T) {
	out, _ := callTool(t, connect(t, &fakeOdoo{}), "list_models", map[string]any{"search": "o"})
	models := out["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["model"] != "sale.order" {
		t.Errorf("models = %v", models)
	}
}

func TestDescribeModelHidesSecretAndBinary(t *testing.T) {
	out, _ := callTool(t, connect(t, &fakeOdoo{}), "describe_model", map[string]any{"model": "sale.order"})
	fields := out["fields"].(map[string]any)
	if _, ok := fields["access_token"]; ok {
		t.Error("access_token exposed")
	}
	if _, ok := fields["image"]; ok {
		t.Error("binary field exposed")
	}
	if fields["partner_id"].(map[string]any)["relation"] != "res.partner" {
		t.Errorf("partner_id = %v", fields["partner_id"])
	}
	if fields["state"].(map[string]any)["selection"] == nil {
		t.Error("selection values missing")
	}
}

func TestBlockedModelNeverReachesOdoo(t *testing.T) {
	fake := &fakeOdoo{}
	_, errText := callTool(t, connect(t, fake), "search_records", map[string]any{"model": "res.users.apikeys"})
	if !strings.Contains(errText, "not available") {
		t.Errorf("error = %q", errText)
	}
	if len(fake.calls) != 0 {
		t.Errorf("Odoo was called: %v", fake.calls)
	}
}

func TestSearchRecords(t *testing.T) {
	fake := &fakeOdoo{}
	out, errText := callTool(t, connect(t, fake), "search_records", map[string]any{
		"model": "sale.order", "domain": []any{[]any{"amount_total", ">", 10}}, "limit": 100000,
	})
	if errText != "" {
		t.Fatal(errText)
	}
	c := fake.find("search_read")
	cols := c.Kwargs["fields"].([]string)
	for _, hidden := range []string{"access_token", "note", "image", "amount_display"} {
		if slices.Contains(cols, hidden) {
			t.Errorf("default fields include %s: %v", hidden, cols)
		}
	}
	if c.Kwargs["limit"] != 500 {
		t.Errorf("limit not clamped: %v", c.Kwargs["limit"])
	}
	rec := out["records"].([]any)[0].(map[string]any)
	if v, ok := rec["client_order_ref"]; !ok || v != nil {
		t.Errorf("false should become null, got %v", v)
	}
	if _, ok := rec["access_token"]; ok {
		t.Error("access_token leaked in record")
	}
	if out["total_matching"] != float64(3) || out["truncated"] != true {
		t.Errorf("paging info = %v", out)
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	s := connect(t, &fakeOdoo{})
	for name, args := range map[string]map[string]any{
		"secret path in domain": {"model": "sale.order", "domain": []any{[]any{"partner_id.user_ids.password", "=", "x"}}},
		"unknown field":         {"model": "sale.order", "fields": []any{"nope"}},
		"secret order":          {"model": "sale.order", "order": "access_token desc"},
	} {
		if _, errText := callTool(t, s, "search_records", args); errText == "" {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAggregateRecords(t *testing.T) {
	fake := &fakeOdoo{}
	out, errText := callTool(t, connect(t, fake), "aggregate_records", map[string]any{
		"model": "sale.order", "groupby": []any{"date_order:month"}, "aggregates": []any{"amount_total:sum"},
	})
	if errText != "" {
		t.Fatal(errText)
	}
	c := fake.find("read_group")
	args, _ := json.Marshal(c.Args)
	if string(args) != `[[],["amount_total:sum"],["date_order:month"]]` || c.Kwargs["lazy"] != false {
		t.Errorf("read_group args = %s %v", args, c.Kwargs)
	}
	group := out["groups"].([]any)[0].(map[string]any)
	if _, ok := group["__domain"]; ok || group["__count"] != float64(2) || group["amount_total"] != 100.5 {
		t.Errorf("group = %v", group)
	}
}

func TestAggregateWithoutAggregatesReadsGroupbyFields(t *testing.T) {
	fake := &fakeOdoo{}
	callTool(t, connect(t, fake), "aggregate_records", map[string]any{"model": "sale.order", "groupby": []any{"partner_id", "date_order:month"}})
	args, _ := json.Marshal(fake.find("read_group").Args)
	if string(args) != `[[],["partner_id","date_order"],["partner_id","date_order:month"]]` {
		t.Errorf("read_group args = %s", args)
	}
}

func TestAggregateRejectsBadInput(t *testing.T) {
	s := connect(t, &fakeOdoo{})
	for name, args := range map[string]map[string]any{
		"non-stored": {"model": "sale.order", "groupby": []any{"partner_id"}, "aggregates": []any{"amount_display:sum"}},
		"no func":    {"model": "sale.order", "groupby": []any{"partner_id"}, "aggregates": []any{"amount_total"}},
		"bad func":   {"model": "sale.order", "groupby": []any{"partner_id"}, "aggregates": []any{"amount_total:median"}},
		"unknown":    {"model": "sale.order", "groupby": []any{"nope"}},
		"empty":      {"model": "sale.order", "groupby": []any{}},
	} {
		if _, errText := callTool(t, s, "aggregate_records", args); errText == "" {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestOdooErrorMessageReachesChatGPT(t *testing.T) {
	fake := &fakeOdoo{err: &odoo.Error{Name: "odoo.exceptions.AccessError", Message: "You are not allowed to access 'Payslip'"}}
	_, errText := callTool(t, connect(t, fake), "count_records", map[string]any{"model": "hr.payslip"})
	if !strings.Contains(errText, "not allowed to access 'Payslip'") {
		t.Errorf("error = %q", errText)
	}
}
