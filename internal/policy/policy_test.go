package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/rizgust/odoo-gpt-mcp/internal/config"
)

func TestModelAllowed(t *testing.T) {
	p := &Policy{Blocked: config.DefaultBlockedModels}
	for model, want := range map[string]bool{
		"sale.order":                  true,
		"sale.report":                 true,
		"ir.config_parameter":         false,
		"res.users.apikeys":           false,
		"res.users.apikeys.show":      false,
		"res.partner.bank":            false,
		"res.partner":                 true,
		"res.config.settings":         false,
		"account.move":                true,
		"payment.provider":            false,
		"payment.transaction":         true,
		"base.module.uninstall":       false,
		"ir.attachment":               false,
		"account.invoice.report":      true,
		"mail.mail":                   false,
		"mail.message":                true,
		"stock.quant":                 true,
		"auth.totp.device":            false,
		"hr.employee":                 true,
		"res.users.log":               false,
		"res.users":                   true,
		"res.users.identitycheck":     true,
		"fetchmail.server":            false,
		"mail.alias.domain":           false,
		"crm.lead":                    true,
		"bus.presence":                false,
		"iap.account":                 false,
		"product.template":            true,
		"account.bank.statement.line": true,
	} {
		if got := p.ModelAllowed(model); got != want {
			t.Errorf("ModelAllowed(%q) = %v, want %v", model, got, want)
		}
	}

	allow := &Policy{Allowed: []string{"sale.*", "account.move"}, Blocked: config.DefaultBlockedModels}
	if !allow.ModelAllowed("sale.order") || !allow.ModelAllowed("account.move") || allow.ModelAllowed("crm.lead") {
		t.Error("allowlist not applied")
	}
}

func TestFieldBlocked(t *testing.T) {
	for name, want := range map[string]bool{
		"password":          true,
		"new_password":      true,
		"access_token":      true,
		"api_key":           true,
		"oauth_access_key":  true,
		"signup_token":      true,
		"signature":         true,
		"totp_secret":       true,
		"amount_total":      false,
		"partner_id":        false,
		"tokenized":         false,
		"shipping_weight":   false,
		"date_order":        false,
		"client_order_ref":  false,
		"is_company":        false,
		"secretary_id":      false,
		"x_studio_password": true,
	} {
		if got := FieldBlocked(name); got != want {
			t.Errorf("FieldBlocked(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCheckDomain(t *testing.T) {
	ok := [][]any{
		nil,
		{[]any{"state", "=", "sale"}},
		{"|", []any{"state", "=", "sale"}, []any{"state", "=", "done"}},
		{"!", []any{"partner_id.country_id.code", "in", []any{"ID", "SG"}}},
	}
	for _, d := range ok {
		if err := CheckDomain(d); err != nil {
			t.Errorf("CheckDomain(%v) = %v, want nil", d, err)
		}
	}
	bad := [][]any{
		{[]any{"partner_id.user_ids.password", "=", "x"}},
		{"OR", []any{"state", "=", "sale"}},
		{[]any{"state", "="}},
		{[]any{1, "=", 2}},
		{42},
	}
	for _, d := range bad {
		if err := CheckDomain(d); err == nil {
			t.Errorf("CheckDomain(%v) = nil, want error", d)
		}
	}
}

func TestResolveAndDefaultFields(t *testing.T) {
	fields := map[string]FieldMeta{
		"name":             {Type: "char", Store: true},
		"amount_total":     {Type: "monetary", Store: true},
		"note":             {Type: "html", Store: true},
		"image_1920":       {Type: "binary", Store: true},
		"access_token":     {Type: "char", Store: true},
		"order_line":       {Type: "one2many", Store: true},
		"tag_ids":          {Type: "many2many", Store: true},
		"computed":         {Type: "float", Store: false},
		"message_ids":      {Type: "one2many", Store: true},
		"message_follower": {Type: "char", Store: true},
	}
	got := DefaultFields(fields)
	want := []string{"name", "amount_total", "tag_ids"}
	if !slices.Equal(got, want) {
		t.Errorf("DefaultFields = %v, want %v", got, want)
	}

	if cols, err := ResolveFields(fields, []string{"name", "computed", "name"}); err != nil || !slices.Equal(cols, []string{"name", "computed"}) {
		t.Errorf("ResolveFields explicit = %v, %v", cols, err)
	}
	if _, err := ResolveFields(fields, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown field error = %v", err)
	}
	if _, err := ResolveFields(fields, []string{"access_token"}); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("hidden field error = %v", err)
	}
	if _, err := ResolveFields(fields, []string{"image_1920"}); err == nil {
		t.Error("binary field should be rejected")
	}
}

func TestShapeRecord(t *testing.T) {
	p := &Policy{MaxTextLength: 5}
	ids := make([]any, 25)
	for i := range ids {
		ids[i] = float64(i)
	}
	fields := map[string]FieldMeta{
		"ref":       {Type: "char"},
		"active":    {Type: "boolean"},
		"partner":   {Type: "many2one"},
		"tag_ids":   {Type: "many2many"},
		"date":      {Type: "date"},
		"long_text": {Type: "text"},
	}
	got := p.ShapeRecord(map[string]any{
		"ref":          false,
		"active":       false,
		"partner":      []any{float64(7), "Acme"},
		"tag_ids":      ids,
		"date:month":   "January 2026",
		"long_text":    "héllo world",
		"access_token": "secret",
		"__count":      float64(3),
		"__domain":     []any{},
	}, fields)

	if got["ref"] != nil {
		t.Errorf("empty char should become nil, got %v", got["ref"])
	}
	if got["active"] != false {
		t.Errorf("boolean false must stay false, got %v", got["active"])
	}
	if got["long_text"] != "héllo…" {
		t.Errorf("truncation = %q", got["long_text"])
	}
	if tags := got["tag_ids"].([]any); len(tags) != 21 || tags[20] != "… 5 more" {
		t.Errorf("x2many truncation = %v", tags)
	}
	if len(ids) != 25 || ids[20] != float64(20) {
		t.Error("ShapeRecord mutated the input slice")
	}
	if _, ok := got["access_token"]; ok {
		t.Error("secret field leaked")
	}
	if _, ok := got["__domain"]; ok {
		t.Error("__domain should be dropped")
	}
	if got["__count"] != float64(3) || got["date:month"] != "Janua…" {
		t.Errorf("group keys lost: %v", got)
	}
}
