//go:build live

package store

import (
	"context"
	"os"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
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

// f1813be re-keyed repos.id without a migration, so a database that already
// held a row keyed the old way had two identities for one repository. PutRepo
// arbitrates ON CONFLICT (id), missed the stale row, and failed on the
// (remote, commit_sha) uniqueness — three attempts, SQLSTATE 23505 every time,
// and no retry could clear it. 0007 drops that constraint.
func TestPreFixRepoRowDoesNotWedgePutRepoLive(t *testing.T) {
	ctx := context.Background()
	const remote = "https://github.com/octocat/Spoon-Knife"
	const key = "github.com/octocat/spoon-knife"
	const commit = "d0dd1f6b1f7e4dfd44dd54e4c2a4c1b9d5e0aa11"
	// Pre-fix code hashed the submitted URL; RepoID's argument changed, not its
	// shape, so the old id is this same call with the URL in it.
	old, want := RepoID(remote, commit), RepoID(key, commit)
	if old == want {
		t.Fatal("the two schemes agree, so this test proves nothing")
	}
	s := fresh(t, old, want)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO repos (id, remote, ref, commit_sha) VALUES ($1, $2, 'main', $3)`,
		old, remote, commit); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepo(ctx, models.Repo{ID: want, Remote: remote, Ref: "main", Commit: commit}, nil); err != nil {
		t.Fatalf("a pre-fix row still blocks the write: %v", err)
	}
}

// The other half: dropping the constraint alone would leave the stale row
// beside the new one, which is the duplication f1813be set out to remove. The
// body is re-run here rather than trusted, because migrate() applies it once
// and the recorded name would make a broken statement look applied.
//
// In one rolled-back transaction: the jobs package's live tests share this
// database, run in their own binary, and truncate jobs on every test, so a
// committed row of ours would be theirs to delete and theirs to trip over.
func TestMigration0007ClearsTheCorpusAndSparesJobsLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)
	body, err := migrationFS.ReadFile("migrations/0007_repo_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	for _, stmt := range []string{
		`INSERT INTO repos (id, remote, ref, commit_sha) VALUES ('m7', 'https://github.com/a/m7', 'main', 'c0ffee')`,
		`INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ('m7-f', 'm7', 'a.go', '', 'go', 1)`,
		`INSERT INTO spans (id, repo_id, file_id, path, kind, start_line, end_line, text, digest, embed_model, embed_dim)
			VALUES ('m7-s', 'm7', 'm7-f', 'a.go', 'func', 1, 1, 'x', 'd', 'm', 768)`,
		`INSERT INTO jobs (id, remote, ref, status) VALUES ('m7-job', 'https://github.com/a/m7', 'main', 'done')`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	// Twice, and the second time against the empty table it just left: a
	// migration that is not safe to re-run is a migration that runs once.
	for i := range 2 {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		for _, tc := range []struct {
			what, from string
			want       int
		}{
			{"repos", "repos", 0},
			{"files", "files", 0},
			{"spans", "spans", 0},
			{"the job", "jobs WHERE id = 'm7-job'", 1},
		} {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+tc.from).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != tc.want {
				t.Errorf("run %d: %s has %d rows, want %d", i+1, tc.what, n, tc.want)
			}
		}
	}
}
