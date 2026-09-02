// Package store is the Postgres system of record: repos, files, spans, their
// embeddings, and (from P4) the symbol graph.
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

	"github.com/jackc/pgx/v5"
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

// migrateLockKey is the advisory lock every migration run takes. The number is
// arbitrary; what matters is that every process migrating a given database
// agrees on it.
const migrateLockKey int64 = 0x0C0DE7241

// migrate applies every embedded migration whose name is not already recorded,
// in filename order. Names are sorted rather than globbed in directory order so
// 0010 cannot run before 0002.
//
// The run is one transaction that takes an advisory lock before it reads the
// ledger. Deciding what to apply is half the race: against a fresh database
// several processes otherwise read an empty ledger and all apply 0001. CI
// starts a new Postgres per PR and `go test ./...` runs package binaries in
// parallel, so that is the ordinary startup path there. Postgres drops the lock
// when the transaction ends, so a crashed migrator cannot wedge the next one.
//
// One transaction rather than one per migration: a half-applied schema is worse
// than none, and the lock has to span the whole run anyway.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Before the CREATE TABLE too — concurrent CREATE TABLE IF NOT EXISTS is
	// itself not safe, it trips pg_type_typname_nsp_index.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
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

	// Read through tx: on any other connection the check would sit outside the
	// lock and see a snapshot the holder is still writing.
	applied, err := appliedMigrations(ctx, tx)
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
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("%s: recording: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}

// rowQuerier is the half of *pgxpool.Pool and pgx.Tx that appliedMigrations
// needs, so the ledger can be read inside the migrating transaction.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func appliedMigrations(ctx context.Context, q rowQuerier) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT name FROM schema_migrations`)
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
