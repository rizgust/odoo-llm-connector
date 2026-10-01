# odoo-gpt-mcp

A read-only [MCP](https://modelcontextprotocol.io) server that lets ChatGPT answer reporting questions from a
self-hosted **Odoo 16**: "sales per salesperson last quarter", "unpaid vendor bills over 50M", "stock on hand
for product X". It covers every installed Odoo app through generic tools; no Odoo addon is required.

```
ChatGPT ──HTTPS──▶ reverse proxy / tunnel ──▶ odoo-gpt-mcp ──JSON-RPC──▶ Odoo 16
                     /mcp/<secret token>        (this repo)    /jsonrpc
```

## Tools

| Tool | Purpose | Odoo call |
|---|---|---|
| `odoo_context` | Connected user, companies, currencies, today's date in the user's timezone | `res.users`, `res.company` |
| `list_models` | Find models by keyword (`invoice` → `account.move`, `account.invoice.report`, …) | `ir.model` |
| `describe_model` | Fields, types, relations, selection values, stored or not | `fields_get` |
| `search_records` | Individual records with domain, fields, order, paging | `search_read` + `search_count` |
| `count_records` | "How many …" | `search_count` |
| `aggregate_records` | Totals/averages grouped by period, salesperson, customer, … | `read_group` |

All tools are annotated read-only, and the server never calls `create`, `write`, `unlink` or action methods.
ChatGPT is told to prefer Odoo's reporting models (`sale.report`, `account.invoice.report`, `purchase.report`,
`stock.quant`, …) and `aggregate_records` over downloading rows.

## What ChatGPT can and cannot see

1. **Odoo access rights are the real boundary.** Every call runs as one Odoo service user, so ChatGPT sees exactly
   what that user's groups and record rules allow, and **everyone who uses the connector sees the same data**.
2. **Blocked models** (default): `ir.*`, `base.*`, `bus.*`, `iap.*`, `auth.*`, `res.users.apikeys*`, `res.users.log`,
   `res.config*`, `res.partner.bank`, `fetchmail.*`, `payment.token`, `payment.provider`, `mail.mail`, `mail.alias*`.
   Narrow further with `ODOO_ALLOWED_MODELS`.
3. **Hidden fields:** binary fields (files, images) and anything whose name looks like a secret (`password`, `token`,
   `secret`, `api_key`, `totp`, `oauth*`, `signup*`, `signature`, …), including through dotted paths in domains
   like `partner_id.user_ids.password`.
4. **Size limits:** rows are capped (default 80, max 500), long text is truncated to 500 characters, and x2many ID
   lists are cut after 20 entries. Responses say when results are truncated.

## Setup

### 1. Odoo service user

1. Create an internal user, e.g. `ChatGPT Reports`. Give it **only** the groups for the data your team should be able
   to ask about, e.g. *Sales: User: All Documents*, *Accounting: Read-only*, *Inventory: User*. Leave out HR/Payroll
   unless intended.
2. Log in as that user → **Preferences → Account Security → New API Key**. Copy the key.

The API key carries that user's full rights over XML/JSON-RPC (including write). This server only reads, but the key
itself must be kept secret.

### 2. Configure

```sh
cp .env.example .env
openssl rand -hex 32   # → MCP_ACCESS_TOKEN
```

| Variable | Required | Default | |
|---|---|---|---|
| `ODOO_URL` | ✓ | | e.g. `https://odoo.example.com` (or the internal URL, e.g. `http://odoo:8069`) |
| `ODOO_DB` | ✓ | | database name |
| `ODOO_USER` | ✓ | | service user's login |
| `ODOO_API_KEY` | ✓ | | service user's API key |
| `MCP_ACCESS_TOKEN` | ✓ | | ≥ 32 chars of `[A-Za-z0-9_-]`; the secret part of the connector URL |
| `MCP_PUBLIC_HOST` | | | public hostname; when set, other `Host` headers get 403 |
| `MCP_LISTEN_ADDR` | | `:8000` | |
| `ODOO_ALLOWED_MODELS` | | all | comma-separated globs, e.g. `sale.*,account.invoice.report,stock.*` |
| `ODOO_BLOCKED_MODELS` | | see above | replaces the default list |
| `ODOO_DEFAULT_LIMIT` / `ODOO_MAX_LIMIT` | | `80` / `500` | rows/groups per call |
| `ODOO_MAX_TEXT_LENGTH` | | `500` | |
| `ODOO_TIMEOUT_SECONDS` | | `60` | per Odoo request |

### 3. Run

```sh
docker compose up -d --build        # listens on 127.0.0.1:8000
# or
go run ./cmd/odoo-gpt-mcp           # needs the env vars exported
```

The server authenticates against Odoo on startup and exits with a clear message if the credentials are wrong.
`GET /healthz` returns `{"status":"ok"}`.

### 4. Expose over HTTPS

ChatGPT only connects to public HTTPS URLs. Put a reverse proxy (nginx, Caddy, Traefik) or a Cloudflare Tunnel in
front of port 8000, and set `MCP_PUBLIC_HOST` to that hostname. Don't expose port 8000 directly.

The connector URL is:

```
https://<MCP_PUBLIC_HOST>/mcp/<MCP_ACCESS_TOKEN>
```

Any other path returns 404. Keep proxy access logs for this host off or redacted, because the token is in the path.

### 5. Add it to ChatGPT

In ChatGPT, open **Settings → Apps & Connectors**, enable **Developer mode** under *Advanced*, then create a
connector with the URL above and **No authentication**. On Business/Enterprise workspaces an admin may need to allow
custom connectors first. The exact menu names change from time to time, so check OpenAI's help center if they've moved.

Then ask, for example:

- *"Using Odoo, show sales revenue per month this year, by salesperson."*
- *"Which customers have overdue invoices, and how much does each owe?"*
- *"How many leads were created last month per source?"*

## Security notes

- The URL token is a **shared secret**: anyone who has the URL can query everything the service user can see.
  Rotate it by changing `MCP_ACCESS_TOKEN` and updating the connector.
- Per-user access (each ChatGPT user mapped to their own Odoo user and rights) would need OAuth between ChatGPT and
  this server. It is not implemented yet.

## Development

```sh
go test ./...
go vet ./...
```

```
cmd/odoo-gpt-mcp/   entrypoint, HTTP routing, Host check
internal/config/    env parsing
internal/odoo/      Odoo 16 /jsonrpc client
internal/policy/    model/field filtering, domain validation, result shaping
internal/tools/     MCP tool definitions and server instructions
```

Tests use a fake Odoo, so no Odoo instance is needed.
