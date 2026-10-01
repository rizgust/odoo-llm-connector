// Package odoo is a minimal client for Odoo 16's external JSON-RPC API (/jsonrpc).
package odoo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Executor is the subset of Odoo the MCP tools need; tests substitute a fake.
type Executor interface {
	// Execute calls model.method via execute_kw and decodes the result into out.
	Execute(ctx context.Context, model, method string, args []any, kwargs map[string]any, out any) error
	UID(ctx context.Context) (int, error)
	ServerVersion(ctx context.Context) (string, error)
}

// Error is a failure reported by Odoo (access error, validation error, bad domain, ...).
type Error struct {
	Name    string // e.g. odoo.exceptions.AccessError
	Message string
}

func (e *Error) Error() string { return e.Message }

type Client struct {
	url    string
	db     string
	user   string
	apiKey string
	http   *http.Client

	mu  sync.Mutex
	uid int
	seq atomic.Int64
}

func NewClient(url, db, user, apiKey string, timeout time.Duration) *Client {
	return &Client{url: url, db: db, user: user, apiKey: apiKey, http: &http.Client{Timeout: timeout}}
}

type rpcRequest struct {
	JSONRPC string    `json:"jsonrpc"`
	Method  string    `json:"method"`
	Params  rpcParams `json:"params"`
	ID      int64     `json:"id"`
}

type rpcParams struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Args    []any  `json:"args"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
		Data    struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

func (c *Client) call(ctx context.Context, service, method string, args []any, out any) error {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  "call",
		Params:  rpcParams{Service: service, Method: method, Args: args},
		ID:      c.seq.Add(1),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/jsonrpc", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Odoo at %s: %w", c.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("odoo returned HTTP %d", resp.StatusCode)
	}

	var rpc rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return fmt.Errorf("decoding Odoo response: %w", err)
	}
	if rpc.Error != nil {
		msg := rpc.Error.Data.Message
		if msg == "" {
			msg = rpc.Error.Message
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &Error{Name: rpc.Error.Data.Name, Message: msg}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rpc.Result, out)
}

func (c *Client) UID(ctx context.Context) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.uid != 0 {
		return c.uid, nil
	}
	// authenticate returns false on bad credentials, so decode loosely.
	var res any
	if err := c.call(ctx, "common", "authenticate", []any{c.db, c.user, c.apiKey, map[string]any{}}, &res); err != nil {
		return 0, err
	}
	uid, ok := res.(float64)
	if !ok || uid == 0 {
		return 0, fmt.Errorf("odoo authentication failed: check ODOO_DB, ODOO_USER and ODOO_API_KEY")
	}
	c.uid = int(uid)
	return c.uid, nil
}

func (c *Client) Execute(ctx context.Context, model, method string, args []any, kwargs map[string]any, out any) error {
	uid, err := c.UID(ctx)
	if err != nil {
		return err
	}
	if args == nil {
		args = []any{}
	}
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	return c.call(ctx, "object", "execute_kw", []any{c.db, uid, c.apiKey, model, method, args, kwargs}, out)
}

func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var v struct {
		ServerVersion string `json:"server_version"`
	}
	err := c.call(ctx, "common", "version", []any{}, &v)
	return v.ServerVersion, err
}
