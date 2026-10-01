// Command odoo-gpt-mcp serves a read-only MCP endpoint that lets ChatGPT report on Odoo 16 data.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // users' Odoo timezones resolve even in minimal containers

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rizgust/odoo-gpt-mcp/internal/config"
	"github.com/rizgust/odoo-gpt-mcp/internal/odoo"
	"github.com/rizgust/odoo-gpt-mcp/internal/policy"
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

	client := odoo.NewClient(cfg.OdooURL, cfg.OdooDB, cfg.OdooUser, cfg.OdooAPIKey, cfg.OdooTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.OdooTimeout)
	uid, err := client.UID(ctx)
	cancel()
	if err != nil {
		return err
	}
	log.Info("connected to Odoo", "url", cfg.OdooURL, "db", cfg.OdooDB, "uid", uid)

	server := tools.NewServer(client, tools.Options{
		Policy:       &policy.Policy{Allowed: cfg.AllowedModels, Blocked: cfg.BlockedModels, MaxTextLength: cfg.MaxTextLength},
		DefaultLimit: cfg.DefaultLimit,
		MaxLimit:     cfg.MaxLimit,
	}, version)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newHandler(cfg, server, log),
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

	log.Info("serving MCP", "addr", cfg.ListenAddr, "endpoint", "/mcp/<MCP_ACCESS_TOKEN>", "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
