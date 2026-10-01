// Package policy decides what ChatGPT may see: model/field filtering and result shaping.
//
// The Odoo service user's access rights and record rules remain the real security
// boundary; this layer hides technical models and secret-looking fields and keeps
// responses small enough for ChatGPT's context.
package policy

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxDefaultFields = 40
	maxX2ManyIDs     = 20
)

var secretField = regexp.MustCompile(
	`(^|_)(password|passwd|pwd|token|secret|api_?key|apikey|totp|oauth\w*|signup\w*|signature|private_key)(_|$)`,
)

// FieldMeta is one entry of Odoo's fields_get.
type FieldMeta struct {
	String    string  `json:"string"`
	Type      string  `json:"type"`
	Relation  string  `json:"relation,omitempty"`
	Selection [][]any `json:"selection,omitempty"`
	Store     bool    `json:"store"`
}

// Error is a request rejected by policy; its message is shown to ChatGPT.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errorf(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

type Policy struct {
	Allowed       []string
	Blocked       []string
	MaxTextLength int
}

func matchAny(patterns []string, model string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool {
		ok, _ := path.Match(p, model)
		return ok
	})
}

func (p *Policy) ModelAllowed(model string) bool {
	if matchAny(p.Blocked, model) {
		return false
	}
	return len(p.Allowed) == 0 || matchAny(p.Allowed, model)
}

func (p *Policy) CheckModel(model string) error {
	if model == "" {
		return errorf("model is required, e.g. 'sale.order'")
	}
	if !p.ModelAllowed(model) {
		return errorf("model '%s' is not available through this connector", model)
	}
	return nil
}

// baseName strips a read_group granularity or aggregate suffix: "date_order:month" -> "date_order".
func baseName(spec string) string {
	name, _, _ := strings.Cut(spec, ":")
	return name
}

func FieldBlocked(name string) bool { return secretField.MatchString(name) }

// CheckFieldPath rejects "partner_id.user_ids.password"-style paths that touch a hidden field anywhere.
func CheckFieldPath(fieldPath string) error {
	for _, part := range strings.Split(fieldPath, ".") {
		if FieldBlocked(baseName(part)) {
			return errorf("field '%s' is not available through this connector", fieldPath)
		}
	}
	return nil
}

func CheckDomain(domain []any) error {
	for _, term := range domain {
		switch t := term.(type) {
		case string:
			if t != "&" && t != "|" && t != "!" {
				return errorf("invalid domain operator %q; use '&', '|' or '!'", t)
			}
		case []any:
			if len(t) != 3 {
				return errorf("invalid domain term %v; expected [field, operator, value]", t)
			}
			field, ok := t[0].(string)
			if !ok {
				return errorf("invalid domain field %v", t[0])
			}
			if _, ok := t[1].(string); !ok {
				return errorf("invalid domain operator %v", t[1])
			}
			if err := CheckFieldPath(field); err != nil {
				return err
			}
		default:
			return errorf("invalid domain term %v; expected [field, operator, value]", term)
		}
	}
	return nil
}

// CheckOrder validates an "a desc, b" order clause.
func CheckOrder(order string) error {
	for _, part := range strings.Split(order, ",") {
		if fields := strings.Fields(part); len(fields) > 0 {
			if err := CheckFieldPath(fields[0]); err != nil {
				return err
			}
		}
	}
	return nil
}

func visible(meta FieldMeta, name string) bool {
	return meta.Type != "binary" && !FieldBlocked(name)
}

// VisibleFields drops file blobs and secret-looking fields.
func VisibleFields(fields map[string]FieldMeta) map[string]FieldMeta {
	out := make(map[string]FieldMeta, len(fields))
	for name, meta := range fields {
		if visible(meta, name) {
			out[name] = meta
		}
	}
	return out
}

// DefaultFields picks a reasonable column set when the caller did not ask for specific fields.
func DefaultFields(fields map[string]FieldMeta) []string {
	var names []string
	for name, meta := range VisibleFields(fields) {
		switch {
		case !meta.Store, meta.Type == "html", meta.Type == "one2many",
			name == "display_name", name == "name",
			strings.HasPrefix(name, "message_"), strings.HasPrefix(name, "activity_"),
			strings.HasPrefix(name, "website_message"):
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	var preferred []string
	for _, n := range []string{"display_name", "name"} {
		if _, ok := fields[n]; ok {
			preferred = append(preferred, n)
		}
	}
	out := append(preferred, names...)
	if len(out) > maxDefaultFields {
		out = out[:maxDefaultFields]
	}
	return out
}

// ResolveFields validates the requested columns, or picks defaults when none were requested.
func ResolveFields(fields map[string]FieldMeta, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return DefaultFields(fields), nil
	}
	var unknown, hidden, out []string
	for _, name := range requested {
		meta, ok := fields[name]
		switch {
		case !ok:
			unknown = append(unknown, name)
		case !visible(meta, name):
			hidden = append(hidden, name)
		case !slices.Contains(out, name):
			out = append(out, name)
		}
	}
	if len(unknown) > 0 {
		return nil, errorf("unknown field(s): %s. Use describe_model to see available fields", strings.Join(unknown, ", "))
	}
	if len(hidden) > 0 {
		return nil, errorf("field(s) not available through this connector: %s", strings.Join(hidden, ", "))
	}
	return out, nil
}

func (p *Policy) shapeValue(value any, fieldType string) any {
	switch v := value.(type) {
	case bool:
		if !v && fieldType != "boolean" {
			return nil // Odoo uses false for "empty"
		}
	case string:
		if utf8.RuneCountInString(v) > p.MaxTextLength {
			return string([]rune(v)[:p.MaxTextLength]) + "…"
		}
	case []any:
		if (fieldType == "one2many" || fieldType == "many2many") && len(v) > maxX2ManyIDs {
			return append(v[:maxX2ManyIDs:maxX2ManyIDs], fmt.Sprintf("… %d more", len(v)-maxX2ManyIDs))
		}
	}
	return value
}

// ShapeRecord cleans one search_read row or read_group group for ChatGPT.
func (p *Policy) ShapeRecord(record map[string]any, fields map[string]FieldMeta) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		if strings.HasPrefix(key, "__") && key != "__count" {
			continue // read_group internals: __domain, __context, __fold
		}
		name := baseName(key)
		if FieldBlocked(name) {
			continue
		}
		out[key] = p.shapeValue(value, fields[name].Type)
	}
	return out
}
