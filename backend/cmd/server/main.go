// Command server is the PepiCanvas backend: it migrates PostgreSQL, serves
// the HTTP API, and serves the built frontend from one origin. In the same
// process it runs the outbox publisher, which delivers committed canvas
// mutations to the realtime hub (TE-012), the live-connection registry
// (TE-002), and the relay that applies each confirmed access cut to it
// (US-003); cuts a stopped process left pending are applied at start, before
// the server listens. The canvas storage quota is always served (US-016);
// uploads need object storage and contained inspection, and with them the
// asset reconciler runs (TE-013). Without any media variable, uploads answer
// media_unavailable. Shutdown stops HTTP, then the publisher and the hub,
// then the reconciler, then the relay, then the registry, then the tracer.
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
//	PEPICANVAS_TRACE_FILE      owner-only file of exported spans, one JSON
//	                           object per line (optional; unset keeps export
//	                           disabled and local logs correlated)
//
// Media storage (optional; setting any of the first six requires the five
// without a default, or the server refuses to start):
//
//	PEPICANVAS_S3_ENDPOINT        S3 API host[:port], without scheme
//	PEPICANVAS_S3_BUCKET          existing bucket of the media objects
//	PEPICANVAS_S3_ACCESS_KEY      S3 access key (secret, never logged)
//	PEPICANVAS_S3_SECRET_KEY      S3 secret key (secret, never logged)
//	PEPICANVAS_S3_SECURE          use HTTPS, true or false (default true)
//	PEPICANVAS_MEDIA_CGROUP_ROOT  delegated cgroup v2 subtree that contains
//	                              every inspection
//	PEPICANVAS_FFPROBE_PATH       absolute ffprobe path (default /usr/bin/ffprobe)
//	PEPICANVAS_FFMPEG_PATH        absolute ffmpeg path (default /usr/bin/ffmpeg)
//	PEPICANVAS_MEDIA_TEMP_DIR     absolute directory that stages inspected
//	                              uploads (default the system temp directory)
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
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/password"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/tracing"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

const (
	defaultListenAddr = "127.0.0.1:8080"
	startupTimeout    = 15 * time.Second
	// shutdownTimeout bounds HTTP draining and closing the outbox publisher,
	// the realtime hub, and the reconciler, and then the relay and registry
	// stop; full process shutdown belongs to TE-016.
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
	// So does a partial or invalid media configuration.
	mediaConfig, err := mediaFromEnv(getenv)
	if err != nil {
		return err
	}
	var inspector media.Inspector
	if mediaConfig != nil {
		if inspector, err = mediaConfig.inspector(); err != nil {
			return err
		}
	}
	// So does an unusable trace file. The tracer stops last, after the hub.
	tracer, stopTracer, err := tracerFromEnv(getenv, logger)
	if err != nil {
		return err
	}
	defer stopTracer()

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
		Sink:   tracing.Sink(outbox.NewHubSink(tracing.Actor(hub, tracer)), tracer),
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
	registry, err := revocation.NewRegistry(revocation.Config{Logger: logger})
	if err != nil {
		return err
	}
	subscriptions := postgres.NewSubscriptionStore(pool)
	invitations := postgres.NewInvitationStore(pool)
	// Every committed usage change wrote an outbox record (CTRL-CONS-01).
	mediaStore := postgres.NewMediaStore(pool, postgres.WithMediaCommitted(func(media.CanvasKey) { publisher.Notify() }))
	var (
		storage    media.ObjectStore
		reconciler *media.Reconciler
	)
	if mediaConfig == nil {
		logger.Info("media storage disabled")
	} else {
		objects, err := s3.Open(startCtx, mediaConfig.storage)
		if err != nil {
			return fmt.Errorf("media storage: %w", err)
		}
		storage = objects
		if reconciler, err = media.NewReconciler(media.ReconcilerConfig{Store: mediaStore, Storage: objects}); err != nil {
			return err
		}
	}
	relay, err := subscription.NewCutRelay(subscription.RelayConfig{
		Store:     subscriptions,
		Cutter:    registry,
		Logger:    logger,
		Correlate: correlate,
	})
	if err != nil {
		return err
	}
	api, err := httpapi.New(httpapi.Config{
		Accounts: accounts,
		// Activation writes the memberships and tenants rows MembershipOf reads.
		Memberships: store,
		// Canvas mutations are rejected for a tenant that is not active.
		Canvases:      postgres.NewCanvasStore(pool, postgres.WithCommitted(func(canvas.Key) { publisher.Notify() })),
		Subscriptions: subscription.NewService(subscriptions, subscription.WithCommitted(relay.Notify)),
		Tenants:       subscriptions,
		// The store clock keeps PostgreSQL's precision, so replayed answers match.
		Invitations: invitation.NewService(invitations, invitation.WithClock(invitations.Now)),
		Media:       mediaStore,
		Storage:     storage,
		Inspector:   inspector,
		Ingress:     in,
		Logger:      logger,
		Limits:      httpapi.DefaultLimits,
		Tracer:      tracer,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	if dir := getenv("PEPICANVAS_STATIC_DIR"); dir != "" {
		mux.Handle("/", in.Static(staticFiles(dir)))
	}

	if err := relay.Start(); err != nil {
		return err
	}
	// Runs after the HTTP server drained, so no deactivation commits a cut
	// the stopped relay would leave for the next start, and before the pool
	// closes.
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelShutdown()
		if err := relay.Close(shutdownCtx); err != nil {
			logger.Error("cut relay stopped", slog.Any("error", err))
		}
		if err := registry.Close(shutdownCtx); err != nil {
			logger.Error("registry stopped", slog.Any("error", err))
		}
	}()
	if reconciler != nil {
		// Passes resume from the states PostgreSQL persisted, so assets an
		// earlier process left half done converge before uploads arrive.
		if err := reconciler.Start(); err != nil {
			return err
		}
		// Stops a reconciler a failed start leaves running; Close is
		// idempotent, so after the shutdown below it returns at once.
		defer func() {
			closeCtx, cancelClose := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancelClose()
			if err := reconciler.Close(closeCtx); err != nil {
				logger.Error("media reconciler close failed", slog.String("error", err.Error()))
			}
		}()
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
	// Interrupted passes resume from their persisted states on the next start.
	if reconciler != nil {
		if err := reconciler.Close(shutdownCtx); err != nil {
			logger.Error("media reconciler close failed", slog.String("error", err.Error()))
		}
	}
	if serveErr != nil {
		return serveErr
	}
	logger.Info("stopped")
	return nil
}

// correlate carries a stored cut's correlation ID into the registry's context,
// so its logs relate to the deactivation request. A missing or malformed ID
// is dropped.
func correlate(ctx context.Context, id string) context.Context {
	parsed, err := correlation.Parse(id)
	if err != nil {
		return ctx
	}
	return correlation.WithID(ctx, parsed)
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
