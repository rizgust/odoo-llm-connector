// Package reports holds the company report catalog: named, pre-approved report definitions
// (model, base filters, measures, dimensions) that ChatGPT runs instead of guessing.
package reports

import (
	_ "embed"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
)

//go:embed default.yaml
var defaultYAML []byte

// AggregateFuncs are the read_group functions a measure may use.
var AggregateFuncs = []string{"sum", "avg", "min", "max", "count", "count_distinct"}

// Granularities are the date groupings Odoo 16 read_group supports.
var Granularities = []string{"day", "week", "month", "quarter", "year"}

const (
	KindAggregate = "aggregate"
	KindList      = "list"
)

type Measure struct {
	Name  string `yaml:"name"`
	Field string `yaml:"field"`
	Agg   string `yaml:"agg"`
}

type Dimension struct {
	Name  string `yaml:"name"`
	Field string `yaml:"field"`
}

type Report struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Notes       string `yaml:"notes"`
	Kind        string `yaml:"kind"`
	Model       string `yaml:"model"`
	// DateField is what run_report's date_from/date_to filter on; empty = no period filter.
	DateField string `yaml:"date_field"`
	// Domain is always applied. The string "{today}" is replaced with today's date.
	Domain []any `yaml:"domain"`

	// aggregate reports
	Measures       []Measure   `yaml:"measures"`
	Dimensions     []Dimension `yaml:"dimensions"`
	DefaultGroupBy []string    `yaml:"default_group_by"`
	DefaultSort    string      `yaml:"default_sort"`

	// list reports
	Fields []string `yaml:"fields"`
	Order  string   `yaml:"order"`
}

type Catalog struct {
	Reports []Report `yaml:"reports"`
}

// Load reads the catalog at path, or the built-in default when path is empty.
func Load(path string) (*Catalog, error) {
	data := defaultYAML
	if path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("reading report catalog: %w", err)
		}
	}
	return Parse(data)
}

func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing report catalog: %w", err)
	}
	seen := map[string]bool{}
	for i := range c.Reports {
		r := &c.Reports[i]
		if r.Kind == "" {
			r.Kind = KindAggregate
		}
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("report catalog: report %q: %w", r.Name, err)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("report catalog: duplicate report name %q", r.Name)
		}
		seen[r.Name] = true
	}
	return &c, nil
}

func (c *Catalog) Get(name string) (*Report, bool) {
	for i := range c.Reports {
		if c.Reports[i].Name == name {
			return &c.Reports[i], true
		}
	}
	return nil, false
}

func (r *Report) validate() error {
	if r.Name == "" || strings.ContainsAny(r.Name, " :") {
		return fmt.Errorf("name is required and may not contain spaces or ':'")
	}
	if r.Model == "" {
		return fmt.Errorf("model is required")
	}
	if err := policy.CheckDomain(r.Domain); err != nil {
		return fmt.Errorf("domain: %w", err)
	}
	switch r.Kind {
	case KindAggregate:
		return r.validateAggregate()
	case KindList:
		if len(r.Fields) == 0 {
			return fmt.Errorf("list reports need fields")
		}
		return nil
	default:
		return fmt.Errorf("kind must be %q or %q", KindAggregate, KindList)
	}
}

func (r *Report) validateAggregate() error {
	if len(r.Measures) == 0 || len(r.Dimensions) == 0 {
		return fmt.Errorf("aggregate reports need at least one measure and one dimension")
	}
	names := map[string]bool{"count": true} // "count" is the per-group record count
	fields := map[string]bool{}
	for _, m := range r.Measures {
		if m.Name == "" || m.Field == "" || !slices.Contains(AggregateFuncs, m.Agg) {
			return fmt.Errorf("measure %q needs name, field and agg (one of %s)", m.Name, strings.Join(AggregateFuncs, ", "))
		}
		if names[m.Name] {
			return fmt.Errorf("duplicate measure/dimension name %q", m.Name)
		}
		// read_group returns one value per field, so two measures can't share a field.
		if fields[m.Field] {
			return fmt.Errorf("field %q is used by more than one measure", m.Field)
		}
		names[m.Name], fields[m.Field] = true, true
	}
	for _, d := range r.Dimensions {
		if d.Name == "" || d.Field == "" {
			return fmt.Errorf("dimension %q needs name and field", d.Name)
		}
		if names[d.Name] {
			return fmt.Errorf("duplicate measure/dimension name %q", d.Name)
		}
		if fields[d.Field] {
			return fmt.Errorf("field %q is both a measure and a dimension", d.Field)
		}
		names[d.Name] = true
	}
	for _, g := range r.DefaultGroupBy {
		if _, ok := r.Dimension(g); !ok {
			return fmt.Errorf("default_group_by %q is not a dimension", g)
		}
	}
	if r.DefaultSort != "" {
		if _, ok := r.SortKey(r.DefaultSort); !ok {
			return fmt.Errorf("default_sort %q must be '<measure|dimension|count> [asc|desc]'", r.DefaultSort)
		}
	}
	return nil
}

// Dimension resolves "name" or "name:granularity" to its dimension.
func (r *Report) Dimension(spec string) (Dimension, bool) {
	name, _, _ := strings.Cut(spec, ":")
	for _, d := range r.Dimensions {
		if d.Name == name {
			return d, true
		}
	}
	return Dimension{}, false
}

func (r *Report) Measure(name string) (Measure, bool) {
	for _, m := range r.Measures {
		if m.Name == name {
			return m, true
		}
	}
	return Measure{}, false
}

// SortKey validates "<measure|dimension|count> [asc|desc]" and returns the name and direction.
func (r *Report) SortKey(sort string) ([2]string, bool) {
	parts := strings.Fields(sort)
	if len(parts) == 0 || len(parts) > 2 {
		return [2]string{}, false
	}
	dir := "asc"
	if len(parts) == 2 {
		dir = strings.ToLower(parts[1])
		if dir != "asc" && dir != "desc" {
			return [2]string{}, false
		}
	}
	name := parts[0]
	_, isMeasure := r.Measure(name)
	_, isDim := r.Dimension(name)
	if name != "count" && !isMeasure && !isDim {
		return [2]string{}, false
	}
	return [2]string{name, dir}, true
}

// ModelFields lists every field on the report's model that the definition depends on,
// so it can be checked against the live Odoo database.
func (r *Report) ModelFields() []string {
	var out []string
	add := func(f string) {
		f, _, _ = strings.Cut(f, ":")
		f, _, _ = strings.Cut(f, ".")
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	add(r.DateField)
	for _, term := range r.Domain {
		if t, ok := term.([]any); ok {
			add(t[0].(string))
		}
	}
	for _, m := range r.Measures {
		add(m.Field)
	}
	for _, d := range r.Dimensions {
		add(d.Field)
	}
	for _, f := range r.Fields {
		add(f)
	}
	return out
}
