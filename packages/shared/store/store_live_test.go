//go:build live

package store

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/testdb"
)

// scratchDSN is the database these tests run against: one this suite creates
// for itself, never the one DATABASE_URL names.
//
// These tests clear whole tables — eviction is a whole-corpus operation — and
// the README tells a reader to run them against the DSN `make up` serves.
// Measured before this: a seeded repo, its 51 spans and a job all vanished
// from that database while the suite went green.
var scratchDSN string

func TestMain(m *testing.M) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		// A skipped live suite prints the same "ok" as one that ran, so in CI a
		// dropped DATABASE_URL would look green with zero live coverage.
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "DATABASE_URL unset in CI — the live suite must never silently skip")
			os.Exit(1)
		}
		os.Exit(m.Run()) // every test skips; see dsn
	}
	code, err := testdb.Scratch(base, "codetrail_store", func(d string) int {
		scratchDSN = d
		return m.Run()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func dsn(t *testing.T) string {
	t.Helper()
	if scratchDSN == "" {
		t.Skip("set DATABASE_URL to run")
	}
	return scratchDSN
}

// The guarantee the suite rests on, asserted rather than assumed: nothing here
// runs in the database a reader pointed it at.
func TestTheLiveSuiteRunsInADatabaseItCreatedLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var here string
	if err := s.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&here); err != nil {
		t.Fatal(err)
	}
	if base := testdb.Name(os.Getenv("DATABASE_URL")); here == base {
		t.Fatalf("the suite is clearing tables in %s, the database it was pointed at", here)
	}
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
	applied, err := appliedMigrations(ctx, first.pool)
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

	after, err := appliedMigrations(ctx, second.pool)
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
		// path, start_line and end_line are P4's deviation from spec §3's
		// column list: without them a definition whose declaration is too long
		// for one span has no citable location at all.
		"symbols": {"id", "repo_id", "file_id", "path", "name", "pkg", "kind",
			"start_line", "end_line", "span_id"},
		// path and line likewise: "who calls this" has to answer with a
		// file:line, not with a name.
		"edges": {"id", "repo_id", "from_symbol_id", "to_symbol_id", "to_name",
			"kind", "provenance", "path", "line"},
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

// freshDatabase creates an empty database beside the one dsn points at and
// returns a pool config for it. A migration race only exists before the ledger
// is populated, so a test that reuses the already-migrated shared database
// proves nothing.
func freshDatabase(t *testing.T, base string) *pgxpool.Config {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("codetrail_race_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// FORCE, because a racer whose pool outlived the failure still holds a
		// session and DROP DATABASE would block on it.
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
		admin.Close(ctx)
	})
	cfg.ConnConfig.Database = name
	return cfg
}

// CI starts a fresh Postgres per PR and `go test ./...` runs the store and jobs
// binaries in parallel, so several processes call migrate() against an empty
// ledger at once. Without a lock they all read "nothing applied" and all apply
// 0001, and one of them loses.
func TestConcurrentMigrationsOnAFreshDatabaseLive(t *testing.T) {
	ctx := context.Background()
	cfg := freshDatabase(t, dsn(t))

	const racers = 8
	pools := make([]*pgxpool.Pool, racers)
	for i := range pools {
		p, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		// Connect before the barrier: pool setup jitter would otherwise stagger
		// the racers enough to hide the race.
		if err := p.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		pools[i] = p
	}

	start := make(chan struct{})
	errs := make(chan error, racers)
	for _, p := range pools {
		go func() {
			<-start
			errs <- (&Store{pool: p}).migrate(ctx)
		}()
	}
	close(start)
	for range racers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent migrate: %v", err)
		}
	}

	names, err := MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	applied, err := appliedMigrations(ctx, pools[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if !applied[n] {
			t.Errorf("%s is not recorded", n)
		}
	}
	if len(applied) != len(names) {
		t.Errorf("ledger has %d rows, want %d", len(applied), len(names))
	}

	// The guard must not outlive the run. Taking it here first proves the count
	// below can see a held lock, so reading zero means released and not that the
	// query looks in the wrong place.
	tx, err := pools[0].Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
		t.Fatal(err)
	}
	if n := heldAdvisoryLocks(t, pools[0]); n != 1 {
		t.Fatalf("pg_locks reports %d advisory locks while one is held", n)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := heldAdvisoryLocks(t, pools[0]); n != 0 {
		t.Errorf("%d advisory locks still held after every migrate returned", n)
	}
}

func heldAdvisoryLocks(t *testing.T, p *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory'
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The gauge exists because dev and production do not run the same pgvector —
// compose is ahead of what RDS offers — so the number has to come from the
// server rather than from the image tag anyone asked for. Asserted against
// pg_extension and current_setting read a second way: a Versions that returned
// a constant would pass every hermetic test there is.
func TestTheDatastoreInfoGaugeCarriesTheServersRealVersions(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	pg, vec, err := s.Versions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wantPG, wantVec string
	if err := s.Pool().QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&wantPG); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool().QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&wantVec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(wantPG, pg) || pg == "" {
		t.Errorf("Versions says postgres %q, the server says %q", pg, wantPG)
	}
	if vec != wantVec {
		t.Errorf("Versions says pgvector %q, pg_extension says %q", vec, wantVec)
	}
	// HNSW, which migrations/0002_span_ann.sql builds, needs 0.5.0.
	if vec < "0.5.0" {
		t.Errorf("pgvector is %q, below the 0.5.0 HNSW floor", vec)
	}

	// And New publishes it, which is the half a caller of Versions alone would
	// not have.
	series := `codetrail_datastore_info{pgvector="` + vec + `",postgres="` + pg + `"}`
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), series+" 1") {
		t.Fatalf("%s is not on /metrics after store.New", series)
	}
	t.Logf("datastore: postgres %s, pgvector %s", pg, vec)
}
