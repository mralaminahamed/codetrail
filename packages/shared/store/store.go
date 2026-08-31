// Package store is the Postgres system of record: repos, files, spans, their
// embeddings, and (from P3) the symbol graph.
//
// One datastore, not two. The graph questions this product answers — who calls
// this, what does it import — are relational, and pgvector puts the embeddings
// in the same database, so a similarity search and a graph hop are one query
// against one consistent snapshot rather than two systems that can disagree
// about what is indexed.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// EmbeddingDim is the vector width the schema is built for. It is a constant
// rather than configuration because an ANN index needs a fixed dimension: see
// migrations/0001_init.sql. New refuses to start against an embedder of a
// different width instead of writing vectors that would rank nonsense
// confidently.
const EmbeddingDim = 768

// ErrDimMismatch is returned when the configured embedder does not produce
// EmbeddingDim-wide vectors.
var ErrDimMismatch = errors.New("store: embedder dimension does not match the schema")

type Store struct{ pool *pgxpool.Pool }

// New connects, verifies the server is reachable, and applies any migration
// that has not run. Migrations run inside the connection rather than as a
// separate deploy step because the whole product is one binary plus a database:
// a schema that lags the code is a failure mode with no upside here.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// CheckDim reports whether an embedder of width dim can write to this schema.
// Callers run it at startup: a mismatch is a configuration error that would
// otherwise surface as silently bad retrieval.
func CheckDim(dim int) error {
	if dim != EmbeddingDim {
		return fmt.Errorf("%w: schema is vector(%d), embedder produces %d", ErrDimMismatch, EmbeddingDim, dim)
	}
	return nil
}

// Ping reports whether Postgres is still reachable. New pings once at startup;
// this is the same check for a process that has been running a while.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Close() { s.pool.Close() }

// Pool exposes the connection pool to the packages that own their own queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// migrate applies every embedded migration whose name is not already recorded,
// in filename order, each in its own transaction. Names are sorted rather than
// globbed in directory order so 0010 cannot run before 0002.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: recording: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("%s: commit: %w", name, err)
		}
	}
	return nil
}

func (s *Store) appliedMigrations(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// MigrationNames is the ordered set the schema is built from, exported so a
// test can pin it without reaching into the embedded filesystem.
func MigrationNames() ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
