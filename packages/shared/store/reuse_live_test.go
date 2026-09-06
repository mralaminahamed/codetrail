//go:build live

package store

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Fixture rule 9, in one corpus: TWO embed_model values, TWO embed_dim values,
// TWO repositories, and TWO spans sharing one digest.
//
// A single-model, single-repo fixture cannot detect three of the four
// predicates, and a corpus of distinct digests cannot detect the duplicate
// resolution at all.
//
// Written through raw SQL rather than through PutSpans, deliberately: PutSpans
// refuses a span whose vector width is not the configured one (spans.go's
// ErrDimMismatch guard), which is exactly the invariant that makes the
// embed_dim row below unreachable through the normal path. The row is corrupt
// on purpose — embed_dim is a plain INTEGER with no CHECK tying it to the
// vector(768) column, so it inserts cleanly, and that is the state the read has
// to refuse to lend from.
type reuseRow struct {
	repo, span, digest, model string
	dim                       int
	vec                       []float32 // nil writes SQL NULL
}

func seedReuse(t *testing.T, rows []reuseRow) *Store {
	t.Helper()
	ctx := context.Background()
	repos := map[string]bool{}
	for _, r := range rows {
		repos[r.repo] = true
	}
	ids := make([]string, 0, len(repos))
	for id := range repos {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	s := fresh(t, ids...)
	for _, id := range ids {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO repos (id, remote, ref, commit_sha) VALUES ($1, $2, 'main', $3)`,
			id, "https://github.com/reuse/"+id, Digest(id)[:40]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ($1, $2, 'a.go', '', 'go', 9)`,
			id+"-f", id); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		var vec any
		if r.vec != nil {
			vec = vecLiteral(r.vec)
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO spans (id, repo_id, file_id, path, kind, symbol, start_line, end_line,
				text, digest, embed_model, embed_dim, embedding)
			VALUES ($1, $2, $3, 'a.go', 'func', 'Sym', 1, 2, 'body', $4, $5, $6, $7::vector)`,
			r.span, r.repo, r.repo+"-f", r.digest, r.model, r.dim, vec); err != nil {
			t.Fatalf("seeding %s: %v", r.span, err)
		}
	}
	return s
}

func digestOf(name string) string { return Digest(name) }

// Two rows with ONE digest under two models. A single-model corpus cannot fire.
func TestEmbeddingsByDigestReturnsOnlyTheMatchingModelLive(t *testing.T) {
	d := digestOf("shared-across-models")
	s := seedReuse(t, []reuseRow{
		{repo: "rm1", span: "rm1-a", digest: d, model: "nomic-embed-text", dim: EmbeddingDim, vec: unit(1)},
		{repo: "rm1", span: "rm1-b", digest: d, model: "fake-hashed-bow", dim: EmbeddingDim, vec: unit(2)},
	})
	got, err := s.EmbeddingsByDigest(context.Background(), []string{d}, "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d embeddings for 1 digest, want 1", len(got))
	}
	if !reflect.DeepEqual(got[d], unit(1)) {
		t.Errorf("digest %s… returned the fake-hashed-bow vector", d[:4])
	}
	// And the other way, so the assertion is about the predicate rather than
	// about which row happens to sort first.
	got, err = s.EmbeddingsByDigest(context.Background(), []string{d}, "fake-hashed-bow", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[d], unit(2)) {
		t.Errorf("digest %s… returned the nomic-embed-text vector", d[:4])
	}
	// A model nobody wrote lends nothing.
	got, err = s.EmbeddingsByDigest(context.Background(), []string{d}, "some-other-model", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d embeddings for an unknown model, want 0", len(got))
	}
}

// Two rows with one digest and ONE model, at embed_dim 768 and 384. The 384 row
// is corrupt by construction and inserts cleanly, which is the point.
func TestEmbeddingsByDigestReturnsOnlyTheMatchingDimLive(t *testing.T) {
	d := digestOf("shared-across-dims")
	// The WRONG-WIDTH row sorts FIRST. ORDER BY digest, id picks the lowest id
	// per digest, so with the 768 row named rd1-a a width-blind read returns the
	// right vector by accident and the mutation survives — measured: it did,
	// on this fixture's first spelling.
	s := seedReuse(t, []reuseRow{
		{repo: "rd1", span: "rd1-a-wrong-width", digest: d, model: "nomic-embed-text", dim: 384, vec: unit(4)},
		{repo: "rd1", span: "rd1-b-right-width", digest: d, model: "nomic-embed-text", dim: EmbeddingDim, vec: unit(3)},
	})
	got, err := s.EmbeddingsByDigest(context.Background(), []string{d}, "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d embeddings, want 1", len(got))
	}
	if !reflect.DeepEqual(got[d], unit(3)) {
		t.Errorf("a row with embed_dim 384 was returned for a %d read", EmbeddingDim)
	}
	// Reading at 384 finds the corrupt row's declared width but a 768-wide
	// vector, which the width check refuses rather than lending.
	if _, err := s.EmbeddingsByDigest(context.Background(), []string{d}, "nomic-embed-text", 384); err == nil {
		t.Errorf("a %d-wide vector was lent under embed_dim 384", EmbeddingDim)
	} else if !strings.Contains(err.Error(), "384") {
		t.Errorf("the error does not name the width it refused: %v", err)
	}
}

// Two repositories sharing a digest, reading from the one that does NOT hold
// the vector. The whole value of the reuse is that it is content-keyed.
func TestEmbeddingsByDigestReadsAcrossRepositoriesLive(t *testing.T) {
	d := digestOf("the same helper in two repos")
	s := seedReuse(t, []reuseRow{
		{repo: "rx1", span: "rx1-a", digest: d, model: "nomic-embed-text", dim: EmbeddingDim, vec: unit(5)},
		// rx2 holds the same content and no vector: it is the repository being
		// indexed now.
		{repo: "rx2", span: "rx2-a", digest: d, model: "nomic-embed-text", dim: EmbeddingDim, vec: nil},
	})
	got, err := s.EmbeddingsByDigest(context.Background(), []string{d}, "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d embeddings for 1 digest, want 1", len(got))
	}
	if !reflect.DeepEqual(got[d], unit(5)) {
		t.Errorf("the vector came from somewhere other than the other repository")
	}
}

// A span with no vector cannot lend one. A fully-embedded corpus cannot fire.
func TestEmbeddingsByDigestSkipsSpansWithNoVectorLive(t *testing.T) {
	d := digestOf("never embedded")
	s := seedReuse(t, []reuseRow{
		{repo: "rn1", span: "rn1-a", digest: d, model: "nomic-embed-text", dim: EmbeddingDim, vec: nil},
	})
	got, err := s.EmbeddingsByDigest(context.Background(), []string{d}, "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got[d]; ok {
		t.Errorf("digest %s… mapped to a %d-component vector", d[:4], len(v))
	}
}

// FIVE rows sharing one digest with DIFFERENT vectors, read many times.
//
// Five rather than two on purpose: P3 measured that an unordered two-row result
// comes back in the same order whichever way it was inserted, so a two-row
// fixture may not separate an ordered read from an unordered one.
func TestEmbeddingsByDigestIsDeterministicWhenTwoRowsShareADigestLive(t *testing.T) {
	d := digestOf("five rows one digest")
	rows := make([]reuseRow, 0, 5)
	for i := range 5 {
		rows = append(rows, reuseRow{
			repo: "rq1", span: fmt.Sprintf("rq1-%d", i), digest: d,
			model: "nomic-embed-text", dim: EmbeddingDim, vec: unit(10 + i),
		})
	}
	s := seedReuse(t, rows)

	first, err := s.EmbeddingsByDigest(context.Background(), []string{d}, "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		got, err := s.EmbeddingsByDigest(context.Background(), []string{d}, "nomic-embed-text", EmbeddingDim)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[d], first[d]) {
			t.Fatalf("read %d returned a different vector for digest %s…", i, d[:4])
		}
	}
	// And it is the row ORDER BY digest, id names, not merely a stable one:
	// rq1-0 sorts first.
	if !reflect.DeepEqual(first[d], unit(10)) {
		t.Errorf("the winning row is not the one ORDER BY digest, id selects")
	}
}

// One round trip for a whole job, not one per digest.
func TestEmbeddingsByDigestIsOneRoundTripForManyDigestsLive(t *testing.T) {
	rows := make([]reuseRow, 0, 50)
	digests := make([]string, 0, 50)
	for i := range 50 {
		d := digestOf(fmt.Sprintf("many-%d", i))
		digests = append(digests, d)
		rows = append(rows, reuseRow{
			repo: "rr1", span: fmt.Sprintf("rr1-%d", i), digest: d,
			model: "nomic-embed-text", dim: EmbeddingDim, vec: unit(i),
		})
	}
	s := seedReuse(t, rows)

	before := queryCount(t, s)
	got, err := s.EmbeddingsByDigest(context.Background(), digests, "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d embeddings for 50 digests, want 50", len(got))
	}
	// The server's own counter, so "one round trip" is measured rather than
	// inferred from the shape of the code.
	if n := queryCount(t, s) - before; n > 3 {
		t.Errorf("the read took %d statements for 50 digests, want one", n)
	}
	// A digest nobody wrote is simply absent rather than an error.
	got, err = s.EmbeddingsByDigest(context.Background(),
		append(slices.Clone(digests), digestOf("absent")), "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Errorf("got %d embeddings, want 50: an unknown digest is an absence, not a row", len(got))
	}
	if len(got) != 0 {
		if _, err := s.EmbeddingsByDigest(context.Background(), nil, "m", EmbeddingDim); err != nil {
			t.Errorf("an empty digest list errored: %v", err)
		}
	}
}

// queryCount reads this backend's own statement counter.
func queryCount(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	err := s.pool.QueryRow(context.Background(),
		`SELECT coalesce(sum(calls), 0) FROM pg_stat_statements WHERE query LIKE '%spans%'`).Scan(&n)
	if err != nil {
		// pg_stat_statements is not installed on the pinned image. Fall back to
		// the transaction counter, which still separates one statement from
		// fifty round trips.
		if err := s.pool.QueryRow(context.Background(),
			`SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = current_database()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// ---- the uniqueness audit -------------------------------------------------

// The P1 blocker's shape, reproduced on purpose: two files rows with the same
// (repo_id, path) and DIFFERENT ids — the state a changed FileID would produce.
//
// A fixture that only inserts consistent rows cannot fire, because the
// constraint is dormant for them.
func TestFilesNoLongerCarriesASecondIdentityLive(t *testing.T) {
	ctx := context.Background()
	s := fresh(t, "fu1")
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO repos (id, remote, ref, commit_sha) VALUES ('fu1', 'https://github.com/a/fu1', 'main', 'c0ffee')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fu1-old-hash", "fu1-new-hash"} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ($1, 'fu1', 'a.go', '', 'go', 9)`,
			id); err != nil {
			t.Fatalf("inserting %s: %v — files still carries a second identity on (repo_id, path)", id, err)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = 'fu1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d files rows, want 2", n)
	}
	// The constraint is gone by name as well as by behaviour.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'files_repo_id_path_key')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Errorf("files_repo_id_path_key still exists")
	}
}

// The drop changes no behaviour today, which is exactly why this asserts the
// OTHER constraint is still doing the arbitration.
func TestTwoFilesWithOnePathInOneRepoAreStillArbitratedByIdLive(t *testing.T) {
	ctx := context.Background()
	s := fresh(t, "fa1")
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO repos (id, remote, ref, commit_sha) VALUES ('fa1', 'https://github.com/a/fa1', 'main', 'c0ffee')`); err != nil {
		t.Fatal(err)
	}
	id := FileID("fa1", "a.go")
	for i, lines := range []int{9, 11} {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ($1, 'fa1', 'a.go', '', 'go', $2)
			ON CONFLICT (id) DO UPDATE SET lines = EXCLUDED.lines`, id, lines); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	var n, lines int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), max(lines) FROM files WHERE repo_id = 'fa1'`).Scan(&n, &lines); err != nil {
		t.Fatal(err)
	}
	if n != 1 || lines != 11 {
		t.Errorf("%d rows with lines=%d, want 1 row updated in place: the primary key is what arbitrates", n, lines)
	}
}

// The ledger is keyed on the NAME and discovery is sort.Strings over a glob, so
// two files with one number is not an error — it is a silent skip of whichever
// sorts second.
func TestTheMigrationLedgerHasNoDuplicateNumbersLive(t *testing.T) {
	names, err := MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0001_init.sql", "0002_span_ann.sql", "0003_jobs.sql", "0004_repo_lru.sql",
		"0005_job_identity.sql", "0006_drop_repo_status.sql", "0007_repo_identity.sql",
		"0008_span_lexical.sql", "0009_history.sql", "0010_symbol_graph.sql",
		"0011_file_identity_and_reuse.sql",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("MigrationNames() = %v\nwant %v", names, want)
	}
	// A hand-written list alone cannot fire on a duplicate number, because a
	// duplicate still appears in it. The index existing after migrate is the
	// other half.
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'spans_reuse_idx')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Errorf("spans_reuse_idx does not exist after migrate")
	}
}

func TestMigration0011AppliesToAPopulatedDatabaseTwiceLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body, err := migrationFS.ReadFile("migrations/0011_file_identity_and_reuse.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// Back to before 0011 — the constraint restored, the index dropped — and
	// then populated, so the statements run against a table that already holds
	// rows and takes locks on it.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS spans_reuse_idx`,
		`ALTER TABLE files ADD CONSTRAINT files_repo_id_path_key UNIQUE (repo_id, path)`,
		`INSERT INTO repos (id, remote, ref, commit_sha) VALUES ('m11', 'https://github.com/a/m11', 'main', 'c0ffee')`,
		`INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ('m11-f', 'm11', 'a.go', '', 'go', 9)`,
		`INSERT INTO spans (id, repo_id, file_id, path, kind, symbol, start_line, end_line,
			text, digest, embed_model, embed_dim) VALUES
			('m11-s', 'm11', 'm11-f', 'a.go', 'func', 'S', 1, 2, 'b', 'd', 'm', 768)`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	for i := range 2 {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if i == 0 {
			// Written between the two runs: a migration that dropped and
			// re-created something would erase this and still apply cleanly.
			if _, err := tx.Exec(ctx, `
				INSERT INTO files (id, repo_id, path, blob, lang, lines)
				VALUES ('m11-f2', 'm11', 'a.go', '', 'go', 11)`); err != nil {
				t.Fatalf("run 1: two files rows on one path were refused: %v", err)
			}
		}
		// Two rows on one path, after both runs: the drop held, and the second
		// run did not re-create the constraint or erase the row written between
		// them.
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = 'm11'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("run %d: %d files rows on one path, want 2", i+1, n)
		}
		var idx bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'spans_reuse_idx')`).Scan(&idx); err != nil {
			t.Fatal(err)
		}
		if !idx {
			t.Errorf("run %d: spans_reuse_idx does not exist", i+1)
		}
	}
}

// spans_reuse_idx exists and is USABLE for the query it was created for.
//
// This is deliberately a weaker claim than "the planner chooses it", and the
// difference is worth stating. Whether a plan picks an index is a function of
// table size and statistics, and at fixture size Postgres correctly prefers a
// sequential scan — so a test asserting the plan would be asserting the
// fixture's size. With enable_seqscan off, the planner's choice is a statement
// about the INDEX rather than about the corpus: it can serve this predicate
// only if it exists and its columns cover it.
//
// What that catches, MEASURED rather than claimed: dropping the index (killed),
// and dropping a column from it or changing the query's predicate out from
// under it.
//
// What it does NOT catch, and an earlier draft of this comment said it did:
// REORDERING the columns. All three predicates are equality, so
// (embed_model, embed_dim, digest) serves this query exactly as well as
// (digest, embed_model, embed_dim) — the mutation survived. Column order would
// matter to a prefix scan on digest alone, and nothing does one. The order
// shipped is the one that reads in the order the caller thinks, not one this
// test pins.
//
// It also does not catch the index being the wrong SIZE: the partial
// `WHERE embedding IS NOT NULL` is a selectivity choice with no fixture-sized
// consequence, and the mutation that removes it is recorded as a survivor
// rather than covered.
func TestTheReuseIndexIsUsableForTheReadItExistsForLive(t *testing.T) {
	ctx := context.Background()
	s := seedReuse(t, []reuseRow{
		{repo: "ri1", span: "ri1-a", digest: digestOf("plan"), model: "nomic-embed-text", dim: EmbeddingDim, vec: unit(7)},
	})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+embeddingsByDigestSQL,
		[]string{digestOf("plan")}, "nomic-embed-text", EmbeddingDim)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "spans_reuse_idx") {
		t.Errorf("the reuse read cannot use spans_reuse_idx even with seqscan off:\n%s", plan.String())
	}
}

// PreviousBlobs answers about the OTHER commit, never about the one being
// indexed.
//
// Without the exclusion it compares a commit against itself, so files_changed
// reads 0 for every job — a number that always says "nothing changed", which is
// worse than no number. Found by the whole-branch sweep: neutralising the
// `r.id <> $2` predicate survived everything, because nothing exercised the
// read with two commits of one remote in the database.
func TestPreviousBlobsAnswersAboutTheOtherCommitLive(t *testing.T) {
	ctx := context.Background()
	const remote = "https://github.com/blobs/two-commits"
	oldID, newID := "pb-old", "pb-new"
	s := fresh(t, oldID, newID)

	for i, id := range []string{oldID, newID} {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO repos (id, remote, ref, commit_sha, indexed_at)
			VALUES ($1, $2, 'main', $3, now() - make_interval(days => $4))`,
			id, remote, Digest(id)[:40], 2-i); err != nil {
			t.Fatal(err)
		}
	}
	// The older commit's blobs, and the newer one's — deliberately different, so
	// a read that answered about the wrong commit is visible in the values.
	for _, r := range []struct{ repo, blob string }{
		{oldID, "1111111111111111111111111111111111111111"},
		{newID, "2222222222222222222222222222222222222222"},
	} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ($1, $2, 'a.go', $3, 'go', 9)`,
			r.repo+"-f", r.repo, r.blob); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.PreviousBlobs(ctx, remote, newID)
	if err != nil {
		t.Fatal(err)
	}
	if got["a.go"] != "1111111111111111111111111111111111111111" {
		t.Errorf("PreviousBlobs returned %q for a.go, want the OLDER commit's blob", got["a.go"])
	}
	// Case-folded, because the forge decides the display case of an owner and a
	// name and two case-variant submissions are one repository.
	up, err := s.PreviousBlobs(ctx, strings.ToUpper(remote), newID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(up, got) {
		t.Errorf("PreviousBlobs is case-sensitive on the remote: %v against %v", up, got)
	}
	// And with no other commit, an empty map rather than its own rows.
	only, err := s.PreviousBlobs(ctx, "https://github.com/blobs/only-one", "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 0 {
		t.Errorf("PreviousBlobs returned %v for a remote with no earlier commit", only)
	}
}
