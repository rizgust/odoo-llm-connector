// Command odoo-map inventories an Odoo database for the connector: apps, menus, report
// views, report-model fields, status values in use, and which catalog reports work.
// It reads metadata and counts only (no business records) and never writes.
//
//	go run ./cmd/odoo-map -env .env -out mapping
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rizgust/odoo-gpt-mcp/internal/config"
	"github.com/rizgust/odoo-gpt-mcp/internal/odoo"
	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
	"github.com/rizgust/odoo-gpt-mcp/internal/reports"
	"github.com/rizgust/odoo-gpt-mcp/internal/tools"
)

// Selection fields whose value distribution tells us how a model is actually used.
var statusFields = []string{"state", "move_type", "payment_state", "type", "invoice_status", "picking_type_code", "holiday_type"}

type fieldSummary struct {
	Label     string  `json:"label"`
	Type      string  `json:"type"`
	Relation  string  `json:"relation,omitempty"`
	Selection [][]any `json:"selection,omitempty"`
}

type modelMap struct {
	Model       string                    `json:"model"`
	UsedBy      []string                  `json:"used_by"`
	Readable    bool                      `json:"readable"`
	Error       string                    `json:"error,omitempty"`
	Records     int                       `json:"records"`
	Measures    map[string]fieldSummary   `json:"measures,omitempty"`
	Dimensions  map[string]fieldSummary   `json:"dimensions,omitempty"`
	Custom      map[string]fieldSummary   `json:"custom_fields,omitempty"`
	StatusUsage map[string]map[string]int `json:"status_usage,omitempty"`
}

type catalogStatus struct {
	Report  string `json:"report"`
	Model   string `json:"model"`
	Problem string `json:"problem,omitempty"`
	Records int    `json:"records_matching_base_filter"`
	MinDate string `json:"earliest,omitempty"`
	MaxDate string `json:"latest,omitempty"`
}

type moduleRow struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Application bool   `json:"application"`
	Category    string `json:"category"`
}

type result struct {
	GeneratedAt   string                    `json:"generated_at"`
	URL           string                    `json:"url"`
	DB            string                    `json:"db"`
	Context       map[string]any            `json:"context"`
	Apps          any                       `json:"apps"`
	Modules       []moduleRow               `json:"installed_modules"`
	CustomModules []moduleRow               `json:"custom_modules"`
	CustomModels  []map[string]any          `json:"custom_models"`
	Menus         []map[string]any          `json:"menus"`
	ReportsTool   map[string]any            `json:"list_reports"`
	Catalog       []catalogStatus           `json:"catalog_status"`
	ReportModels  map[string]*modelMap      `json:"report_models"`
	ModelCounts   map[string]map[string]int `json:"usage_by_company"`
}

func main() {
	envFile := flag.String("env", ".env", "env file with ODOO_URL, ODOO_DB, ODOO_USER, ODOO_API_KEY")
	outDir := flag.String("out", "mapping", "output directory")
	flag.Parse()
	if err := loadEnv(*envFile); err != nil {
		log.Fatal(err)
	}
	for _, k := range []string{"ODOO_URL", "ODOO_DB", "ODOO_USER", "ODOO_API_KEY"} {
		if os.Getenv(k) == "" {
			log.Fatalf("%s is not set", k)
		}
	}
	url := strings.TrimRight(os.Getenv("ODOO_URL"), "/")
	client := odoo.NewClient(url, os.Getenv("ODOO_DB"), os.Getenv("ODOO_USER"), os.Getenv("ODOO_API_KEY"), 120*time.Second)
	ctx := context.Background()

	m := &mapper{ctx: ctx, odoo: client}
	res, err := m.run(url)
	if err != nil {
		log.Fatal(err)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	path := filepath.Join(*outDir, fmt.Sprintf("%s-%s.json", res.DB, time.Now().Format("20060102-150405")))
	data, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
	printSummary(res)
	fmt.Printf("\nFull map written to %s\n", path)
}

// loadEnv sets variables from a KEY=VALUE file without overriding the real environment.
func loadEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string(rune(0xFEFF))))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

type mapper struct {
	ctx  context.Context
	odoo *odoo.Client
	sess *mcp.ClientSession
}

func (m *mapper) exec(model, method string, args []any, kwargs map[string]any, out any) error {
	return m.odoo.Execute(m.ctx, model, method, args, kwargs, out)
}

// tool calls a connector tool exactly as ChatGPT would.
func (m *mapper) tool(name string, args map[string]any) (map[string]any, error) {
	res, err := m.sess.CallTool(m.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, fmt.Errorf("%s: %s", name, res.Content[0].(*mcp.TextContent).Text)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	return out, json.Unmarshal(raw, &out)
}

func (m *mapper) run(url string) (*result, error) {
	if _, err := m.odoo.UID(m.ctx); err != nil {
		return nil, err
	}
	catalog, err := reports.Load("")
	if err != nil {
		return nil, err
	}
	pol := &policy.Policy{Blocked: config.DefaultBlockedModels, MaxTextLength: 500}
	server := tools.NewServer(m.odoo, tools.Options{Policy: pol, Catalog: catalog, DefaultLimit: 80, MaxLimit: 500}, "map")
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(m.ctx, st, nil); err != nil {
		return nil, err
	}
	if m.sess, err = mcp.NewClient(&mcp.Implementation{Name: "odoo-map"}, nil).Connect(m.ctx, ct, nil); err != nil {
		return nil, err
	}
	defer m.sess.Close()

	res := &result{GeneratedAt: time.Now().Format(time.RFC3339), URL: url, DB: os.Getenv("ODOO_DB"), ReportModels: map[string]*modelMap{}}
	step := func(name string) { fmt.Fprintf(os.Stderr, "• %s\n", name) }

	step("user context")
	if res.Context, err = m.tool("odoo_context", nil); err != nil {
		return nil, err
	}
	step("apps")
	apps, err := m.tool("list_modules", nil)
	if err != nil {
		return nil, err
	}
	res.Apps = apps["apps"]

	step("installed modules")
	if err := m.modules(res); err != nil {
		fmt.Fprintf(os.Stderr, "  modules: %v\n", err)
	}
	step("custom models")
	m.customModels(res)

	step("menus")
	menus, err := m.tool("list_menus", map[string]any{"limit": 100000})
	if err != nil {
		return nil, err
	}
	for _, item := range menus["menus"].([]any) {
		res.Menus = append(res.Menus, item.(map[string]any))
	}
	step("report catalog (list_reports)")
	if res.ReportsTool, err = m.tool("list_reports", nil); err != nil {
		return nil, err
	}

	step("report models: fields, status values, counts")
	for _, r := range catalog.Reports {
		m.reportModel(res, r.Model, "catalog:"+r.Name)
	}
	for _, menu := range res.ReportsTool["odoo_analysis_menus"].([]any) {
		mm := menu.(map[string]any)
		m.reportModel(res, mm["model"].(string), "menu:"+mm["path"].(string))
	}

	step("catalog report checks")
	for i := range catalog.Reports {
		res.Catalog = append(res.Catalog, m.catalogStatus(&catalog.Reports[i], pol))
	}

	step("usage by company")
	res.ModelCounts = m.usageByCompany()
	return res, nil
}

func (m *mapper) modules(res *result) error {
	// category_id may be false, so decode loosely.
	var raw []map[string]any
	if err := m.exec("ir.module.module", "search_read", []any{[]any{[]any{"state", "=", "installed"}}},
		map[string]any{"fields": []string{"name", "shortdesc", "author", "application", "category_id"}, "order": "name"}, &raw); err != nil {
		return err
	}
	for _, r := range raw {
		row := moduleRow{Name: str(r["name"]), Title: str(r["shortdesc"]), Author: str(r["author"]), Application: r["application"] == true}
		if c, ok := r["category_id"].([]any); ok && len(c) == 2 {
			row.Category = str(c[1])
		}
		res.Modules = append(res.Modules, row)
		if !strings.Contains(row.Author, "Odoo S.A.") && !strings.Contains(row.Author, "Odoo SA") {
			res.CustomModules = append(res.CustomModules, row)
		}
	}
	return nil
}

func (m *mapper) customModels(res *result) {
	var custom []string
	for _, c := range res.CustomModules {
		custom = append(custom, c.Name)
	}
	var rows []map[string]any
	if err := m.exec("ir.model", "search_read", []any{[]any{[]any{"transient", "=", false}}},
		map[string]any{"fields": []string{"model", "name", "modules", "state"}}, &rows); err != nil {
		fmt.Fprintf(os.Stderr, "  ir.model: %v\n", err)
		return
	}
	for _, r := range rows {
		mods := strings.Split(str(r["modules"]), ", ")
		studio := str(r["state"]) == "manual"
		fromCustom := slices.ContainsFunc(mods, func(s string) bool { return slices.Contains(custom, s) })
		if studio || fromCustom {
			fmt.Fprintf(os.Stderr, "    custom model %s\n", str(r["model"]))
			var n int
			readable := m.exec(str(r["model"]), "search_count", []any{[]any{}}, nil, &n) == nil
			res.CustomModels = append(res.CustomModels, map[string]any{
				"model": r["model"], "label": r["name"], "modules": r["modules"], "studio": studio,
				"readable": readable, "records": n,
			})
		}
	}
}

func (m *mapper) reportModel(res *result, model, usedBy string) {
	if mm, ok := res.ReportModels[model]; ok {
		if !slices.Contains(mm.UsedBy, usedBy) {
			mm.UsedBy = append(mm.UsedBy, usedBy)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "    model %s\n", model)
	mm := &modelMap{Model: model, UsedBy: []string{usedBy}}
	res.ReportModels[model] = mm

	var meta map[string]struct {
		String    string  `json:"string"`
		Type      string  `json:"type"`
		Relation  string  `json:"relation"`
		Selection [][]any `json:"selection"`
		Store     bool    `json:"store"`
	}
	if err := m.exec(model, "fields_get", nil, map[string]any{"attributes": []string{"string", "type", "relation", "selection", "store"}}, &meta); err != nil {
		mm.Error = err.Error()
		return
	}
	if err := m.exec(model, "search_count", []any{[]any{}}, nil, &mm.Records); err != nil {
		mm.Error = err.Error()
		return
	}
	mm.Readable = true
	mm.Measures, mm.Dimensions, mm.Custom = map[string]fieldSummary{}, map[string]fieldSummary{}, map[string]fieldSummary{}
	for name, f := range meta {
		if policy.FieldBlocked(name) || f.Type == "binary" {
			continue
		}
		fs := fieldSummary{Label: f.String, Type: f.Type, Relation: f.Relation}
		if strings.HasPrefix(name, "x_") {
			fs.Selection = f.Selection
			mm.Custom[name] = fs
		}
		if !f.Store || strings.HasPrefix(name, "message_") || strings.HasPrefix(name, "activity_") {
			continue
		}
		switch f.Type {
		case "integer", "float", "monetary":
			if name != "id" && name != "sequence" {
				mm.Measures[name] = fs
			}
		case "many2one", "selection", "date", "datetime", "boolean":
			if !slices.Contains([]string{"create_uid", "write_uid", "write_date", "message_main_attachment_id"}, name) {
				mm.Dimensions[name] = fs
			}
		}
	}
	for _, sf := range statusFields {
		f, ok := meta[sf]
		if !ok || !f.Store || f.Type != "selection" {
			continue
		}
		var groups []map[string]any
		if err := m.exec(model, "read_group", []any{[]any{}, []string{sf}, []string{sf}}, map[string]any{"lazy": false}, &groups); err != nil {
			continue
		}
		usage := map[string]int{}
		for _, g := range groups {
			usage[fmt.Sprint(g[sf])] = int(num(g["__count"]))
		}
		if mm.StatusUsage == nil {
			mm.StatusUsage = map[string]map[string]int{}
		}
		mm.StatusUsage[sf] = usage
	}
}

func (m *mapper) catalogStatus(r *reports.Report, pol *policy.Policy) catalogStatus {
	cs := catalogStatus{Report: r.Name, Model: r.Model, Problem: tools.CheckReport(m.ctx, m.odoo, pol, r)}
	if cs.Problem != "" {
		return cs
	}
	domain := substitute(r.Domain)
	if err := m.exec(r.Model, "search_count", []any{domain}, nil, &cs.Records); err != nil {
		cs.Problem = "count failed: " + err.Error()
		return cs
	}
	if r.DateField != "" && cs.Records > 0 {
		var span []map[string]any
		if err := m.exec(r.Model, "read_group", []any{domain, []string{"min_d:min(" + r.DateField + ")", "max_d:max(" + r.DateField + ")"}, []string{}},
			map[string]any{"lazy": false}, &span); err == nil && len(span) == 1 {
			cs.MinDate, cs.MaxDate = str(span[0]["min_d"]), str(span[0]["max_d"])
		}
	}
	return cs
}

// usageByCompany counts core documents per company, to see which apps are really used.
func (m *mapper) usageByCompany() map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, model := range []string{"sale.order", "account.move", "purchase.order", "stock.picking", "crm.lead", "pos.order",
		"hr.employee", "hr.expense", "project.task", "account.analytic.line", "mrp.production", "helpdesk.ticket", "event.registration"} {
		var groups []map[string]any
		if err := m.exec(model, "read_group", []any{[]any{}, []string{"company_id"}, []string{"company_id"}}, map[string]any{"lazy": false}, &groups); err != nil {
			continue
		}
		counts := map[string]int{}
		for _, g := range groups {
			company := "(none)"
			if c, ok := g["company_id"].([]any); ok && len(c) == 2 {
				company = str(c[1])
			}
			counts[company] = int(num(g["__count"]))
		}
		out[model] = counts
	}
	return out
}

func substitute(domain []any) []any {
	today := time.Now().Format(time.DateOnly)
	out := make([]any, len(domain))
	for i, v := range domain {
		switch t := v.(type) {
		case string:
			if t == "{today}" {
				v = today
			}
		case []any:
			v = substitute(t)
		}
		out[i] = v
	}
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func printSummary(res *result) {
	fmt.Println("\n================ ODOO MAP ================")
	ctx := res.Context
	fmt.Printf("DB %s  |  user %v  |  tz %v  |  today %v\n", res.DB, ctx["user"], ctx["timezone"], ctx["today"])
	fmt.Printf("companies: %v\n", ctx["companies"])
	if p, ok := ctx["privileges"].([]any); ok {
		fmt.Printf("privileges (%d): %v\n", len(p), p)
	}

	fmt.Printf("\n--- apps visible to this user (%d)\n", len(res.Apps.([]any)))
	for _, a := range res.Apps.([]any) {
		am := a.(map[string]any)
		fmt.Printf("  %-28s %s\n", am["app"], am["module"])
	}
	fmt.Printf("\n--- installed modules: %d total, %d not by Odoo S.A.\n", len(res.Modules), len(res.CustomModules))
	for _, c := range res.CustomModules {
		fmt.Printf("  %-40s %-40s %s\n", c.Name, c.Title, c.Author)
	}
	fmt.Printf("\n--- custom models (Studio or custom modules): %d\n", len(res.CustomModels))
	for _, c := range res.CustomModels {
		fmt.Printf("  %-40v %-35v records=%v readable=%v\n", c["model"], c["label"], c["records"], c["readable"])
	}

	reportingMenus := 0
	for _, mn := range res.Menus {
		if strings.Contains(strings.ToLower(str(mn["path"])), "reporting") {
			reportingMenus++
		}
	}
	fmt.Printf("\n--- menus with actions: %d (under Reporting: %d)\n", len(res.Menus), reportingMenus)
	fmt.Println("\n--- Odoo analysis views (Reporting menus, queryable)")
	for _, a := range res.ReportsTool["odoo_analysis_menus"].([]any) {
		am := a.(map[string]any)
		fmt.Printf("  %-60v %-32v %v %v\n", am["path"], am["model"], str(am["domain"]), str(am["context"]))
	}
	fmt.Printf("\n--- printable document models: %d\n", len(res.ReportsTool["printable_documents"].([]any)))

	fmt.Println("\n--- catalog reports")
	for _, c := range res.Catalog {
		if c.Problem != "" {
			fmt.Printf("  ✗ %-18s %-28s %s\n", c.Report, c.Model, c.Problem)
		} else {
			fmt.Printf("  ✓ %-18s %-28s records=%d  %s → %s\n", c.Report, c.Model, c.Records, c.MinDate, c.MaxDate)
		}
	}

	fmt.Println("\n--- report models")
	var names []string
	for n := range res.ReportModels {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		mm := res.ReportModels[n]
		if !mm.Readable {
			fmt.Printf("  %s: NOT READABLE (%s)\n", n, mm.Error)
			continue
		}
		fmt.Printf("  %s  records=%d  measures=%d dims=%d  custom=%v\n", n, mm.Records, len(mm.Measures), len(mm.Dimensions), keys(mm.Custom))
		for f, u := range mm.StatusUsage {
			fmt.Printf("      %s: %v\n", f, u)
		}
	}

	fmt.Println("\n--- documents per company")
	for model, counts := range res.ModelCounts {
		fmt.Printf("  %-24s %v\n", model, counts)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
