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
	"github.com/rizgust/odoo-gpt-mcp/internal/reports"
)

var modelFields = map[string]string{
	"sale.order": `{
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
	}`,
	"sale.report": `{
		"date": {"string": "Order Date", "type": "datetime", "store": true},
		"user_id": {"string": "Salesperson", "type": "many2one", "relation": "res.users", "store": true},
		"partner_id": {"string": "Customer", "type": "many2one", "relation": "res.partner", "store": true},
		"order_id": {"string": "Order", "type": "many2one", "relation": "sale.order", "store": true},
		"price_subtotal": {"string": "Untaxed Total", "type": "monetary", "store": true},
		"state": {"string": "Status", "type": "selection", "store": true}
	}`,
	"hr.payslip": `{
		"date_from": {"string": "From", "type": "date", "store": true},
		"employee_id": {"string": "Employee", "type": "many2one", "store": true},
		"net_wage": {"string": "Net", "type": "monetary", "store": true}
	}`,
	"account.invoice.report": `{"invoice_date": {"string": "Date", "type": "date", "store": true}}`,
}

const testCatalog = `
reports:
  - name: sales
    title: Sales
    description: Confirmed sales
    notes: Untaxed, company currency.
    model: sale.report
    date_field: date
    domain: [[state, in, [sale, done]]]
    measures:
      - {name: revenue, field: price_subtotal, agg: sum}
      - {name: orders, field: order_id, agg: count_distinct}
    dimensions:
      - {name: date, field: date}
      - {name: salesperson, field: user_id}
      - {name: customer, field: partner_id}
    default_group_by: [date]
  - name: old_orders
    title: Old orders
    description: Orders before today
    kind: list
    model: sale.order
    domain: [[date_order, "<", "{today}"]]
    fields: [name, partner_id, amount_total]
    order: date_order asc
  - name: payroll
    title: Payroll
    description: Net wages (user has no payroll access)
    model: hr.payslip
    date_field: date_from
    measures: [{name: net, field: net_wage, agg: sum}]
    dimensions: [{name: employee, field: employee_id}]
  - name: broken
    title: Broken
    description: References a field that doesn't exist
    model: sale.report
    measures: [{name: margin, field: margin, agg: sum}]
    dimensions: [{name: salesperson, field: user_id}]
  - name: uninstalled
    title: Uninstalled
    description: Model from a module that isn't installed
    model: pos.order
    measures: [{name: total, field: amount_total, agg: sum}]
    dimensions: [{name: session, field: session_id}]
`

type call struct {
	Model, Method string
	Args          []any
	Kwargs        map[string]any
}

type fakeOdoo struct {
	calls    []call
	err      error
	noAccess map[string]bool
}

func newFake() *fakeOdoo {
	return &fakeOdoo{noAccess: map[string]bool{"hr.payslip": true}}
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
	case method == "check_access_rights":
		_, known := modelFields[model]
		resp = fmt.Sprint(known && !f.noAccess[model])
	case method == "fields_get":
		var ok bool
		if resp, ok = modelFields[model]; !ok {
			return &odoo.Error{Name: "builtins.KeyError", Message: model}
		}
	case method == "search_count":
		resp = `3`
	case model == "ir.model":
		resp = `[{"model": "ir.config_parameter", "name": "System Parameter", "modules": "base"},
		         {"model": "hr.payslip", "name": "Pay Slip", "modules": "hr_payroll"},
		         {"model": "sale.order", "name": "Sales Order", "modules": "sale"}]`
	case model == "res.users":
		resp = `[{"name": "ChatGPT Reports", "tz": "Asia/Makassar", "lang": "en_US", "company_id": [1, "Nuanu"],
		          "company_ids": [1], "groups_id": [1, 2, 3]}]`
	case model == "res.groups":
		resp = `[{"full_name": "Sales / User: All Documents", "category_id": [5, "Sales"]},
		         {"full_name": "Technical / Multi-currencies", "category_id": [6, "Technical"]},
		         {"full_name": "Portal", "category_id": false}]`
	case model == "res.company":
		resp = `[{"id": 1, "name": "Nuanu", "currency_id": [12, "IDR"]}]`
	case model == "ir.ui.menu" && strings.Contains(fmt.Sprint(args), "parent_id"):
		resp = `[{"id": 1, "name": "Sales", "web_icon": "sale_management,static/description/icon.png"},
		         {"id": 2, "name": "Invoicing", "web_icon": "account,static/description/icon.png"}]`
	case model == "ir.ui.menu":
		resp = `[{"complete_name": "Sales", "action": false},
		         {"complete_name": "Sales/Orders/Orders", "action": "ir.actions.act_window,10"},
		         {"complete_name": "Sales/Reporting/Sales", "action": "ir.actions.act_window,11"},
		         {"complete_name": "Invoicing/Reporting/Invoice Analysis", "action": "ir.actions.act_window,12"},
		         {"complete_name": "Settings/Technical/System Parameters", "action": "ir.actions.act_window,13"},
		         {"complete_name": "Point of Sale/Reporting/Sales Details", "action": "ir.actions.act_window,14"},
		         {"complete_name": "Discuss", "action": "ir.actions.client,5"}]`
	case model == "ir.actions.act_window":
		resp = `[{"id": 10, "res_model": "sale.order", "view_mode": "tree,form", "domain": false, "context": "{}"},
		         {"id": 11, "res_model": "sale.report", "view_mode": "graph,pivot", "domain": "[]", "context": "{'group_by': ['date:month']}"},
		         {"id": 12, "res_model": "account.invoice.report", "view_mode": "graph,pivot", "domain": "[('move_type', 'in', ('out_invoice', 'out_refund'))]", "context": "{}"},
		         {"id": 13, "res_model": "ir.config_parameter", "view_mode": "tree,form", "domain": false, "context": "{}"},
		         {"id": 14, "res_model": "sale.order", "view_mode": "form", "target": "new", "domain": false, "context": "{}"}]`
	case model == "ir.model.data":
		resp = `[{"module": "sale", "res_id": 1}]`
	case model == "ir.module.module":
		resp = `[{"name": "sale", "shortdesc": "Sales", "summary": "From quotations to invoices", "category_id": [1, "Sales/Sales"]},
		         {"name": "account", "shortdesc": "Invoicing", "summary": false, "category_id": [2, "Accounting"]}]`
	case model == "ir.actions.report":
		resp = `[{"name": "Payslip", "model": "hr.payslip", "groups_id": []},
		         {"name": "Quotation / Order", "model": "sale.order", "groups_id": []},
		         {"name": "PRO-FORMA Invoice", "model": "sale.order", "groups_id": [3]},
		         {"name": "Secret Margin Sheet", "model": "sale.order", "groups_id": [99]}]`
	case method == "search_read":
		resp = `[{"id": 1, "name": "S001", "partner_id": [7, "Acme"], "client_order_ref": false, "access_token": "x"}]`
	case method == "read_group":
		group := map[string]any{"__count": 4, "__domain": []any{}}
		for _, spec := range args[2].([]string) {
			group[spec] = "G:" + spec
		}
		for _, agg := range args[1].([]string) {
			name, _, _ := strings.Cut(agg, ":")
			group[name] = 100.5
		}
		b, _ := json.Marshal([]any{group})
		resp = string(b)
	default:
		return fmt.Errorf("unexpected call %s.%s", model, method)
	}
	return json.Unmarshal([]byte(resp), out)
}

func (f *fakeOdoo) find(model, method string) *call {
	for i := range f.calls {
		if f.calls[i].Method == method && (model == "" || f.calls[i].Model == model) {
			return &f.calls[i]
		}
	}
	return nil
}

func connect(t *testing.T, exec odoo.Executor) *mcp.ClientSession {
	t.Helper()
	catalog, err := reports.Parse([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(exec, Options{
		Policy:       &policy.Policy{Blocked: config.DefaultBlockedModels, MaxTextLength: 500},
		Catalog:      catalog,
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

func mustCall(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	out, errText := callTool(t, s, name, args)
	if errText != "" {
		t.Fatalf("%s: %s", name, errText)
	}
	return out
}

// toJSON encodes without HTML escaping so expected domains can be written with < and >.
func toJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return strings.TrimSpace(b.String())
}

func list(v any) []map[string]any {
	var out []map[string]any
	for _, item := range v.([]any) {
		out = append(out, item.(map[string]any))
	}
	return out
}

func pluck(items []map[string]any, key string) []string {
	var out []string
	for _, item := range items {
		out = append(out, fmt.Sprint(item[key]))
	}
	return out
}

func TestToolsAreRegisteredReadOnly(t *testing.T) {
	s := connect(t, newFake())
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
	want := []string{"aggregate_records", "count_records", "describe_model", "list_menus", "list_models", "list_modules",
		"list_reports", "odoo_context", "run_report", "search_records"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestOdooContext(t *testing.T) {
	out := mustCall(t, connect(t, newFake()), "odoo_context", nil)
	// 20:00 UTC on Sep 30 is already Oct 1 in Bali.
	if out["today"] != "2026-10-01" || out["timezone"] != "Asia/Makassar" || out["default_company"] != "Nuanu" {
		t.Errorf("context = %v", out)
	}
	if c := list(out["companies"])[0]; c["currency"] != "IDR" {
		t.Errorf("companies = %v", out["companies"])
	}
	if p := fmt.Sprint(out["privileges"]); p != "[Sales / User: All Documents]" {
		t.Errorf("privileges = %v (technical and uncategorised groups should be hidden)", p)
	}
}

// ---- catalogs and privileges

func TestListModelsOnlyReadableAndAllowed(t *testing.T) {
	out := mustCall(t, connect(t, newFake()), "list_models", map[string]any{"search": "o"})
	if got := pluck(list(out["models"]), "model"); !slices.Equal(got, []string{"sale.order"}) {
		t.Errorf("models = %v; want blocked ir.* and unreadable hr.payslip hidden", got)
	}
}

func TestListModules(t *testing.T) {
	out := mustCall(t, connect(t, newFake()), "list_modules", nil)
	apps := list(out["apps"])
	if got := pluck(apps, "app"); !slices.Equal(got, []string{"Sales", "Invoicing"}) {
		t.Fatalf("apps = %v", got)
	}
	// Sales resolved through ir.model.data, Invoicing through the web_icon fallback.
	if apps[0]["module"] != "sale" || apps[0]["summary"] != "From quotations to invoices" {
		t.Errorf("sales app = %v", apps[0])
	}
	if apps[1]["module"] != "account" || apps[1]["title"] != "Invoicing" {
		t.Errorf("invoicing app = %v", apps[1])
	}
	filtered := mustCall(t, connect(t, newFake()), "list_modules", map[string]any{"search": "quotation"})
	if got := pluck(list(filtered["apps"]), "app"); !slices.Equal(got, []string{"Sales"}) {
		t.Errorf("search = %v", got)
	}
}

func TestListMenus(t *testing.T) {
	s := connect(t, newFake())
	out := mustCall(t, s, "list_menus", nil)
	menus := list(out["menus"])
	if got := pluck(menus, "path"); !slices.Equal(got, []string{
		"Sales/Orders/Orders", "Sales/Reporting/Sales", "Invoicing/Reporting/Invoice Analysis",
		"Settings/Technical/System Parameters", "Point of Sale/Reporting/Sales Details", "Discuss",
	}) {
		t.Fatalf("paths = %v (folder menus without action should be skipped)", got)
	}
	byPath := map[string]map[string]any{}
	for _, m := range menus {
		byPath[m["path"].(string)] = m
	}
	if m := byPath["Settings/Technical/System Parameters"]; m["queryable"] != false {
		t.Errorf("blocked model should not be queryable: %v", m)
	}
	if m := byPath["Sales/Reporting/Sales"]; m["model"] != "sale.report" || m["queryable"] != true || m["context"] != "{'group_by': ['date:month']}" {
		t.Errorf("sales report menu = %v", m)
	}
	if m := byPath["Sales/Orders/Orders"]; m["domain"] != nil || m["context"] != nil {
		t.Errorf("empty domain/context should be omitted: %v", m)
	}
	if m := byPath["Point of Sale/Reporting/Sales Details"]; m["wizard"] != true {
		t.Errorf("pop-up action should be marked wizard: %v", m)
	}
	if m := byPath["Discuss"]; m["action_type"] != "client" || m["model"] != nil {
		t.Errorf("client action = %v", m)
	}

	reporting := mustCall(t, s, "list_menus", map[string]any{"reporting_only": true, "app": "sales"})
	if got := pluck(list(reporting["menus"]), "path"); !slices.Equal(got, []string{"Sales/Reporting/Sales"}) {
		t.Errorf("reporting_only+app = %v", got)
	}
}

func TestListReportsRespectsPrivileges(t *testing.T) {
	out := mustCall(t, connect(t, newFake()), "list_reports", nil)

	company := list(out["company_reports"])
	if got := pluck(company, "name"); !slices.Equal(got, []string{"sales", "old_orders"}) {
		t.Errorf("company reports = %v; payroll (no access), broken and uninstalled must be hidden", got)
	}
	if out["company_reports_unavailable"] != float64(3) {
		t.Errorf("unavailable = %v", out["company_reports_unavailable"])
	}
	if c := company[0]; fmt.Sprint(c["dimensions"]) != "[date salesperson customer]" || c["period_filter"] != "date" {
		t.Errorf("sales entry = %v", c)
	}

	if got := pluck(list(out["odoo_analysis_menus"]), "path"); !slices.Equal(got, []string{"Sales/Reporting/Sales", "Invoicing/Reporting/Invoice Analysis"}) {
		t.Errorf("analysis menus = %v", got)
	}

	printable := list(out["printable_documents"])
	if len(printable) != 1 || printable[0]["model"] != "sale.order" ||
		fmt.Sprint(printable[0]["documents"]) != "[Quotation / Order PRO-FORMA Invoice]" {
		t.Errorf("printable = %v; payslip (no model access) and group-99 report must be hidden", printable)
	}
}

func TestListReportsSearch(t *testing.T) {
	out := mustCall(t, connect(t, newFake()), "list_reports", map[string]any{"search": "invoice"})
	if len(list(out["company_reports"])) != 0 {
		t.Errorf("company = %v", out["company_reports"])
	}
	if got := pluck(list(out["odoo_analysis_menus"]), "path"); !slices.Equal(got, []string{"Invoicing/Reporting/Invoice Analysis"}) {
		t.Errorf("analysis = %v", got)
	}
}

// ---- run_report

func TestRunAggregateReport(t *testing.T) {
	fake := newFake()
	out := mustCall(t, connect(t, fake), "run_report", map[string]any{
		"name": "sales", "date_from": "2026-09-01", "date_to": "2026-09-30",
		"group_by": []any{"date:week", "salesperson"}, "sort": "revenue desc",
	})
	c := fake.find("sale.report", "read_group")
	args := toJSON(c.Args)
	// Datetime boundaries are local midnight in Asia/Makassar (UTC+8), converted to UTC.
	want := `[[["state","in",["sale","done"]],["date",">=","2026-08-31 16:00:00"],["date","<","2026-09-30 16:00:00"]],` +
		`["price_subtotal:sum","order_id:count_distinct"],["date:week","user_id"]]`
	if args != want {
		t.Errorf("read_group args\n got %s\nwant %s", args, want)
	}
	if c.Kwargs["orderby"] != "price_subtotal desc" || c.Kwargs["lazy"] != false {
		t.Errorf("kwargs = %v", c.Kwargs)
	}
	if ctx := c.Kwargs["context"].(map[string]any); ctx["tz"] != "Asia/Makassar" {
		t.Errorf("context = %v", ctx)
	}
	row := list(out["rows"])[0]
	wantRow := map[string]any{"date:week": "G:date:week", "salesperson": "G:user_id", "revenue": 100.5, "orders": 100.5, "count": float64(4)}
	if fmt.Sprint(row) != fmt.Sprint(wantRow) {
		t.Errorf("row = %v, want %v", row, wantRow)
	}
	if p := out["period"].(map[string]any); p["from"] != "2026-09-01" || p["to"] != "2026-09-30" {
		t.Errorf("period = %v", p)
	}
	if out["notes"] != "Untaxed, company currency." {
		t.Errorf("notes = %v", out["notes"])
	}
}

func TestRunReportDefaultsAndFilters(t *testing.T) {
	fake := newFake()
	mustCall(t, connect(t, fake), "run_report", map[string]any{
		"name": "sales", "filters": []any{[]any{"partner_id.name", "ilike", "acme"}}, "sort": "count desc",
	})
	c := fake.find("sale.report", "read_group")
	args := toJSON(c.Args)
	want := `[[["state","in",["sale","done"]],["partner_id.name","ilike","acme"]],["price_subtotal:sum","order_id:count_distinct"],["date:month"]]`
	if args != want || c.Kwargs["orderby"] != "__count desc" {
		t.Errorf("args = %s kwargs = %v", args, c.Kwargs)
	}
}

func TestRunListReportSubstitutesToday(t *testing.T) {
	fake := newFake()
	out := mustCall(t, connect(t, fake), "run_report", map[string]any{"name": "old_orders"})
	c := fake.find("sale.order", "search_read")
	args := toJSON(c.Args)
	if args != `[[["date_order","<","2026-10-01"]]]` || c.Kwargs["order"] != "date_order asc" {
		t.Errorf("search_read args = %s kwargs = %v", args, c.Kwargs)
	}
	if out["total_matching"] != float64(3) || out["truncated"] != true {
		t.Errorf("out = %v", out)
	}
	if _, leaked := list(out["rows"])[0]["access_token"]; leaked {
		t.Error("secret field leaked")
	}
}

func TestRunReportErrors(t *testing.T) {
	s := connect(t, newFake())
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"unknown report":       {map[string]any{"name": "nope"}, "available: sales, old_orders"},
		"no access":            {map[string]any{"name": "payroll"}, "no read access to hr.payslip"},
		"broken definition":    {map[string]any{"name": "broken"}, "field(s) not found on sale.report: margin"},
		"module not installed": {map[string]any{"name": "uninstalled"}, "not available (module not installed?)"},
		"unknown dimension":    {map[string]any{"name": "sales", "group_by": []any{"region"}}, "unknown dimension"},
		"granularity on m2o":   {map[string]any{"name": "sales", "group_by": []any{"salesperson:month"}}, "not a date"},
		"bad granularity":      {map[string]any{"name": "sales", "group_by": []any{"date:decade"}}, "granularity"},
		"sort by ungrouped":    {map[string]any{"name": "sales", "sort": "customer desc"}, "when grouping by it"},
		"bad sort":             {map[string]any{"name": "sales", "sort": "price_subtotal desc"}, "sort must be"},
		"bad date":             {map[string]any{"name": "sales", "date_from": "01/09/2026"}, "YYYY-MM-DD"},
		"period on no-date":    {map[string]any{"name": "old_orders", "date_from": "2026-01-01"}, "no period filter"},
		"secret filter":        {map[string]any{"name": "sales", "filters": []any{[]any{"user_id.password", "=", "x"}}}, "not available"},
		"list sort non-field":  {map[string]any{"name": "old_orders", "sort": "state desc"}, "report's fields"},
	} {
		_, errText := callTool(t, s, "run_report", tc.args)
		if !strings.Contains(errText, tc.want) {
			t.Errorf("%s: error = %q, want it to contain %q", name, errText, tc.want)
		}
	}
}

// ---- ad-hoc tools

func TestDescribeModelHidesSecretAndBinary(t *testing.T) {
	out := mustCall(t, connect(t, newFake()), "describe_model", map[string]any{"model": "sale.order"})
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
	fake := newFake()
	_, errText := callTool(t, connect(t, fake), "search_records", map[string]any{"model": "res.users.apikeys"})
	if !strings.Contains(errText, "not available") {
		t.Errorf("error = %q", errText)
	}
	if len(fake.calls) != 0 {
		t.Errorf("Odoo was called: %v", fake.calls)
	}
}

func TestSearchRecords(t *testing.T) {
	fake := newFake()
	out := mustCall(t, connect(t, fake), "search_records", map[string]any{
		"model": "sale.order", "domain": []any{[]any{"amount_total", ">", 10}}, "limit": 100000,
	})
	c := fake.find("sale.order", "search_read")
	cols := c.Kwargs["fields"].([]string)
	for _, hidden := range []string{"access_token", "note", "image", "amount_display"} {
		if slices.Contains(cols, hidden) {
			t.Errorf("default fields include %s: %v", hidden, cols)
		}
	}
	if c.Kwargs["limit"] != 500 {
		t.Errorf("limit not clamped: %v", c.Kwargs["limit"])
	}
	rec := list(out["records"])[0]
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
	s := connect(t, newFake())
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
	fake := newFake()
	out := mustCall(t, connect(t, fake), "aggregate_records", map[string]any{
		"model": "sale.order", "groupby": []any{"date_order:month"}, "aggregates": []any{"amount_total:sum"},
	})
	c := fake.find("sale.order", "read_group")
	args := toJSON(c.Args)
	if args != `[[],["amount_total:sum"],["date_order:month"]]` || c.Kwargs["lazy"] != false {
		t.Errorf("read_group args = %s %v", args, c.Kwargs)
	}
	if ctx := c.Kwargs["context"].(map[string]any); ctx["tz"] != "Asia/Makassar" {
		t.Errorf("read_group should run in the user's timezone, context = %v", ctx)
	}
	group := list(out["groups"])[0]
	if _, ok := group["__domain"]; ok || group["__count"] != float64(4) || group["amount_total"] != 100.5 {
		t.Errorf("group = %v", group)
	}
}

func TestAggregateWithoutAggregatesReadsGroupbyFields(t *testing.T) {
	fake := newFake()
	mustCall(t, connect(t, fake), "aggregate_records", map[string]any{"model": "sale.order", "groupby": []any{"partner_id", "date_order:month"}})
	args := toJSON(fake.find("sale.order", "read_group").Args)
	if args != `[[],["partner_id","date_order"],["partner_id","date_order:month"]]` {
		t.Errorf("read_group args = %s", args)
	}
}

func TestAggregateRejectsBadInput(t *testing.T) {
	s := connect(t, newFake())
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
