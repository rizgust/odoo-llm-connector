// Command odoo-gpt-mcp serves a read-only MCP endpoint that lets ChatGPT report on Odoo 16 data.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // users' Odoo timezones resolve even in minimal containers

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rizgust/odoo-gpt-mcp/internal/config"
	"github.com/rizgust/odoo-gpt-mcp/internal/oauth"
	"github.com/rizgust/odoo-gpt-mcp/internal/odoo"
	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
	"github.com/rizgust/odoo-gpt-mcp/internal/reports"
	"github.com/rizgust/odoo-gpt-mcp/internal/tools"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	catalog, err := reports.Load(cfg.ReportsFile)
	if err != nil {
		return err
	}
	pol := &policy.Policy{Allowed: cfg.AllowedModels, Blocked: cfg.BlockedModels, MaxTextLength: cfg.MaxTextLength}
	opts := tools.Options{Policy: pol, Catalog: catalog, DefaultLimit: cfg.DefaultLimit, MaxLimit: cfg.MaxLimit}

	// The service account is required for shared mode; with OAuth it only runs the startup report check.
	var client *odoo.Client
	if cfg.OdooUser != "" && cfg.OdooAPIKey != "" {
		client = odoo.NewClient(cfg.OdooURL, cfg.OdooDB, cfg.OdooUser, cfg.OdooAPIKey, cfg.OdooTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), cfg.OdooTimeout)
		uid, err := client.UID(ctx)
		cancel()
		if err != nil {
			return err
		}
		log.Info("connected to Odoo", "url", cfg.OdooURL, "db", cfg.OdooDB, "uid", uid)
		checkReports(log, client, pol, catalog, cfg.OdooTimeout)
	} else {
		log.Info("no ODOO_USER/ODOO_API_KEY: skipping startup report check", "url", cfg.OdooURL, "db", cfg.OdooDB)
	}

	var handler http.Handler
	if cfg.OAuth() {
		as, err := oauth.New(cfg.PublicURL, cfg.OAuthSecret, oauth.OdooAuthenticator(cfg.OdooURL, cfg.OdooDB, cfg.OdooTimeout))
		if err != nil {
			return err
		}
		users := &userServers{cfg: cfg, opts: opts, servers: map[string]*mcp.Server{}}
		handler = newOAuthHandler(cfg, as, users.get, log)
		log.Info("per-user sign-in enabled", "endpoint", cfg.PublicURL+"/mcp")
	} else {
		opts.KeepWarm = true
		handler = newHandler(cfg, tools.NewServer(client, opts, version), log)
		log.Info("shared-account mode", "endpoint", "/mcp/<MCP_ACCESS_TOKEN>")
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdown, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdown.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Info("serving MCP", "addr", cfg.ListenAddr, "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// userServers keeps one MCP server per signed-in Odoo identity, so every tool call runs
// with that user's own Odoo session, access rights and caches.
type userServers struct {
	cfg  *config.Config
	opts tools.Options

	mu      sync.Mutex
	servers map[string]*mcp.Server
}

func (u *userServers) get(id oauth.Identity) *mcp.Server {
	sum := sha256.Sum256([]byte(id.Login + "\x00" + id.APIKey))
	key := hex.EncodeToString(sum[:])
	u.mu.Lock()
	defer u.mu.Unlock()
	if s, ok := u.servers[key]; ok {
		return s
	}
	client := odoo.NewClient(u.cfg.OdooURL, u.cfg.OdooDB, id.Login, id.APIKey, u.cfg.OdooTimeout)
	s := tools.NewServer(client, u.opts, version)
	u.servers[key] = s
	return s
}

// newOAuthHandler routes the OAuth endpoints, /healthz, and the bearer-protected /mcp endpoint.
func newOAuthHandler(cfg *config.Config, as *oauth.Server, serverFor func(oauth.Identity) *mcp.Server, log *slog.Logger) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		id, ok := oauth.IdentityFrom(r)
		if !ok {
			return nil
		}
		return serverFor(id)
	}, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		Logger:                     log,
		DisableLocalhostProtection: true, // checkHost below pins the public hostname instead
	})
	protected := auth.RequireBearerToken(as.Verifier(), &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: as.ResourceMetadataURL(),
	})(mcpHandler)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	as.Register(mux, "/mcp")
	mux.Handle("/mcp", checkHost(cfg.PublicHost, protected))
	return mux
}

// checkReports logs which catalog reports work against this database, so a wrong field
// name or a missing module shows up at startup instead of in a ChatGPT conversation.
func checkReports(log *slog.Logger, exec odoo.Executor, pol *policy.Policy, catalog *reports.Catalog, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var ok []string
	for i := range catalog.Reports {
		r := &catalog.Reports[i]
		if reason := tools.CheckReport(ctx, exec, pol, r); reason != "" {
			log.Warn("report unavailable", "report", r.Name, "reason", reason)
			continue
		}
		ok = append(ok, r.Name)
	}
	log.Info("report catalog loaded", "available", strings.Join(ok, ","), "total", len(catalog.Reports))
}

// newHandler routes /healthz and the token-guarded MCP endpoint.
func newHandler(cfg *config.Config, server *mcp.Server, log *slog.Logger) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		Logger:       log,
		// A reverse proxy or tunnel on the same host connects via 127.0.0.1 with the public Host
		// header, which the SDK's localhost protection would reject; checkHost replaces it.
		DisableLocalhostProtection: cfg.PublicHost != "",
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	// ChatGPT connectors can't send a static header, so the secret lives in the URL path.
	// Wrong paths fall through to the mux's 404.
	mux.Handle("/mcp/"+cfg.AccessToken, checkHost(cfg.PublicHost, mcpHandler))
	return mux
}

// checkHost rejects requests whose Host header isn't the configured public hostname
// (DNS rebinding protection). An empty publicHost disables the check.
func checkHost(publicHost string, next http.Handler) http.Handler {
	if publicHost == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != publicHost && host != "localhost" && host != "127.0.0.1" {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
