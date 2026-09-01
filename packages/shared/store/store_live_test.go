//go:build live

package store

import (
	"context"
	"os"
	"testing"
)

// dsn is the database these tests run against. They create and drop their own
// schema objects, so point them at a throwaway database.
func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("DATABASE_URL")
	if v == "" {
		// A skipped live suite prints the same "ok" as one that ran, so in CI a
		// dropped DATABASE_URL would look green with zero live coverage.
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_URL unset in CI — the live suite must never silently skip")
		}
		t.Skip("set DATABASE_URL to run")
	}
	return v
}

// Migrations have to be safe to run against a database that already has them.
// Every restart runs them, so "applies twice" is not an edge case, it is the
// normal path.
func TestMigrationsAreIdempotentLive(t *testing.T) {
	ctx := context.Background()
	first, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	names, err := MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	applied, err := first.appliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if !applied[n] {
			t.Fatalf("%s did not run", n)
		}
	}
	first.Close()

	// A second connection re-runs migrate(). If any migration is not guarded,
	// this is where "relation already exists" surfaces.
	second, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatalf("re-running migrations failed: %v", err)
	}
	defer second.Close()

	after, err := second.appliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(applied) {
		t.Fatalf("migration count changed on a second run: %d -> %d", len(applied), len(after))
	}
}

// The schema the code assumes has to be the schema Postgres has. These are the
// columns every later phase reads and writes.
func TestSchemaShapeLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for table, want := range map[string][]string{
		"repos": {"id", "remote", "ref", "commit_sha", "indexed_at"},
		"files": {"id", "repo_id", "path", "blob", "lang", "lines"},
		"spans": {"id", "repo_id", "file_id", "path", "kind", "symbol",
			"start_line", "end_line", "text", "digest", "embed_model", "embed_dim", "embedding"},
	} {
		have := map[string]bool{}
		rows, err := s.pool.Query(ctx,
			`SELECT column_name FROM information_schema.columns WHERE table_name = $1`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			have[c] = true
		}
		rows.Close()
		if len(have) == 0 {
			t.Fatalf("table %q does not exist", table)
		}
		for _, col := range want {
			if !have[col] {
				t.Errorf("%s.%s is missing", table, col)
			}
		}
	}
}

// The embedding column must be exactly the width the code writes, and the ANN
// index must exist — an unindexed vector column works, slowly, and silently.
func TestVectorColumnAndIndexLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var dim int
	err = s.pool.QueryRow(ctx, `
		SELECT a.atttypmod
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'spans' AND a.attname = 'embedding'`).Scan(&dim)
	if err != nil {
		t.Fatal(err)
	}
	if dim != EmbeddingDim {
		t.Fatalf("spans.embedding is vector(%d), EmbeddingDim is %d", dim, EmbeddingDim)
	}

	var idx string
	err = s.pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'spans' AND indexname = 'spans_embedding_idx'`).Scan(&idx)
	if err != nil {
		t.Fatalf("the ANN index is missing: %v", err)
	}
	// Cosine, matching how the retriever will score. A euclidean index would
	// silently not be used by a cosine query.
	if !contains(idx, "hnsw") || !contains(idx, "vector_cosine_ops") {
		t.Fatalf("want an hnsw cosine index, got: %s", idx)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// Every column in repos is written by something. status was not: 0004 added
// it, nothing set it, and it read as a lifecycle the code does not have.
func TestReposHasNoUnwrittenStatusColumn(t *testing.T) {
	st, err := New(context.Background(), dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'repos' AND column_name = 'status'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("repos.status is back, and nothing writes it")
	}
}
