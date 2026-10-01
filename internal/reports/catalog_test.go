package reports

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultCatalogParses(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Reports) < 10 {
		t.Errorf("default catalog has %d reports", len(c.Reports))
	}
	for _, r := range c.Reports {
		if r.Title == "" || r.Description == "" {
			t.Errorf("%s: title and description are what ChatGPT reads; both are required in the default catalog", r.Name)
		}
	}
	sales, ok := c.Get("sales")
	if !ok {
		t.Fatal("sales report missing")
	}
	if got := sales.ModelFields(); !slices.Equal(got, []string{"date", "state", "price_subtotal", "product_uom_qty", "order_id",
		"commercial_partner_id", "user_id", "team_id", "product_id", "categ_id", "country_id", "company_id"}) {
		t.Errorf("sales fields = %v", got)
	}
	stock, _ := c.Get("stock_on_hand")
	if !slices.Contains(stock.ModelFields(), "location_id") {
		t.Errorf("dotted domain path should contribute its first field: %v", stock.ModelFields())
	}
}

func TestParseRejectsInvalidDefinitions(t *testing.T) {
	base := `
reports:
  - name: r
    title: R
    description: d
    model: sale.report
    measures: [{name: revenue, field: price_subtotal, agg: sum}]
    dimensions: [{name: salesperson, field: user_id}]
`
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatalf("base definition should parse: %v", err)
	}
	for name, tc := range map[string]struct{ old, new, want string }{
		"no model":          {"model: sale.report", "model: ''", "model is required"},
		"bad name":          {"name: r", "name: 'my report'", "may not contain spaces"},
		"bad agg":           {"agg: sum", "agg: median", "needs name, field and agg"},
		"no dimensions":     {"dimensions: [{name: salesperson, field: user_id}]", "dimensions: []", "at least one measure and one dimension"},
		"name clash":        {"name: salesperson", "name: revenue", "duplicate measure/dimension name"},
		"reserved count":    {"name: salesperson", "name: count", "duplicate measure/dimension name"},
		"field reuse":       {"field: user_id", "field: price_subtotal", "both a measure and a dimension"},
		"bad kind":          {"model: sale.report", "model: sale.report\n    kind: chart", "kind must be"},
		"list needs fields": {"model: sale.report", "model: sale.report\n    kind: list", "list reports need fields"},
		"bad domain":        {"model: sale.report", "model: sale.report\n    domain: [[state, in]]", "domain"},
		"secret domain":     {"model: sale.report", "model: sale.report\n    domain: [[user_id.password, '=', x]]", "not available"},
		"bad default group": {"model: sale.report", "model: sale.report\n    default_group_by: [region]", "default_group_by"},
		"bad default sort":  {"model: sale.report", "model: sale.report\n    default_sort: revenue sideways", "default_sort"},
	} {
		_, err := Parse([]byte(strings.Replace(base, tc.old, tc.new, 1)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
	dup := base + strings.TrimPrefix(base, "\nreports:\n")
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), "duplicate report name") {
		t.Errorf("duplicate report: err = %v", err)
	}
}

func TestSortKeyAndDimension(t *testing.T) {
	r := &Report{
		Measures:   []Measure{{Name: "revenue", Field: "price_subtotal", Agg: "sum"}},
		Dimensions: []Dimension{{Name: "date", Field: "date"}},
	}
	for sort, want := range map[string]bool{
		"revenue": true, "revenue DESC": true, "count asc": true, "date desc": true,
		"price_subtotal": false, "revenue up": false, "": false, "a b c": false,
	} {
		if _, ok := r.SortKey(sort); ok != want {
			t.Errorf("SortKey(%q) ok = %v, want %v", sort, ok, want)
		}
	}
	if d, ok := r.Dimension("date:quarter"); !ok || d.Field != "date" {
		t.Errorf("Dimension(date:quarter) = %v %v", d, ok)
	}
}
