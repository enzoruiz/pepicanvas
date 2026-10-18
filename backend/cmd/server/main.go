// Command server is the PepiCanvas backend: it migrates PostgreSQL, serves
// the HTTP API, and serves the built frontend from one origin.
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
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/password"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
)

const (
	defaultListenAddr = "127.0.0.1:8080"
	startupTimeout    = 15 * time.Second
	// shutdownTimeout bounds HTTP draining; full process shutdown belongs to TE-016.
	shutdownTimeout = 5 * time.Second
)

func main() {
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, logger); err != nil {
		logger.Error("server stopped", slog.Any("error", err))
		os.Exit(1)
	}
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

	store := postgres.NewAccountStore(pool)
	accounts, err := access.NewService(store, store, password.Argon2id{})
	if err != nil {
		return err
	}
	api, err := httpapi.New(httpapi.Config{
		Accounts:      accounts,
		Memberships:   store,
		Canvases:      postgres.NewCanvasStore(pool),
		AllowedOrigin: origin,
		Logger:        logger,
		Limits:        httpapi.DefaultLimits,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	if dir := getenv("PEPICANVAS_STATIC_DIR"); dir != "" {
		mux.Handle("/", staticFiles(dir))
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
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	logger.Info("listening", slog.String("addr", listener.Addr().String()))

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("stopped")
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
