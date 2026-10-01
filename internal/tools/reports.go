package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rizgust/odoo-gpt-mcp/internal/odoo"
	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
	"github.com/rizgust/odoo-gpt-mcp/internal/reports"
)

// CheckReport verifies a catalog report against the live database: model installed and
// allowed, every referenced field present, and grouped/aggregated fields stored.
// It returns "" when the report is usable, otherwise the reason it isn't.
func CheckReport(ctx context.Context, exec odoo.Executor, pol *policy.Policy, r *reports.Report) string {
	if !pol.ModelAllowed(r.Model) {
		return fmt.Sprintf("model %s is blocked by connector configuration", r.Model)
	}
	var meta map[string]policy.FieldMeta
	if err := exec.Execute(ctx, r.Model, "fields_get", nil, map[string]any{"attributes": []string{"type", "store"}}, &meta); err != nil {
		var oe *odoo.Error
		if errors.As(err, &oe) {
			return fmt.Sprintf("model %s is not available (module not installed?)", r.Model)
		}
		return "could not check: " + err.Error()
	}
	return checkFields(r, meta)
}

func checkFields(r *reports.Report, meta map[string]policy.FieldMeta) string {
	var missing []string
	for _, f := range r.ModelFields() {
		if _, ok := meta[f]; !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("field(s) not found on %s: %s", r.Model, strings.Join(missing, ", "))
	}
	for _, m := range r.Measures {
		if !meta[m.Field].Store {
			return fmt.Sprintf("measure field %s is not stored", m.Field)
		}
	}
	for _, d := range r.Dimensions {
		if !meta[d.Field].Store {
			return fmt.Sprintf("dimension field %s is not stored", d.Field)
		}
	}
	if r.DateField != "" && !isDate(meta[r.DateField].Type) {
		return fmt.Sprintf("date_field %s is not a date or datetime", r.DateField)
	}
	return ""
}

func isDate(fieldType string) bool { return fieldType == "date" || fieldType == "datetime" }

// reportProblem is CheckReport cached per report for the life of the process.
func (h *handlers) reportProblem(ctx context.Context, r *reports.Report) string {
	h.mu.Lock()
	reason, ok := h.reportCache[r.Name]
	h.mu.Unlock()
	if ok {
		return reason
	}
	reason = CheckReport(ctx, h.odoo, h.opts.Policy, r)
	if strings.HasPrefix(reason, "could not check") {
		return reason // transient; retry next time
	}
	h.mu.Lock()
	h.reportCache[r.Name] = reason
	h.mu.Unlock()
	return reason
}

// ---- list_reports

type listReportsIn struct {
	Search string `json:"search,omitempty" jsonschema:"optional keyword matched against report names, titles and descriptions"`
}

type companyReport struct {
	Name           string   `json:"name"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Notes          string   `json:"notes,omitempty"`
	Kind           string   `json:"kind"`
	Model          string   `json:"model"`
	PeriodFilter   string   `json:"period_filter,omitempty"`
	Measures       []string `json:"measures,omitempty"`
	Dimensions     []string `json:"dimensions,omitempty"`
	DefaultGroupBy []string `json:"default_group_by,omitempty"`
}

type printableReport struct {
	Model string   `json:"model"`
	Names []string `json:"documents"`
}

type listReportsOut struct {
	CompanyReports []companyReport   `json:"company_reports"`
	OdooAnalysis   []menuEntry       `json:"odoo_analysis_menus"`
	Printable      []printableReport `json:"printable_documents"`
	PrintableNote  string            `json:"printable_note,omitempty"`
	Unavailable    int               `json:"company_reports_unavailable"`
}

func (h *handlers) listReports(ctx context.Context, _ *mcp.CallToolRequest, in listReportsIn) (*mcp.CallToolResult, listReportsOut, error) {
	search := strings.ToLower(in.Search)
	matches := func(texts ...string) bool {
		return search == "" || strings.Contains(strings.ToLower(strings.Join(texts, " ")), search)
	}
	out := listReportsOut{CompanyReports: []companyReport{}, OdooAnalysis: []menuEntry{}, Printable: []printableReport{}}

	// 1. Company catalog: only reports that validated and whose model this user can read.
	var models []string
	for _, r := range h.opts.Catalog.Reports {
		models = append(models, r.Model)
	}
	readable := h.readable(ctx, models)
	for i := range h.opts.Catalog.Reports {
		r := &h.opts.Catalog.Reports[i]
		if !matches(r.Name, r.Title, r.Description) {
			continue
		}
		if !readable[r.Model] || h.reportProblem(ctx, r) != "" {
			out.Unavailable++
			continue
		}
		cr := companyReport{Name: r.Name, Title: r.Title, Description: r.Description, Notes: r.Notes, Kind: r.Kind,
			Model: r.Model, PeriodFilter: r.DateField, DefaultGroupBy: r.DefaultGroupBy}
		for _, m := range r.Measures {
			cr.Measures = append(cr.Measures, m.Name)
		}
		for _, d := range r.Dimensions {
			cr.Dimensions = append(cr.Dimensions, d.Name)
		}
		out.CompanyReports = append(out.CompanyReports, cr)
	}

	// 2. Odoo's own analysis views (each app's Reporting menu) the user can open.
	if menus, err := h.menus(ctx); err == nil {
		for _, m := range menus {
			if m.isReporting() && m.Model != "" && m.Queryable && !m.Wizard && matches(m.Path, m.Model) {
				out.OdooAnalysis = append(out.OdooAnalysis, m)
			}
		}
	}

	// 3. Printable (PDF) documents, filtered by the report's groups and model access.
	printable, err := h.printable(ctx)
	if err == nil {
		for _, p := range printable {
			if matches(append([]string{p.Model}, p.Names...)...) {
				out.Printable = append(out.Printable, p)
			}
		}
		if len(out.Printable) > 0 {
			out.PrintableNote = "Printable documents are generated per record inside Odoo (Print menu); this connector can query their data but not produce the PDF."
		}
	}
	return nil, out, nil
}

func (h *handlers) printable(ctx context.Context) ([]printableReport, error) {
	h.mu.Lock()
	c := h.printableCache
	h.mu.Unlock()
	if c != nil && h.opts.Now().Sub(c.at) < cacheTTL {
		return c.val, nil
	}
	u, err := h.user(ctx)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name     string `json:"name"`
		Model    string `json:"model"`
		GroupsID []int  `json:"groups_id"`
	}
	if err := h.odoo.Execute(ctx, "ir.actions.report", "search_read", []any{[]any{}},
		map[string]any{"fields": []string{"name", "model", "groups_id"}, "order": "model, name"}, &rows); err != nil {
		return nil, err
	}
	var models []string
	for _, r := range rows {
		if !slices.Contains(models, r.Model) {
			models = append(models, r.Model)
		}
	}
	readable := h.readable(ctx, models)
	var out []printableReport
	for _, r := range rows {
		if !readable[r.Model] || !h.opts.Policy.ModelAllowed(r.Model) {
			continue
		}
		if len(r.GroupsID) > 0 && !slices.ContainsFunc(r.GroupsID, func(g int) bool { return slices.Contains(u.GroupIDs, g) }) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Model == r.Model {
			if !slices.Contains(out[n-1].Names, r.Name) {
				out[n-1].Names = append(out[n-1].Names, r.Name)
			}
		} else {
			out = append(out, printableReport{Model: r.Model, Names: []string{r.Name}})
		}
	}
	h.mu.Lock()
	h.printableCache = &cached[[]printableReport]{out, h.opts.Now()}
	h.mu.Unlock()
	return out, nil
}

// ---- run_report

type runReportIn struct {
	Name     string   `json:"name" jsonschema:"company report name from list_reports, e.g. 'invoiced_revenue'"`
	DateFrom string   `json:"date_from,omitempty" jsonschema:"start of the period, YYYY-MM-DD, inclusive"`
	DateTo   string   `json:"date_to,omitempty" jsonschema:"end of the period, YYYY-MM-DD, inclusive"`
	GroupBy  []string `json:"group_by,omitempty" jsonschema:"dimensions to group by; date dimensions take :day|week|month|quarter|year, e.g. [\"date:quarter\", \"customer\"]"`
	Filters  []any    `json:"filters,omitempty" jsonschema:"extra Odoo domain ANDed with the report's own, e.g. [[\"commercial_partner_id.name\",\"ilike\",\"acme\"]]; \"{today}\" is replaced with today"`
	Sort     string   `json:"sort,omitempty" jsonschema:"'<measure|dimension|count> [asc|desc]', e.g. 'revenue_untaxed desc'"`
	Limit    int      `json:"limit,omitempty" jsonschema:"max rows/groups (default 80, capped by the server)"`
}

type period struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type runReportOut struct {
	Report    string           `json:"report"`
	Title     string           `json:"title"`
	Notes     string           `json:"notes,omitempty"`
	Period    *period          `json:"period,omitempty"`
	GroupBy   []string         `json:"group_by,omitempty"`
	Model     string           `json:"model"`
	Domain    []any            `json:"domain_applied"`
	Rows      []map[string]any `json:"rows"`
	Returned  int              `json:"returned"`
	Total     *int             `json:"total_matching,omitempty"`
	Truncated bool             `json:"truncated"`
}

func (h *handlers) runReport(ctx context.Context, _ *mcp.CallToolRequest, in runReportIn) (*mcp.CallToolResult, runReportOut, error) {
	r, ok := h.opts.Catalog.Get(in.Name)
	if !ok {
		var names []string
		for _, r := range h.opts.Catalog.Reports {
			names = append(names, r.Name)
		}
		return nil, runReportOut{}, fmt.Errorf("unknown report %q; available: %s", in.Name, strings.Join(names, ", "))
	}
	if reason := h.reportProblem(ctx, r); reason != "" {
		return nil, runReportOut{}, fmt.Errorf("report %s is not available: %s", r.Name, reason)
	}
	if !h.canRead(ctx, r.Model) {
		return nil, runReportOut{}, fmt.Errorf("the connected Odoo user has no read access to %s, which report %s needs", r.Model, r.Name)
	}
	if err := policy.CheckDomain(in.Filters); err != nil {
		return nil, runReportOut{}, err
	}
	meta, err := h.fields(ctx, r.Model)
	if err != nil {
		return nil, runReportOut{}, userError(err)
	}

	today := h.today(ctx)
	domain := append(substituteToday(r.Domain, today), substituteToday(in.Filters, today)...)
	out := runReportOut{Report: r.Name, Title: r.Title, Notes: r.Notes, Model: r.Model}

	if in.DateFrom != "" || in.DateTo != "" {
		if r.DateField == "" {
			return nil, runReportOut{}, fmt.Errorf("report %s has no period filter; drop date_from/date_to", r.Name)
		}
		terms, p, err := h.periodDomain(ctx, r.DateField, meta[r.DateField].Type, in.DateFrom, in.DateTo)
		if err != nil {
			return nil, runReportOut{}, err
		}
		domain = append(domain, terms...)
		out.Period = p
	}
	if domain == nil {
		domain = []any{}
	}
	out.Domain = domain
	limit := h.clampLimit(in.Limit)

	if r.Kind == reports.KindList {
		return h.runListReport(ctx, r, in, meta, domain, limit, out)
	}
	return h.runAggregateReport(ctx, r, in, meta, domain, limit, out)
}

func (h *handlers) runListReport(ctx context.Context, r *reports.Report, in runReportIn, meta map[string]policy.FieldMeta,
	domain []any, limit int, out runReportOut) (*mcp.CallToolResult, runReportOut, error) {
	columns, err := policy.ResolveFields(meta, r.Fields)
	if err != nil {
		return nil, runReportOut{}, err
	}
	order := r.Order
	if in.Sort != "" {
		if err := policy.CheckOrder(in.Sort); err != nil {
			return nil, runReportOut{}, err
		}
		field, _, _ := strings.Cut(in.Sort, " ")
		if !slices.Contains(r.Fields, field) {
			return nil, runReportOut{}, fmt.Errorf("sort must use one of the report's fields: %s", strings.Join(r.Fields, ", "))
		}
		order = in.Sort
	}
	kwargs := map[string]any{"fields": columns, "limit": limit}
	if order != "" {
		kwargs["order"] = order
	}
	var rows []map[string]any
	if err := h.odoo.Execute(ctx, r.Model, "search_read", []any{domain}, kwargs, &rows); err != nil {
		return nil, runReportOut{}, userError(err)
	}
	var total int
	if err := h.odoo.Execute(ctx, r.Model, "search_count", []any{domain}, nil, &total); err != nil {
		return nil, runReportOut{}, userError(err)
	}
	out.Rows = make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out.Rows = append(out.Rows, h.opts.Policy.ShapeRecord(row, meta))
	}
	out.Returned, out.Total, out.Truncated = len(rows), &total, len(rows) < total
	return nil, out, nil
}

func (h *handlers) runAggregateReport(ctx context.Context, r *reports.Report, in runReportIn, meta map[string]policy.FieldMeta,
	domain []any, limit int, out runReportOut) (*mcp.CallToolResult, runReportOut, error) {
	groupBy := in.GroupBy
	if len(groupBy) == 0 {
		groupBy = r.DefaultGroupBy
	}
	if len(groupBy) == 0 {
		groupBy = []string{r.Dimensions[0].Name}
	}

	// Translate dimension names to read_group specs and remember how to rename result keys.
	rename := map[string]string{"__count": "count"}
	var specs []string
	for _, g := range groupBy {
		dim, ok := r.Dimension(g)
		if !ok {
			return nil, runReportOut{}, fmt.Errorf("unknown dimension %q for %s; use one of: %s", g, r.Name, dimensionNames(r))
		}
		_, gran, _ := strings.Cut(g, ":")
		spec := dim.Field
		label := dim.Name
		if isDate(meta[dim.Field].Type) {
			if gran == "" {
				gran = "month"
			}
			if !slices.Contains(reports.Granularities, gran) {
				return nil, runReportOut{}, fmt.Errorf("granularity %q must be one of %s", gran, strings.Join(reports.Granularities, ", "))
			}
			spec += ":" + gran
			label += ":" + gran
		} else if gran != "" {
			return nil, runReportOut{}, fmt.Errorf("dimension %s is not a date, so it takes no granularity", dim.Name)
		}
		specs = append(specs, spec)
		rename[spec] = label
		out.GroupBy = append(out.GroupBy, label)
	}

	var aggregates []string
	for _, m := range r.Measures {
		aggregates = append(aggregates, m.Field+":"+m.Agg)
		rename[m.Field] = m.Name
	}

	sort := in.Sort
	if sort == "" {
		sort = r.DefaultSort
	}
	var orderby string
	if sort != "" {
		key, ok := r.SortKey(sort)
		if !ok {
			return nil, runReportOut{}, fmt.Errorf("sort must be '<measure|dimension|count> [asc|desc]'; measures: %s; dimensions: %s", measureNames(r), dimensionNames(r))
		}
		switch m, isMeasure := r.Measure(key[0]); {
		case key[0] == "count":
			orderby = "__count " + key[1]
		case isMeasure:
			orderby = m.Field + " " + key[1]
		default:
			idx := slices.IndexFunc(out.GroupBy, func(l string) bool { d, _, _ := strings.Cut(l, ":"); return d == key[0] })
			if idx < 0 {
				return nil, runReportOut{}, fmt.Errorf("can only sort by dimension %s when grouping by it", key[0])
			}
			orderby = specs[idx] + " " + key[1]
		}
	}

	kwargs := map[string]any{"limit": limit, "lazy": false, "context": h.rpcContext(ctx)}
	if orderby != "" {
		kwargs["orderby"] = orderby
	}
	var groups []map[string]any
	if err := h.odoo.Execute(ctx, r.Model, "read_group", []any{domain, aggregates, specs}, kwargs, &groups); err != nil {
		return nil, runReportOut{}, userError(err)
	}
	out.Rows = make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		shaped := h.opts.Policy.ShapeRecord(g, meta)
		row := make(map[string]any, len(shaped))
		for k, v := range shaped {
			if name, ok := rename[k]; ok {
				row[name] = v
			}
		}
		out.Rows = append(out.Rows, row)
	}
	out.Returned, out.Truncated = len(groups), len(groups) >= limit
	return nil, out, nil
}

// periodDomain builds inclusive date_from/date_to terms. Datetime fields are stored in UTC,
// so day boundaries are taken in the user's timezone and converted.
func (h *handlers) periodDomain(ctx context.Context, field, fieldType, from, to string) ([]any, *period, error) {
	var terms []any
	p := &period{From: from, To: to}
	loc := h.location(ctx)
	parse := func(s, label string) (time.Time, error) {
		t, err := time.ParseInLocation(time.DateOnly, s, loc)
		if err != nil {
			return t, fmt.Errorf("%s must be YYYY-MM-DD, got %q", label, s)
		}
		return t, nil
	}
	if from != "" {
		t, err := parse(from, "date_from")
		if err != nil {
			return nil, nil, err
		}
		value := from
		if fieldType == "datetime" {
			value = t.UTC().Format(time.DateTime)
		}
		terms = append(terms, []any{field, ">=", value})
	}
	if to != "" {
		t, err := parse(to, "date_to")
		if err != nil {
			return nil, nil, err
		}
		if fieldType == "datetime" {
			terms = append(terms, []any{field, "<", t.AddDate(0, 0, 1).UTC().Format(time.DateTime)})
		} else {
			terms = append(terms, []any{field, "<=", to})
		}
	}
	return terms, p, nil
}

func substituteToday(domain []any, today string) []any {
	if domain == nil {
		return nil
	}
	out := make([]any, len(domain))
	for i, v := range domain {
		switch t := v.(type) {
		case string:
			if t == "{today}" {
				v = today
			}
		case []any:
			v = substituteToday(t, today)
		}
		out[i] = v
	}
	return out
}

func dimensionNames(r *reports.Report) string {
	var n []string
	for _, d := range r.Dimensions {
		n = append(n, d.Name)
	}
	return strings.Join(n, ", ")
}

func measureNames(r *reports.Report) string {
	n := []string{"count"}
	for _, m := range r.Measures {
		n = append(n, m.Name)
	}
	return strings.Join(n, ", ")
}
