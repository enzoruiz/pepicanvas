// Command server is the PepiCanvas backend: it migrates PostgreSQL, serves
// the HTTP API, and serves the built frontend from one origin. In the same
// process it runs the outbox publisher, which delivers committed canvas
// mutations to the realtime hub (TE-012).
//
// Usage:
//
//	server                         serve the API and the frontend
//	server grant-operator <email>  give the account of email the operator
//	                               role (US-002); granting again is a no-op
//
// grant-operator reads only PEPICANVAS_DATABASE_URL, applies pending
// migrations, prints "operator granted: <account_id>", and exits 0. It exits
// 1 when no account has the email or the database fails, and 2 on bad usage.
//
// Configuration (environment):
//
//	PEPICANVAS_DATABASE_URL    PostgreSQL connection string (required)
//	PEPICANVAS_ALLOWED_ORIGIN  exact editor origin, scheme://host[:port] (required)
//	PEPICANVAS_LISTEN_ADDR     listen address (default 127.0.0.1:8080)
//	PEPICANVAS_STATIC_DIR      built frontend directory (optional)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/password"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

const (
	defaultListenAddr = "127.0.0.1:8080"
	startupTimeout    = 15 * time.Second
	// shutdownTimeout bounds HTTP draining and closing the outbox publisher
	// and the realtime hub; full process shutdown belongs to TE-016.
	shutdownTimeout = 5 * time.Second
	// outboxOperationPrefix starts the operation ID of every command
	// outbox.HubSink submits (outbox-<record ID>).
	outboxOperationPrefix = "outbox-"
)

func main() {
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "grant-operator" {
		code := grantOperator(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr, logger)
		stop()
		os.Exit(code)
	}
	if err := run(ctx, os.Getenv, logger); err != nil {
		logger.Error("server stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

// grantOperator runs the grant-operator subcommand and returns the exit code.
// Errors go through the redacting logger, which never sees the DSN.
func grantOperator(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, logger *slog.Logger) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: server grant-operator <email>")
		return 2
	}
	dsn := getenv("PEPICANVAS_DATABASE_URL")
	if dsn == "" {
		logger.Error("grant operator failed", slog.String("error", "PEPICANVAS_DATABASE_URL is required"))
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		logger.Error("grant operator failed", slog.Any("error", err))
		return 1
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		logger.Error("grant operator failed", slog.Any("error", err))
		return 1
	}
	account, err := postgres.NewSubscriptionStore(pool).GrantOperator(ctx, args[0])
	if errors.Is(err, subscription.ErrAccountNotFound) {
		logger.Error("grant operator failed", slog.String("error", "no account has that email"))
		return 1
	}
	if err != nil {
		logger.Error("grant operator failed", slog.Any("error", err))
		return 1
	}
	fmt.Fprintf(stdout, "operator granted: %s\n", account)
	return 0
}

func run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	dsn := getenv("PEPICANVAS_DATABASE_URL")
	if dsn == "" {
		return errors.New("PEPICANVAS_DATABASE_URL is required")
	}
	origin := getenv("PEPICANVAS_ALLOWED_ORIGIN")
	if origin == "" {
		return errors.New("PEPICANVAS_ALLOWED_ORIGIN is required")
	}
	listenAddr := getenv("PEPICANVAS_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	// An origin that would weaken the ingress controls stops the process
	// before any dependency is opened or any request is accepted.
	in, err := ingress.New(ingress.Config{AllowedOrigins: []string{origin}, Limits: ingress.DefaultLimits, Logger: logger})
	if err != nil {
		return err
	}

	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	pool, err := postgres.Open(startCtx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(startCtx, pool); err != nil {
		return err
	}

	// The hub only receives what the outbox publisher delivers: mutations
	// PostgreSQL already committed after TE-001 authorized them. No client
	// submits to it yet (US-017), so it refuses every other command.
	hub, err := realtime.NewHub(realtime.Config{Authorize: authorizeOutbox})
	if err != nil {
		return err
	}
	publisher, err := outbox.NewPublisher(outbox.Config{
		Store:  postgres.NewOutboxStore(pool),
		Sink:   outbox.NewHubSink(hub),
		Logger: logger,
	})
	if err != nil {
		return err
	}

	store := postgres.NewAccountStore(pool)
	accounts, err := access.NewService(store, store, password.Argon2id{})
	if err != nil {
		return err
	}
	api, err := httpapi.New(httpapi.Config{
		Accounts: accounts,
		// Activation writes the memberships and tenants rows MembershipOf reads.
		Memberships:   store,
		Canvases:      postgres.NewCanvasStore(pool, postgres.WithCommitted(func(canvas.Key) { publisher.Notify() })),
		Subscriptions: subscription.NewService(postgres.NewSubscriptionStore(pool)),
		Ingress:       in,
		Logger:        logger,
		Limits:        httpapi.DefaultLimits,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	if dir := getenv("PEPICANVAS_STATIC_DIR"); dir != "" {
		mux.Handle("/", in.Static(staticFiles(dir)))
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// Records left pending by an earlier process are delivered on the first pass.
	if err := publisher.Start(); err != nil {
		_ = listener.Close()
		return err
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	logger.Info("listening", slog.String("addr", listener.Addr().String()))

	var serveErr error
	select {
	case err := <-served:
		serveErr = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if serveErr == nil {
		if err := server.Shutdown(shutdownCtx); err != nil {
			serveErr = fmt.Errorf("shutdown: %w", err)
		}
	}
	// No request can commit any more, so the publisher stops after its last
	// delivery and the hub after the commands it queued. Undelivered records
	// stay pending for the next start.
	if err := publisher.Close(shutdownCtx); err != nil {
		logger.Error("outbox publisher close failed", slog.String("error", err.Error()))
	}
	if err := hub.Close(shutdownCtx); err != nil {
		logger.Error("realtime hub close failed", slog.String("error", err.Error()))
	}
	if serveErr != nil {
		return serveErr
	}
	logger.Info("stopped")
	return nil
}

// authorizeOutbox accepts only commands submitted by outbox.HubSink, whose
// mutations were authorized before they committed.
func authorizeOutbox(_ context.Context, command realtime.Command) error {
	if !strings.HasPrefix(command.OperationID, outboxOperationPrefix) {
		return access.ErrAccessDenied
	}
	return nil
}

// staticFiles serves the built frontend. HTML documents are never cached, so
// every page load asks the server again (TE-003, US-001 S4).
func staticFiles(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/editor/", http.StatusFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/") {
			// Serve directory index pages only; never list a directory.
			index := filepath.Join(dir, filepath.FromSlash(path.Clean(r.URL.Path)), "index.html")
			if info, err := os.Stat(index); err != nil || !info.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/") || path.Ext(r.URL.Path) == ".html" {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
		}
		files.ServeHTTP(w, r)
	})
}
