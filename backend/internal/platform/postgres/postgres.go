// Package postgres adapts the domain ports to PostgreSQL through pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/enzoruiz/pepicanvas/backend/migrations"
)

// ErrUnavailable reports that the database could not be configured or reached.
// The cause is not wrapped because connection errors can echo the DSN.
var ErrUnavailable = errors.New("postgres: database unavailable")

// Open connects a pool and verifies the server answers.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid configuration", ErrUnavailable)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("%w: pool rejected configuration", ErrUnavailable)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w: ping failed", ErrUnavailable)
	}
	return pool, nil
}

// Migrate applies every pending embedded migration. Applied migrations are
// skipped, so calling it on every start is safe.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("postgres: load migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("postgres: apply migrations: %w", err)
	}
	return nil
}
