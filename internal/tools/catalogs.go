package tools

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// menuEntry is one actionable Odoo menu the connected user can see.
type menuEntry struct {
	Path       string `json:"path"`
	ActionType string `json:"action_type"`
	Model      string `json:"model,omitempty"`
	ViewModes  string `json:"view_modes,omitempty"`
	Domain     string `json:"domain,omitempty"`
	Context    string `json:"context,omitempty"`
	// Queryable means the connector may query Model for this user.
	Queryable bool `json:"queryable"`
}

func (m menuEntry) app() string {
	app, _, _ := strings.Cut(m.Path, "/")
	return app
}

func (m menuEntry) isReporting() bool {
	return slices.ContainsFunc(strings.Split(m.Path, "/"), func(s string) bool {
		return strings.EqualFold(strings.TrimSpace(s), "Reporting")
	})
}

// menus returns every actionable menu visible to the connected user. Odoo's ir.ui.menu
// search already hides menus the user's groups or model access rights don't allow.
func (h *handlers) menus(ctx context.Context) ([]menuEntry, error) {
	h.mu.Lock()
	c := h.menuCache
	h.mu.Unlock()
	if c != nil && h.opts.Now().Sub(c.at) < cacheTTL {
		return c.val, nil
	}

	var rows []struct {
		CompleteName string `json:"complete_name"`
		Action       any    `json:"action"` // "ir.actions.act_window,123" or false
	}
	if err := h.odoo.Execute(ctx, "ir.ui.menu", "search_read", []any{[]any{}},
		map[string]any{"fields": []string{"complete_name", "action"}, "order": "sequence, id"}, &rows); err != nil {
		return nil, err
	}

	type ref struct {
		path, actionType string
		id               int
	}
	var refs []ref
	var windowIDs []int
	for _, r := range rows {
		action, _ := r.Action.(string)
		actionType, idStr, ok := strings.Cut(action, ",")
		if !ok {
			continue // folder menu without an action
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		refs = append(refs, ref{r.CompleteName, actionType, id})
		if actionType == "ir.actions.act_window" {
			windowIDs = append(windowIDs, id)
		}
	}

	type window struct {
		ID       int    `json:"id"`
		ResModel string `json:"res_model"`
		ViewMode string `json:"view_mode"`
		Domain   any    `json:"domain"`
		Context  any    `json:"context"`
	}
	var windows []window
	if len(windowIDs) > 0 {
		if err := h.odoo.Execute(ctx, "ir.actions.act_window", "read", []any{windowIDs},
			map[string]any{"fields": []string{"res_model", "view_mode", "domain", "context"}}, &windows); err != nil {
			return nil, err
		}
	}
	byID := make(map[int]window, len(windows))
	var models []string
	for _, w := range windows {
		byID[w.ID] = w
		if w.ResModel != "" && !slices.Contains(models, w.ResModel) {
			models = append(models, w.ResModel)
		}
	}
	readable := h.readable(ctx, models)

	out := make([]menuEntry, 0, len(refs))
	for _, r := range refs {
		e := menuEntry{Path: r.path, ActionType: strings.TrimPrefix(r.actionType, "ir.actions.")}
		if w, ok := byID[r.id]; ok {
			e.Model = w.ResModel
			e.ViewModes = w.ViewMode
			e.Domain, _ = w.Domain.(string)
			e.Context, _ = w.Context.(string)
			if e.Context == "{}" {
				e.Context = ""
			}
			if e.Domain == "[]" {
				e.Domain = ""
			}
			e.Queryable = h.opts.Policy.ModelAllowed(e.Model) && readable[e.Model]
		}
		out = append(out, e)
	}

	h.mu.Lock()
	h.menuCache = &cached[[]menuEntry]{out, h.opts.Now()}
	h.mu.Unlock()
	return out, nil
}

// ---- list_modules

type listModulesIn struct {
	Search string `json:"search,omitempty" jsonschema:"optional keyword matched against app name, module or summary"`
}

type moduleInfo struct {
	App      string `json:"app"`
	Module   string `json:"module,omitempty"`
	Title    string `json:"title,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Category string `json:"category,omitempty"`
}

type listModulesOut struct {
	Apps []moduleInfo `json:"apps"`
}

func (h *handlers) listModules(ctx context.Context, _ *mcp.CallToolRequest, in listModulesIn) (*mcp.CallToolResult, listModulesOut, error) {
	// Top-level menus visible to the user are exactly the apps on their Odoo home screen.
	var roots []struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		WebIcon any    `json:"web_icon"`
	}
	if err := h.odoo.Execute(ctx, "ir.ui.menu", "search_read", []any{[]any{[]any{"parent_id", "=", false}}},
		map[string]any{"fields": []string{"name", "web_icon"}, "order": "sequence, id"}, &roots); err != nil {
		return nil, listModulesOut{}, userError(err)
	}

	// Map each root menu to the module that defines it: ir.model.data first, web_icon ("module,path") as fallback.
	moduleOf := map[int]string{}
	ids := make([]int, len(roots))
	for i, r := range roots {
		ids[i] = r.ID
		if icon, ok := r.WebIcon.(string); ok {
			if mod, _, ok := strings.Cut(icon, ","); ok {
				moduleOf[r.ID] = mod
			}
		}
	}
	var xmlids []struct {
		Module string `json:"module"`
		ResID  int    `json:"res_id"`
	}
	if err := h.odoo.Execute(ctx, "ir.model.data", "search_read",
		[]any{[]any{[]any{"model", "=", "ir.ui.menu"}, []any{"res_id", "in", ids}}},
		map[string]any{"fields": []string{"module", "res_id"}}, &xmlids); err == nil {
		for _, x := range xmlids {
			moduleOf[x.ResID] = x.Module
		}
	}

	var names []string
	for _, m := range moduleOf {
		names = append(names, m)
	}
	details := map[string]moduleInfo{}
	var mods []struct {
		Name       string   `json:"name"`
		Shortdesc  string   `json:"shortdesc"`
		Summary    any      `json:"summary"`
		CategoryID many2one `json:"category_id"`
	}
	if len(names) > 0 {
		if err := h.odoo.Execute(ctx, "ir.module.module", "search_read",
			[]any{[]any{[]any{"name", "in", names}, []any{"state", "=", "installed"}}},
			map[string]any{"fields": []string{"name", "shortdesc", "summary", "category_id"}}, &mods); err == nil {
			for _, m := range mods {
				summary, _ := m.Summary.(string)
				details[m.Name] = moduleInfo{Module: m.Name, Title: m.Shortdesc, Summary: summary, Category: m.CategoryID.name()}
			}
		}
	}

	search := strings.ToLower(in.Search)
	out := listModulesOut{Apps: []moduleInfo{}}
	for _, r := range roots {
		info := details[moduleOf[r.ID]]
		if info.Module == "" {
			info.Module = moduleOf[r.ID]
		}
		info.App = r.Name
		if search != "" && !strings.Contains(strings.ToLower(info.App+" "+info.Module+" "+info.Title+" "+info.Summary), search) {
			continue
		}
		out.Apps = append(out.Apps, info)
	}
	return nil, out, nil
}

// ---- list_menus

type listMenusIn struct {
	App           string `json:"app,omitempty" jsonschema:"only menus of this app, e.g. 'Sales' or 'Invoicing' (see list_modules)"`
	Search        string `json:"search,omitempty" jsonschema:"keyword matched against the menu path"`
	ReportingOnly bool   `json:"reporting_only,omitempty" jsonschema:"only menus under an app's Reporting section"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max menus (default 150)"`
}

type listMenusOut struct {
	Menus     []menuEntry `json:"menus"`
	Truncated bool        `json:"truncated"`
}

func (h *handlers) listMenus(ctx context.Context, _ *mcp.CallToolRequest, in listMenusIn) (*mcp.CallToolResult, listMenusOut, error) {
	all, err := h.menus(ctx)
	if err != nil {
		return nil, listMenusOut{}, userError(err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 150
	}
	search := strings.ToLower(in.Search)
	out := listMenusOut{Menus: []menuEntry{}}
	for _, m := range all {
		if in.App != "" && !strings.EqualFold(strings.TrimSpace(m.app()), strings.TrimSpace(in.App)) {
			continue
		}
		if in.ReportingOnly && !m.isReporting() {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(m.Path), search) {
			continue
		}
		if len(out.Menus) == limit {
			out.Truncated = true
			break
		}
		out.Menus = append(out.Menus, m)
	}
	return nil, out, nil
}
