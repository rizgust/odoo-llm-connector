// Package tools registers the read-only Odoo reporting tools on an MCP server.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rizgust/odoo-gpt-mcp/internal/odoo"
	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
	"github.com/rizgust/odoo-gpt-mcp/internal/reports"
)

const Instructions = `You are connected to the company's Odoo 16 ERP (read-only). Use it to answer business and reporting questions.
Everything you see is limited to what the connected Odoo user is allowed to access; catalogs only list what that user can open.

Start of a conversation: call odoo_context (today's date, timezone, companies, currencies, the user's privileges).

When the user asks for "a report", a business metric, or doesn't name a module:
1. Call list_reports. Company reports are the official definitions: if one matches, use run_report and quote its notes.
   If the request is vague (e.g. "give me a report"), offer the matching company reports and Odoo analysis menus instead of guessing.
2. If no company report fits, odoo_analysis_menus show the analysis views Odoo itself offers (model + domain + context).
   Query that model with aggregate_records, translating the menu's Python domain into a JSON domain.
3. list_modules shows which apps the user has; list_menus shows what each app contains, like browsing Odoo's menus.

Ad-hoc questions:
- list_models finds models by keyword; describe_model gives exact field names and selection values. Inspect before querying.
- aggregate_records for totals and breakdowns (prefer *.report models); search_records for specific records; count_records for "how many".

Odoo domain syntax: a list of [field, operator, value] terms, implicitly AND-ed; prefix-notation "|" and "!" for OR/NOT.
Operators: = != > >= < <= like ilike "not ilike" in "not in" child_of. Dates are "YYYY-MM-DD", datetimes "YYYY-MM-DD HH:MM:SS" in UTC.
Related fields can be traversed with dots, e.g. ["partner_id.country_id.code", "=", "ID"].
Many2one values come back as [id, display_name]. Amounts on documents are in the document's currency unless the model
has a company-currency field (e.g. amount_total_signed, price_subtotal on reports).
Always state the report or model, filters and period you used. If a result says truncated, tell the user.
If Odoo reports an access error, explain that the connected user lacks that privilege; don't try to work around it.`

type Options struct {
	Policy       *policy.Policy
	Catalog      *reports.Catalog
	DefaultLimit int
	MaxLimit     int
	// Now is overridable for tests.
	Now func() time.Time
}

type handlers struct {
	odoo odoo.Executor
	opts Options

	mu          sync.Mutex
	fieldsCache map[string]map[string]policy.FieldMeta
	accessCache map[string]cached[bool]
	reportCache map[string]string
	userCache   *cached[*userInfo]
	menuCache   *cached[[]menuEntry]
}

// NewServer builds the MCP server with all tools registered.
func NewServer(exec odoo.Executor, opts Options, version string) *mcp.Server {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Catalog == nil {
		opts.Catalog = &reports.Catalog{}
	}
	h := &handlers{
		odoo:        exec,
		opts:        opts,
		fieldsCache: map[string]map[string]policy.FieldMeta{},
		accessCache: map[string]cached[bool]{},
		reportCache: map[string]string{},
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "odoo", Title: "Odoo", Version: version}, &mcp.ServerOptions{
		Instructions: Instructions,
	})

	f := false
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &f, OpenWorldHint: &f}
	tool := func(name, title, description string) *mcp.Tool {
		return &mcp.Tool{Name: name, Title: title, Description: description, Annotations: readOnly}
	}

	mcp.AddTool(s, tool("odoo_context", "Odoo context",
		"Who the connector is logged in as, their privileges (Odoo groups), companies, currencies and today's date. "+
			"Call once at the start of a conversation so relative periods (\"this month\") and currencies are right."),
		h.odooContextTool)

	// Catalogs
	mcp.AddTool(s, tool("list_reports", "Report catalog",
		"All reports available to the connected user: company_reports (official definitions, run with run_report), "+
			"odoo_analysis_menus (Odoo's own Reporting views: model, domain, context), and printable_documents (PDFs per model). "+
			"Start here when the user asks for a report or a business metric without naming a model."),
		h.listReports)
	mcp.AddTool(s, tool("run_report", "Run company report",
		"Run a company report from list_reports for a period, grouped by its dimensions, with optional extra filters."),
		h.runReport)
	mcp.AddTool(s, tool("list_modules", "App catalog",
		"Installed Odoo apps the connected user can open (as on their Odoo home screen), with module name and summary."),
		h.listModules)
	mcp.AddTool(s, tool("list_menus", "Menu catalog",
		"Odoo menus the connected user can see, with the model, domain and context each one opens. "+
			"Filter by app, keyword, or reporting_only to find analysis views."),
		h.listMenus)
	mcp.AddTool(s, tool("list_models", "Model catalog",
		"Find Odoo models (tables) the connected user can read, by keyword. Returns technical name, label and defining modules."),
		h.listModels)

	// Ad-hoc queries
	mcp.AddTool(s, tool("describe_model", "Describe Odoo model",
		"List a model's fields: label, type, related model, selection values, and whether it is stored. "+
			"Only stored fields can be used in aggregate_records groupby/aggregates and are reliable in domains. "+
			"Fields restricted to groups the user lacks are omitted by Odoo."),
		h.describeModel)
	mcp.AddTool(s, tool("search_records", "Search Odoo records",
		"Fetch individual records matching a domain. For totals or breakdowns use aggregate_records instead."),
		h.searchRecords)
	mcp.AddTool(s, tool("count_records", "Count Odoo records", "Count records matching a domain."),
		h.countRecords)
	mcp.AddTool(s, tool("aggregate_records", "Aggregate Odoo records",
		"Grouped totals (Odoo read_group): sums, averages and counts per period, salesperson, product, customer, etc. "+
			"Each group includes '__count' (number of records in it). Date groupings follow the user's timezone."),
		h.aggregateRecords)
	return s
}

// userError keeps Odoo and policy messages (they help ChatGPT fix its query) but hides
// transport details behind a generic message.
func userError(err error) error {
	var oe *odoo.Error
	var pe *policy.Error
	if errors.As(err, &oe) || errors.As(err, &pe) {
		return err
	}
	return fmt.Errorf("odoo request failed: %w", err)
}

func (h *handlers) fields(ctx context.Context, model string) (map[string]policy.FieldMeta, error) {
	h.mu.Lock()
	cached, ok := h.fieldsCache[model]
	h.mu.Unlock()
	if ok {
		return cached, nil
	}
	var meta map[string]policy.FieldMeta
	err := h.odoo.Execute(ctx, model, "fields_get", nil,
		map[string]any{"attributes": []string{"string", "type", "relation", "selection", "store"}}, &meta)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.fieldsCache[model] = meta
	h.mu.Unlock()
	return meta, nil
}

func (h *handlers) clampLimit(limit int) int {
	if limit <= 0 {
		limit = h.opts.DefaultLimit
	}
	return min(limit, h.opts.MaxLimit)
}

func nonNil(domain []any) []any {
	if domain == nil {
		return []any{}
	}
	return domain
}

// ---- odoo_context

type contextIn struct{}

type company struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

type contextOut struct {
	OdooVersion    string    `json:"odoo_version"`
	User           string    `json:"user"`
	Timezone       string    `json:"timezone"`
	Language       string    `json:"language"`
	DefaultCompany string    `json:"default_company"`
	Companies      []company `json:"companies"`
	Today          string    `json:"today"`
	Privileges     []string  `json:"privileges,omitempty"`
}

// many2one decodes Odoo's [id, "name"], or false when empty.
type many2one []any

func (m *many2one) UnmarshalJSON(data []byte) error {
	var v []any
	if json.Unmarshal(data, &v) == nil {
		*m = v
	}
	return nil
}

func (m many2one) name() string {
	if len(m) == 2 {
		if s, ok := m[1].(string); ok {
			return s
		}
	}
	return ""
}

func (h *handlers) odooContextTool(ctx context.Context, _ *mcp.CallToolRequest, _ contextIn) (*mcp.CallToolResult, contextOut, error) {
	u, err := h.user(ctx)
	if err != nil {
		return nil, contextOut{}, userError(err)
	}
	var companies []struct {
		ID         int      `json:"id"`
		Name       string   `json:"name"`
		CurrencyID many2one `json:"currency_id"`
	}
	if err := h.odoo.Execute(ctx, "res.company", "search_read", []any{[]any{[]any{"id", "in", u.CompanyIDs}}},
		map[string]any{"fields": []string{"name", "currency_id"}}, &companies); err != nil {
		return nil, contextOut{}, userError(err)
	}
	version, err := h.odoo.ServerVersion(ctx)
	if err != nil {
		return nil, contextOut{}, userError(err)
	}
	// Privileges are informational; don't fail the whole call if res.groups isn't readable.
	privileges, _ := h.privileges(ctx, u)
	out := contextOut{
		OdooVersion:    version,
		User:           u.Name,
		Timezone:       u.TZ,
		Language:       u.Lang,
		DefaultCompany: u.Company,
		Today:          h.opts.Now().In(u.loc).Format(time.DateOnly),
		Privileges:     privileges,
	}
	for _, c := range companies {
		out.Companies = append(out.Companies, company{ID: c.ID, Name: c.Name, Currency: c.CurrencyID.name()})
	}
	return nil, out, nil
}

// ---- list_models

type listModelsIn struct {
	Search string `json:"search,omitempty" jsonschema:"keyword matched against model name or label, e.g. 'invoice'"`
}

type modelInfo struct {
	Model   string `json:"model"`
	Label   string `json:"label"`
	Modules string `json:"modules"`
}

type listModelsOut struct {
	Models    []modelInfo `json:"models"`
	Truncated bool        `json:"truncated"`
}

func (h *handlers) listModels(ctx context.Context, _ *mcp.CallToolRequest, in listModelsIn) (*mcp.CallToolResult, listModelsOut, error) {
	domain := []any{[]any{"transient", "=", false}}
	if in.Search != "" {
		domain = append(domain, "|", []any{"model", "ilike", in.Search}, []any{"name", "ilike", in.Search})
	}
	var rows []struct {
		Model   string `json:"model"`
		Name    string `json:"name"`
		Modules any    `json:"modules"`
	}
	if err := h.odoo.Execute(ctx, "ir.model", "search_read", []any{domain},
		map[string]any{"fields": []string{"model", "name", "modules"}, "order": "model"}, &rows); err != nil {
		return nil, listModelsOut{}, userError(err)
	}
	var candidates []string
	for _, r := range rows {
		if h.opts.Policy.ModelAllowed(r.Model) {
			candidates = append(candidates, r.Model)
		}
	}
	readable := h.readable(ctx, candidates)

	const maxModels = 200
	out := listModelsOut{Models: []modelInfo{}}
	for _, r := range rows {
		if !readable[r.Model] {
			continue
		}
		if len(out.Models) == maxModels {
			out.Truncated = true
			break
		}
		modules, _ := r.Modules.(string)
		out.Models = append(out.Models, modelInfo{Model: r.Model, Label: r.Name, Modules: modules})
	}
	return nil, out, nil
}

// ---- describe_model

type describeIn struct {
	Model  string `json:"model" jsonschema:"technical model name, e.g. 'sale.order'"`
	Search string `json:"search,omitempty" jsonschema:"optional keyword to filter fields by name or label"`
}

type fieldInfo struct {
	Label     string  `json:"label"`
	Type      string  `json:"type"`
	Stored    bool    `json:"stored"`
	Relation  string  `json:"relation,omitempty"`
	Selection [][]any `json:"selection,omitempty"`
}

type describeOut struct {
	Model  string               `json:"model"`
	Fields map[string]fieldInfo `json:"fields"`
}

func (h *handlers) describeModel(ctx context.Context, _ *mcp.CallToolRequest, in describeIn) (*mcp.CallToolResult, describeOut, error) {
	if err := h.opts.Policy.CheckModel(in.Model); err != nil {
		return nil, describeOut{}, err
	}
	meta, err := h.fields(ctx, in.Model)
	if err != nil {
		return nil, describeOut{}, userError(err)
	}
	search := strings.ToLower(in.Search)
	out := describeOut{Model: in.Model, Fields: map[string]fieldInfo{}}
	for name, m := range policy.VisibleFields(meta) {
		if search != "" && !strings.Contains(name, search) && !strings.Contains(strings.ToLower(m.String), search) {
			continue
		}
		out.Fields[name] = fieldInfo{Label: m.String, Type: m.Type, Stored: m.Store, Relation: m.Relation, Selection: m.Selection}
	}
	return nil, out, nil
}

// ---- search_records

type searchIn struct {
	Model  string   `json:"model" jsonschema:"technical model name, e.g. 'sale.order'"`
	Domain []any    `json:"domain,omitempty" jsonschema:"Odoo domain, e.g. [[\"state\",\"in\",[\"sale\",\"done\"]],[\"date_order\",\">=\",\"2026-01-01\"]]; omit for all records"`
	Fields []string `json:"fields,omitempty" jsonschema:"fields to return; always pass the few you need (omitted = up to 40 stored fields)"`
	Order  string   `json:"order,omitempty" jsonschema:"sort, e.g. 'date_order desc, id desc'"`
	Limit  int      `json:"limit,omitempty" jsonschema:"max rows (default 80, capped by the server)"`
	Offset int      `json:"offset,omitempty" jsonschema:"rows to skip, for paging"`
}

type searchOut struct {
	Model         string           `json:"model"`
	TotalMatching int              `json:"total_matching"`
	Returned      int              `json:"returned"`
	Offset        int              `json:"offset"`
	Truncated     bool             `json:"truncated"`
	Records       []map[string]any `json:"records"`
}

func (h *handlers) searchRecords(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	p := h.opts.Policy
	if err := p.CheckModel(in.Model); err != nil {
		return nil, searchOut{}, err
	}
	if err := policy.CheckDomain(in.Domain); err != nil {
		return nil, searchOut{}, err
	}
	if err := policy.CheckOrder(in.Order); err != nil {
		return nil, searchOut{}, err
	}
	meta, err := h.fields(ctx, in.Model)
	if err != nil {
		return nil, searchOut{}, userError(err)
	}
	columns, err := policy.ResolveFields(meta, in.Fields)
	if err != nil {
		return nil, searchOut{}, err
	}
	domain := nonNil(in.Domain)
	kwargs := map[string]any{"fields": columns, "limit": h.clampLimit(in.Limit), "offset": max(in.Offset, 0)}
	if in.Order != "" {
		kwargs["order"] = in.Order
	}
	var rows []map[string]any
	if err := h.odoo.Execute(ctx, in.Model, "search_read", []any{domain}, kwargs, &rows); err != nil {
		return nil, searchOut{}, userError(err)
	}
	var total int
	if err := h.odoo.Execute(ctx, in.Model, "search_count", []any{domain}, nil, &total); err != nil {
		return nil, searchOut{}, userError(err)
	}
	out := searchOut{
		Model:         in.Model,
		TotalMatching: total,
		Returned:      len(rows),
		Offset:        max(in.Offset, 0),
		Records:       make([]map[string]any, 0, len(rows)),
	}
	out.Truncated = out.Offset+len(rows) < total
	for _, r := range rows {
		out.Records = append(out.Records, p.ShapeRecord(r, meta))
	}
	return nil, out, nil
}

// ---- count_records

type countIn struct {
	Model  string `json:"model" jsonschema:"technical model name, e.g. 'crm.lead'"`
	Domain []any  `json:"domain,omitempty" jsonschema:"Odoo domain; omit to count all records"`
}

type countOut struct {
	Model string `json:"model"`
	Count int    `json:"count"`
}

func (h *handlers) countRecords(ctx context.Context, _ *mcp.CallToolRequest, in countIn) (*mcp.CallToolResult, countOut, error) {
	if err := h.opts.Policy.CheckModel(in.Model); err != nil {
		return nil, countOut{}, err
	}
	if err := policy.CheckDomain(in.Domain); err != nil {
		return nil, countOut{}, err
	}
	var n int
	if err := h.odoo.Execute(ctx, in.Model, "search_count", []any{nonNil(in.Domain)}, nil, &n); err != nil {
		return nil, countOut{}, userError(err)
	}
	return nil, countOut{Model: in.Model, Count: n}, nil
}

// ---- aggregate_records

type aggregateIn struct {
	Model      string   `json:"model" jsonschema:"technical model name; reporting models like 'sale.report' work best"`
	Groupby    []string `json:"groupby" jsonschema:"fields to group by; dates take a granularity 'field:day|week|month|quarter|year', e.g. [\"date:month\", \"user_id\"]"`
	Aggregates []string `json:"aggregates,omitempty" jsonschema:"values to compute as 'field:func' with func = sum|avg|min|max|count|count_distinct, e.g. [\"price_subtotal:sum\"]; omit for record counts only"`
	Domain     []any    `json:"domain,omitempty" jsonschema:"Odoo domain filtering the records before grouping; omit for all records"`
	Orderby    string   `json:"orderby,omitempty" jsonschema:"sort groups, e.g. 'price_subtotal desc' or 'date:month'"`
	Limit      int      `json:"limit,omitempty" jsonschema:"max groups (default 80, capped by the server)"`
}

type aggregateOut struct {
	Model          string           `json:"model"`
	Groupby        []string         `json:"groupby"`
	ReturnedGroups int              `json:"returned_groups"`
	Truncated      bool             `json:"truncated"`
	Groups         []map[string]any `json:"groups"`
}

var aggregateFuncs = []string{"sum", "avg", "min", "max", "count", "count_distinct", "array_agg", "bool_and", "bool_or"}

func (h *handlers) aggregateRecords(ctx context.Context, _ *mcp.CallToolRequest, in aggregateIn) (*mcp.CallToolResult, aggregateOut, error) {
	p := h.opts.Policy
	if err := p.CheckModel(in.Model); err != nil {
		return nil, aggregateOut{}, err
	}
	if err := policy.CheckDomain(in.Domain); err != nil {
		return nil, aggregateOut{}, err
	}
	if len(in.Groupby) == 0 {
		return nil, aggregateOut{}, errors.New("groupby must contain at least one field")
	}
	if err := policy.CheckOrder(in.Orderby); err != nil {
		return nil, aggregateOut{}, err
	}
	meta, err := h.fields(ctx, in.Model)
	if err != nil {
		return nil, aggregateOut{}, userError(err)
	}
	for _, spec := range append(slices.Clone(in.Groupby), in.Aggregates...) {
		if err := policy.CheckFieldPath(spec); err != nil {
			return nil, aggregateOut{}, err
		}
		name, _, _ := strings.Cut(spec, ":")
		m, ok := meta[name]
		if !ok {
			return nil, aggregateOut{}, fmt.Errorf("unknown field '%s' on %s. Use describe_model to see available fields", name, in.Model)
		}
		if !m.Store {
			return nil, aggregateOut{}, fmt.Errorf("field '%s' is not stored, so it cannot be grouped or aggregated", name)
		}
	}
	for _, agg := range in.Aggregates {
		_, fn, ok := strings.Cut(agg, ":")
		if !ok || !slices.Contains(aggregateFuncs, fn) {
			return nil, aggregateOut{}, fmt.Errorf("aggregate '%s' needs a function, e.g. 'amount_total:sum' (one of %s)", agg, strings.Join(aggregateFuncs, ", "))
		}
	}

	// Odoo 16 read_group: with no aggregate fields it sums every numeric column, so pass the groupby fields.
	readFields := in.Aggregates
	if len(readFields) == 0 {
		for _, g := range in.Groupby {
			name, _, _ := strings.Cut(g, ":")
			readFields = append(readFields, name)
		}
	}
	limit := h.clampLimit(in.Limit)
	kwargs := map[string]any{"limit": limit, "lazy": false, "context": h.rpcContext(ctx)}
	if in.Orderby != "" {
		kwargs["orderby"] = in.Orderby
	}
	var groups []map[string]any
	if err := h.odoo.Execute(ctx, in.Model, "read_group", []any{nonNil(in.Domain), readFields, in.Groupby}, kwargs, &groups); err != nil {
		return nil, aggregateOut{}, userError(err)
	}
	out := aggregateOut{
		Model:          in.Model,
		Groupby:        in.Groupby,
		ReturnedGroups: len(groups),
		Truncated:      len(groups) >= limit,
		Groups:         make([]map[string]any, 0, len(groups)),
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, p.ShapeRecord(g, meta))
	}
	return nil, out, nil
}
