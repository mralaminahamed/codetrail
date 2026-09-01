//go:build live

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

const fakeModel = "fake-768"

// seedSpanRepo writes the repo and file a span has to hang off, and clears them
// again afterwards through fresh — so every test below starts from a repo that
// holds nothing, whatever an earlier run left in the database.
//
// The commit is derived from name, so two tests naming their fixtures
// differently cannot land on one repo id and read each other's spans.
func seedSpanRepo(t *testing.T, name string) (*Store, string, string) {
	t.Helper()
	remote, commit := "https://github.com/spans/"+name, Digest(name)[:40]
	repoID := RepoID(remote, commit)
	s := fresh(t, repoID)
	fileID := FileID(repoID, "a.go")
	if err := s.PutRepo(context.Background(),
		models.Repo{ID: repoID, Remote: remote, Ref: "main", Commit: commit},
		[]models.File{{ID: fileID, RepoID: repoID, Path: "a.go", Blob: "b", Lang: "go", Lines: 9}}); err != nil {
		t.Fatal(err)
	}
	return s, repoID, fileID
}

// mkSpan derives the id the way the indexer will, so a fixture cannot disagree
// with SpanID about what makes a span.
func mkSpan(repoID, fileID, path string, start, end int, text string, vec []float32) EmbeddedSpan {
	d := Digest(text)
	return EmbeddedSpan{
		Span: models.Span{
			ID: SpanID(repoID, path, start, end, d), RepoID: repoID, FileID: fileID,
			Path: path, Kind: models.KindFunc, Symbol: "Sym" + path,
			StartLine: start, EndLine: end, Text: text, Digest: d,
		},
		Embedding: vec,
	}
}

// unit is a one-hot vector: unit length, distinct per i, and every other
// component renders as a single "0", which keeps a thousand-span fixture from
// becoming a ten-megabyte statement.
func unit(i int) []float32 {
	v := make([]float32, EmbeddingDim)
	v[i%EmbeddingDim] = 1
	return v
}

// sparse builds a vector from index/value pairs, for the fixtures that need a
// magnitude cosine distance is supposed to ignore.
func sparse(at map[int]float32) []float32 {
	v := make([]float32, EmbeddingDim)
	for i, f := range at {
		v[i] = f
	}
	return v
}

func spanIDs(t *testing.T, s *Store, repoID string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT id FROM spans WHERE repo_id = $1 ORDER BY id`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The round trip everything downstream assumes: what went in comes back, with
// its line range, its kind, its symbol and its digest intact. Two spans, and
// they differ in every one of those, so a query that dropped a column or
// swapped two of them cannot read as a pass.
func TestPutSpansRoundTripLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "roundtrip")

	want := []EmbeddedSpan{
		mkSpan(repoID, fileID, "a.go", 4, 11, "func One() int { return 1 }", unit(3)),
		mkSpan(repoID, fileID, "b.go", 20, 20, "type Two struct{}", unit(7)),
	}
	want[1].Kind = models.KindType
	if err := s.PutSpans(ctx, repoID, want, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		var got models.Span
		var model, vec string
		var dim int
		if err := s.pool.QueryRow(ctx, `
			SELECT id, repo_id, file_id, path, kind, symbol, start_line, end_line,
			       text, digest, embed_model, embed_dim, embedding::text
			FROM spans WHERE id = $1`, w.ID).
			Scan(&got.ID, &got.RepoID, &got.FileID, &got.Path, &got.Kind, &got.Symbol,
				&got.StartLine, &got.EndLine, &got.Text, &got.Digest, &model, &dim, &vec); err != nil {
			t.Fatalf("%s (%s:%d-%d): %v", w.ID, w.Path, w.StartLine, w.EndLine, err)
		}
		if got != w.Span {
			t.Errorf("read back %+v, want %+v", got, w.Span)
		}
		// Parsed here rather than compared against vecLiteral's own output: a
		// format that lost precision would agree with itself on both sides of
		// that comparison. These floats are the ones the fixture built.
		back := parseVector(t, vec)
		if len(back) != len(w.Embedding) {
			t.Fatalf("%s: %d components came back, want %d", w.Path, len(back), len(w.Embedding))
		}
		for i := range back {
			if back[i] != w.Embedding[i] {
				t.Fatalf("%s: component %d came back as %v, want %v", w.Path, i, back[i], w.Embedding[i])
			}
		}
	}
}

func parseVector(t *testing.T, s string) []float32 {
	t.Helper()
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		t.Fatalf("not a pgvector value: %.40s…", s)
	}
	fields := strings.Split(s[1:len(s)-1], ",")
	out := make([]float32, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 32)
		if err != nil {
			t.Fatalf("component %d: %v", i, err)
		}
		out[i] = float32(v)
	}
	return out
}

// embed_model and embed_dim are written per row (spec §3), so a corpus built
// with the fake is identifiable in the database without anyone remembering.
func TestPutSpansRecordsTheModelAndDimLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "modeldim")

	sp := mkSpan(repoID, fileID, "a.go", 1, 2, "x", unit(1))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{sp}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	var model string
	var dim int
	if err := s.pool.QueryRow(ctx,
		`SELECT embed_model, embed_dim FROM spans WHERE id = $1`, sp.ID).Scan(&model, &dim); err != nil {
		t.Fatal(err)
	}
	if model != fakeModel || dim != EmbeddingDim {
		t.Fatalf("row records model %q dim %d, want %q and %d", model, dim, fakeModel, EmbeddingDim)
	}
}

// Everything past the first batch has to be written too. A fixture of one span,
// or of a hundred, cannot see a loop that stops after its first round trip.
func TestPutSpansWritesEveryBatchLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "batches")

	// Two full batches and a remainder, so both "stops after the first" and
	// "drops the short last one" are visible.
	const n = 2*spanBatchSize + 3
	spans := make([]EmbeddedSpan, n)
	want := make([]string, n)
	for i := range spans {
		spans[i] = mkSpan(repoID, fileID, fmt.Sprintf("f%04d.go", i), i+1, i+2,
			fmt.Sprintf("span number %d", i), unit(i))
		want[i] = spans[i].ID
	}
	if err := s.PutSpans(ctx, repoID, spans, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	got, err := s.CountSpans(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("wrote %d spans, %d came back", n, got)
	}
	// Which rows, not how many: a count alone cannot tell a dropped batch from
	// a batch written twice under different ids.
	slices.Sort(want)
	if have := spanIDs(t, s, repoID); !slices.Equal(have, want) {
		for i := range min(len(have), len(want)) {
			if have[i] != want[i] {
				t.Fatalf("row %d is %s, want %s", i, have[i], want[i])
			}
		}
		t.Fatalf("got %d ids, want %d", len(have), len(want))
	}
	// And the last span of the last batch carries its own text, not a
	// neighbour's: a batch that reused one argument set would still count n.
	var text string
	if err := s.pool.QueryRow(ctx, `SELECT text FROM spans WHERE id = $1`, spans[n-1].ID).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("span number %d", n-1); text != want {
		t.Fatalf("last span holds %q, want %q", text, want)
	}
}

// pgvector has to have understood the literal as a vector, and the ranking has
// to be cosine — the operator spans_embedding_idx is built for.
//
// The vectors are deliberately not all unit length. On unit vectors cosine and
// L2 rank identically, so a suite built only from those cannot tell <=> from
// <->; here the two orderings disagree at every position.
func TestSpansRankByCosineDistanceLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "cosine")

	// Against the query e0: cosine puts same-direction first regardless of
	// magnitude, L2 puts nearest-in-space first.
	//   aligned  3·e0          cosine 0       L2 2.0
	//   tilted   0.9e0+0.1e1   cosine 0.0061  L2 0.141
	//   ortho    e1            cosine 1       L2 1.414
	aligned := mkSpan(repoID, fileID, "aligned.go", 1, 1, "aligned", sparse(map[int]float32{0: 3}))
	tilted := mkSpan(repoID, fileID, "tilted.go", 2, 2, "tilted", sparse(map[int]float32{0: 0.9, 1: 0.1}))
	ortho := mkSpan(repoID, fileID, "ortho.go", 3, 3, "ortho", sparse(map[int]float32{1: 1}))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{ortho, tilted, aligned}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT path, embedding <=> $1::vector
		FROM spans WHERE repo_id = $2
		ORDER BY embedding <=> $1::vector`, vecLiteral(unit(0)), repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	var dists []float64
	for rows.Next() {
		var p string
		var d float64
		if err := rows.Scan(&p, &d); err != nil {
			t.Fatal(err)
		}
		order = append(order, p)
		dists = append(dists, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"aligned.go", "tilted.go", "ortho.go"}
	if !slices.Equal(order, want) {
		t.Fatalf("cosine order %v with distances %v, want %v (L2 would give [tilted ortho aligned])", order, dists, want)
	}
	// Total, not merely sorted: three equal distances would satisfy the order
	// above and would mean the vectors never made it into the column.
	for i := 1; i < len(dists); i++ {
		if dists[i] <= dists[i-1] {
			t.Fatalf("distances %v are not strictly increasing", dists)
		}
	}
}

// Spec §3: re-indexing the same commit writes the same rows.
func TestPutSpansIsIdempotentLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "idempotent")

	spans := []EmbeddedSpan{
		mkSpan(repoID, fileID, "a.go", 1, 3, "one", unit(1)),
		mkSpan(repoID, fileID, "a.go", 5, 8, "two", unit(2)),
		mkSpan(repoID, fileID, "b.go", 1, 2, "three", unit(3)),
	}
	var first []string
	for i := range 2 {
		if err := s.PutSpans(ctx, repoID, spans, fakeModel, EmbeddingDim); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
		ids := spanIDs(t, s, repoID)
		if len(ids) != len(spans) {
			t.Fatalf("write %d left %d spans, want %d", i+1, len(ids), len(spans))
		}
		if i == 0 {
			first = ids
		} else if !slices.Equal(ids, first) {
			t.Fatalf("the second write produced different rows: %v then %v", first, ids)
		}
	}
}

// The contract PutRepo does not have. A second, smaller set replaces the first:
// otherwise a re-chunk under a different window leaves both runs' spans in one
// table and a retrieval mixes them.
//
// Not covered by idempotence — measured: with the DELETE removed, the identical
// second write still lands on the same ids and still counts three.
func TestPutSpansReplacesTheRepoPreviousSpansLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "replace")

	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{
		mkSpan(repoID, fileID, "a.go", 1, 3, "one", unit(1)),
		mkSpan(repoID, fileID, "a.go", 5, 8, "two", unit(2)),
		mkSpan(repoID, fileID, "b.go", 1, 2, "three", unit(3)),
	}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	// A different window over the same file: new ids, none of them shared with
	// the first set.
	only := mkSpan(repoID, fileID, "a.go", 1, 8, "one\ntwo", unit(4))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{only}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountSpans(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d spans after replacing 3 with 1", n)
	}
	if ids := spanIDs(t, s, repoID); !slices.Equal(ids, []string{only.ID}) {
		t.Fatalf("the survivor is %v, want the newly written %s", ids, only.ID)
	}
}

// No spans is a set too. A re-index whose files all became unchunkable has to
// clear the repo, not leave the previous run's rows as the answer.
func TestPutSpansWithNoSpansClearsTheRepoLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "empty")

	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{
		mkSpan(repoID, fileID, "a.go", 1, 3, "one", unit(1)),
		mkSpan(repoID, fileID, "a.go", 5, 8, "two", unit(2)),
	}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSpans(ctx, repoID, nil, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountSpans(ctx, repoID); err != nil || n != 0 {
		t.Fatalf("want the repo cleared, %d spans remain (%v)", n, err)
	}
}

// The replacement is scoped to one repo. A DELETE that forgot its WHERE, or a
// count that forgot it, would both read as a pass against a single repo.
func TestPutSpansAndCountSpansAreScopedToTheRepoLive(t *testing.T) {
	ctx := context.Background()
	s, mine, myFile := seedSpanRepo(t, "scoped-mine")
	_, theirs, theirFile := seedSpanRepo(t, "scoped-theirs")

	if err := s.PutSpans(ctx, theirs, []EmbeddedSpan{
		mkSpan(theirs, theirFile, "a.go", 1, 1, "theirs one", unit(9)),
		mkSpan(theirs, theirFile, "a.go", 2, 2, "theirs two", unit(10)),
	}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSpans(ctx, mine, []EmbeddedSpan{
		mkSpan(mine, myFile, "a.go", 1, 1, "mine", unit(11)),
	}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountSpans(ctx, mine); err != nil || n != 1 {
		t.Fatalf("my repo has %d spans, want 1 (%v)", n, err)
	}
	if n, err := s.CountSpans(ctx, theirs); err != nil || n != 2 {
		t.Fatalf("writing my repo changed theirs: %d spans, want 2 (%v)", n, err)
	}
}

// A duplicate inside one call converges. PutSpans takes a caller-built slice,
// and a repeated span in it must not abort a job at its last step.
func TestPutSpansConvergesOnADuplicateInOneCallLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "duplicate")

	sp := mkSpan(repoID, fileID, "a.go", 1, 3, "one", unit(1))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{sp, sp}, fakeModel, EmbeddingDim); err != nil {
		t.Fatalf("a repeated span was refused: %v", err)
	}
	if n, err := s.CountSpans(ctx, repoID); err != nil || n != 1 {
		t.Fatalf("want 1 row for a span written twice, got %d (%v)", n, err)
	}
}

// Eviction is one DELETE that cascades (spec §3). Spans are new dependents of
// repos, so the cascade has to reach them.
func TestEvictionRemovesSpansLive(t *testing.T) {
	ctx := context.Background()
	// Whole-corpus: Evict counts every repo, so this one owns the table.
	s := evictFresh(t)
	remote, commit := "https://github.com/spans/evicted", Digest("evicted")[:40]
	repoID := RepoID(remote, commit)
	fileID := FileID(repoID, "a.go")
	if err := s.PutRepo(ctx, models.Repo{ID: repoID, Remote: remote, Ref: "main", Commit: commit},
		[]models.File{{ID: fileID, RepoID: repoID, Path: "a.go", Blob: "b", Lang: "go", Lines: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{
		mkSpan(repoID, fileID, "a.go", 1, 1, "one", unit(1)),
		mkSpan(repoID, fileID, "a.go", 2, 2, "two", unit(2)),
	}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountSpans(ctx, repoID); err != nil || n != 2 {
		t.Fatalf("the fixture wrote %d spans, want 2 (%v)", n, err)
	}
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountSpans(ctx, repoID); err != nil || n != 0 {
		t.Fatalf("%d spans survived their repo's eviction (%v)", n, err)
	}
}

// The width is fixed and enforced in Go, so the message can name the span
// rather than a column.
func TestPutSpansRefusesAVectorOfTheWrongWidthLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "width")

	// A corpus to lose. The refusal has to leave it exactly as it was, or a
	// mis-configured embedder costs a repo its spans on every attempt.
	kept := []EmbeddedSpan{
		mkSpan(repoID, fileID, "a.go", 1, 3, "one", unit(1)),
		mkSpan(repoID, fileID, "a.go", 5, 8, "two", unit(2)),
	}
	if err := s.PutSpans(ctx, repoID, kept, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}

	good := mkSpan(repoID, fileID, "c.go", 1, 1, "good", unit(3))
	narrow := mkSpan(repoID, fileID, "b.go", 7, 9, "narrow", make([]float32, EmbeddingDim-1))
	err := s.PutSpans(ctx, repoID, []EmbeddedSpan{good, narrow}, fakeModel, EmbeddingDim)
	// errors.Is, not a substring. Postgres refuses this too, with "expected 768
	// dimensions, not 767" — a message that contains 768 and would satisfy a
	// substring check while proving only that pgvector works. The sentinel is
	// the only assertion that separates the two.
	if !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("want ErrDimMismatch, got %v", err)
	}
	// And it has to say which span, or an operator has a repo to search.
	if !strings.Contains(err.Error(), "b.go:7-9") {
		t.Errorf("the error does not name the span: %v", err)
	}
	want := []string{kept[0].ID, kept[1].ID}
	slices.Sort(want)
	if got := spanIDs(t, s, repoID); !slices.Equal(got, want) {
		t.Fatalf("the refused write changed the corpus: %v, want %v", got, want)
	}
}

// The write path must not be a way around CheckDim: an embedder of another
// width handing over its own dim with matching vectors would otherwise pass a
// per-span length check and be refused only by the column.
func TestPutSpansRefusesAnEmbedderOfAnotherWidthLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "otherwidth")

	const narrowModel, narrowDim = "all-minilm", 384
	sp := mkSpan(repoID, fileID, "a.go", 1, 1, "x", make([]float32, narrowDim))
	err := s.PutSpans(ctx, repoID, []EmbeddedSpan{sp}, narrowModel, narrowDim)
	if !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("want ErrDimMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), narrowModel) {
		t.Errorf("the error does not name the model to change: %v", err)
	}
}

// A span whose file_id does not exist must fail loudly. Task 6 writes files
// before spans, and this constraint is what makes that ordering a fact.
func TestPutSpansRequiresItsFileLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _ := seedSpanRepo(t, "orphan")

	sp := mkSpan(repoID, "no-such-file", "a.go", 1, 1, "x", unit(1))
	err := s.PutSpans(ctx, repoID, []EmbeddedSpan{sp}, fakeModel, EmbeddingDim)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Fatalf("want a foreign key violation (23503), got %v", err)
	}
}

// A write that fails part way must leave the repo as it was. The DELETE and the
// inserts are one transaction for exactly this: the previous corpus is what a
// query still answers from until a replacement commits.
func TestAFailedPutSpansLeavesThePreviousSpansLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "atomic")

	before := []EmbeddedSpan{
		mkSpan(repoID, fileID, "a.go", 1, 3, "one", unit(1)),
		mkSpan(repoID, fileID, "a.go", 5, 8, "two", unit(2)),
		mkSpan(repoID, fileID, "b.go", 1, 2, "three", unit(3)),
	}
	if err := s.PutSpans(ctx, repoID, before, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	// It fails inside the batch, after the DELETE has run — the case a
	// pre-flight check cannot cover and only the transaction can.
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{
		mkSpan(repoID, fileID, "c.go", 1, 1, "good", unit(4)),
		mkSpan(repoID, "no-such-file", "d.go", 1, 1, "orphan", unit(5)),
	}, fakeModel, EmbeddingDim); err == nil {
		t.Fatal("a span with no file was accepted")
	}
	n, err := s.CountSpans(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(before) {
		t.Fatalf("%d spans survived a failed write, want %d", n, len(before))
	}
	want := make([]string, len(before))
	for i, sp := range before {
		want[i] = sp.ID
	}
	slices.Sort(want)
	if got := spanIDs(t, s, repoID); !slices.Equal(got, want) {
		t.Fatalf("a failed write changed which spans are stored: %v, want %v", got, want)
	}
}

// The rows written are the argument's repo, not whatever the spans claim. A
// mismatched RepoID means the caller built these for another repo: relabelling
// them would store a row whose id hashes one repo under another, and honouring
// them would put rows outside the set the DELETE just cleared.
func TestPutSpansRefusesASpanFromAnotherRepoLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, fileID := seedSpanRepo(t, "foreign")
	_, other, _ := seedSpanRepo(t, "foreign-other")

	err := s.PutSpans(ctx, repoID, []EmbeddedSpan{
		mkSpan(other, fileID, "a.go", 1, 1, "x", unit(1)),
	}, fakeModel, EmbeddingDim)
	if err == nil {
		t.Fatal("a span belonging to another repo was written")
	}
	if !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), repoID) {
		t.Errorf("the error names neither repo: %v", err)
	}
	if n, err := s.CountSpans(ctx, repoID); err != nil || n != 0 {
		t.Fatalf("%d spans were written anyway (%v)", n, err)
	}
}
