package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// cacheTTL bounds how long user/group/access information is reused before re-asking Odoo,
// so privilege changes made in Odoo show up without a restart.
const cacheTTL = 10 * time.Minute

type cached[T any] struct {
	val T
	at  time.Time
}

type userInfo struct {
	Name       string
	TZ         string
	Lang       string
	Company    string
	CompanyIDs []int
	GroupIDs   []int
	loc        *time.Location
}

// user returns the connected Odoo user's profile and groups.
func (h *handlers) user(ctx context.Context) (*userInfo, error) {
	h.mu.Lock()
	c := h.userCache
	h.mu.Unlock()
	if c != nil && h.opts.Now().Sub(c.at) < cacheTTL {
		return c.val, nil
	}

	uid, err := h.odoo.UID(ctx)
	if err != nil {
		return nil, err
	}
	var users []struct {
		Name       string   `json:"name"`
		TZ         any      `json:"tz"`
		Lang       any      `json:"lang"`
		CompanyID  many2one `json:"company_id"`
		CompanyIDs []int    `json:"company_ids"`
		GroupIDs   []int    `json:"groups_id"`
	}
	if err := h.odoo.Execute(ctx, "res.users", "read", []any{[]int{uid}},
		map[string]any{"fields": []string{"name", "tz", "lang", "company_id", "company_ids", "groups_id"}}, &users); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("connector user %d not found in Odoo", uid)
	}
	u := users[0]
	info := &userInfo{Name: u.Name, TZ: "UTC", Company: u.CompanyID.name(), CompanyIDs: u.CompanyIDs, GroupIDs: u.GroupIDs, loc: time.UTC}
	if tz, ok := u.TZ.(string); ok && tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			info.TZ, info.loc = tz, loc
		}
	}
	if lang, ok := u.Lang.(string); ok {
		info.Lang = lang
	}

	h.mu.Lock()
	h.userCache = &cached[*userInfo]{info, h.opts.Now()}
	h.mu.Unlock()
	return info, nil
}

// location is the user's timezone, falling back to UTC if Odoo can't be asked.
func (h *handlers) location(ctx context.Context) *time.Location {
	if u, err := h.user(ctx); err == nil {
		return u.loc
	}
	return time.UTC
}

func (h *handlers) today(ctx context.Context) string {
	return h.opts.Now().In(h.location(ctx)).Format(time.DateOnly)
}

// rpcContext is passed to read_group so datetime groupings ("date:month") follow the
// user's timezone instead of UTC.
func (h *handlers) rpcContext(ctx context.Context) map[string]any {
	c := map[string]any{}
	if u, err := h.user(ctx); err == nil {
		c["tz"] = u.TZ
		if u.Lang != "" {
			c["lang"] = u.Lang
		}
	}
	return c
}

// canRead reports whether the connected user has read access to model (ACLs; record rules
// still apply per query). Unknown or uninstalled models are not readable.
func (h *handlers) canRead(ctx context.Context, model string) bool {
	h.mu.Lock()
	c, ok := h.accessCache[model]
	h.mu.Unlock()
	if ok && h.opts.Now().Sub(c.at) < cacheTTL {
		return c.val
	}
	var allowed bool
	err := h.odoo.Execute(ctx, model, "check_access_rights", []any{"read"}, map[string]any{"raise_exception": false}, &allowed)
	if err != nil {
		if ctx.Err() != nil {
			return false // don't cache a cancelled request
		}
		allowed = false
	}
	h.mu.Lock()
	h.accessCache[model] = cached[bool]{allowed, h.opts.Now()}
	h.mu.Unlock()
	return allowed
}

// readable filters models down to those the user can read, checking in parallel.
func (h *handlers) readable(ctx context.Context, models []string) map[string]bool {
	out := make(map[string]bool, len(models))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, m := range models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			ok := h.canRead(ctx, m)
			<-sem
			mu.Lock()
			out[m] = ok
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// privileges returns the user's group names that describe application rights,
// e.g. "Sales / User: All Documents", skipping technical groups.
func (h *handlers) privileges(ctx context.Context, u *userInfo) ([]string, error) {
	var groups []struct {
		FullName   string   `json:"full_name"`
		CategoryID many2one `json:"category_id"`
	}
	if err := h.odoo.Execute(ctx, "res.groups", "read", []any{u.GroupIDs},
		map[string]any{"fields": []string{"full_name", "category_id"}}, &groups); err != nil {
		return nil, err
	}
	var out []string
	for _, g := range groups {
		category := g.CategoryID.name()
		if category == "" || category == "Hidden" || strings.HasPrefix(category, "Technical") || category == "Extra Rights" {
			continue
		}
		out = append(out, g.FullName)
	}
	slices.Sort(out)
	return out, nil
}

// keepWarm rebuilds the expensive caches shortly before they expire, for the life of the process.
func (h *handlers) keepWarm() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		h.mu.Lock()
		h.userCache, h.menuCache, h.printableCache, h.accessCache = nil, nil, nil, map[string]cached[bool]{}
		h.mu.Unlock()
		var models []string
		for i := range h.opts.Catalog.Reports {
			r := &h.opts.Catalog.Reports[i]
			models = append(models, r.Model)
			h.reportProblem(ctx, r)
		}
		h.readable(ctx, models)
		h.menus(ctx)
		h.printable(ctx)
		cancel()
		time.Sleep(cacheTTL - time.Minute)
	}
}
