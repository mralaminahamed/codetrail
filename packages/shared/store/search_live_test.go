//go:build live

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// The vector arm's fixtures, against the query q = e0.
//
//	span  path       vector        cosine sim   L2 distance   inner product
//	A     zeta.go    10·e0         1.000        9.000         10.0
//	B     mu.go      e0 + 0.1·e1   0.995        0.100          1.0
//	D     alpha.go   2·e0 + 5·e1   0.371        5.099          2.0
//	C     beta.go    e1            0.000        1.414          0.0
//
// cosine (<=>) ranks A B D C, L2 (<->) ranks B C D A, inner product (<#>)
// ranks A D B C. Three different orders, so one fixture separates the operator
// the schema's index is built for from the two that also compile.
//
// The vectors are deliberately not unit length. For unit vectors L2 distance
// is a monotone function of cosine distance and the two orders are identical,
// so a unit-vector fixture cannot tell the operators apart at all — and equal
// magnitudes make inner product agree with cosine, which is the trap that
// looks the most like a passing test.
type searchSpan struct {
	label string
	path  string
	vec   []float32
}

// query is e0: the vector the tables above are computed against.
func query() []float32 { return sparse(map[int]float32{0: 1}) }

var (
	spanA = searchSpan{"A", "zeta.go", sparse(map[int]float32{0: 10})}
	spanB = searchSpan{"B", "mu.go", sparse(map[int]float32{0: 1, 1: 0.1})}
	spanD = searchSpan{"D", "alpha.go", sparse(map[int]float32{0: 2, 1: 5})}
	spanC = searchSpan{"C", "beta.go", sparse(map[int]float32{1: 1})}
	// A copy of the query itself, for the second repo: it ties A and beats
	// everything else, so a missing repo filter shows up as this span
	// appearing rather than as a subtly different order among the target
	// repo's own spans.
	spanOther = searchSpan{"OTHER", "other.go", sparse(map[int]float32{0: 1})}
)

// target is the ranking fixture in insertion order — C, D, B, A, the reverse
// of the answer. A query that lost its ORDER BY returns rows in physical
// order, which is insertion order, and a fixture inserted best-first would
// pass under exactly that bug.
var target = []searchSpan{spanC, spanD, spanB, spanA}

func searchRepoID(name string) string {
	return RepoID("https://github.com/search/"+name, Digest(name)[:40])
}

// seedSearch writes a target repo and a second repo, and returns a span id ->
// label map so an assertion reads [A B D C] rather than a list of hashes.
//
// Two repos in every fixture: a missing WHERE repo_id is otherwise invisible,
// and the corpus holds up to KEEP_REPOS of them in production.
func seedSearch(t *testing.T, targetSpans, otherSpans []searchSpan) (*Store, string, string, map[string]string) {
	t.Helper()
	targetID, otherID := searchRepoID(t.Name()+"/target"), searchRepoID(t.Name()+"/other")
	s := fresh(t, targetID, otherID)
	label := map[string]string{}
	writeSearchRepo(t, s, targetID, targetSpans, label)
	writeSearchRepo(t, s, otherID, otherSpans, label)
	return s, targetID, otherID, label
}

func writeSearchRepo(t *testing.T, s *Store, repoID string, spans []searchSpan, label map[string]string) {
	t.Helper()
	ctx := context.Background()
	files := make([]models.File, len(spans))
	embedded := make([]EmbeddedSpan, len(spans))
	for i, sp := range spans {
		fileID := FileID(repoID, sp.path)
		files[i] = models.File{ID: fileID, RepoID: repoID, Path: sp.path, Blob: "blob-" + sp.path, Lang: "go", Lines: 9}
		text := "span " + sp.label + " lives in " + sp.path
		d := Digest(text)
		id := SpanID(repoID, sp.path, 1, 9, d)
		embedded[i] = EmbeddedSpan{
			Span: models.Span{
				ID: id, RepoID: repoID, FileID: fileID, Path: sp.path,
				Kind: models.KindFunc, Symbol: sp.label,
				StartLine: 1, EndLine: 9, Text: text, Digest: d,
			},
			Embedding: sp.vec,
		}
		label[id] = sp.label
	}
	repo := models.Repo{ID: repoID, Remote: "https://github.com/search/" + repoID, Ref: "main", Commit: Digest(repoID)[:40]}
	if err := s.PutRepo(ctx, repo, files); err != nil {
		t.Fatal(err)
	}
	// PutSpans queues its inserts in slice order, so insertion order is the
	// physical order a query with no ORDER BY reads back.
	if err := s.PutSpans(ctx, repoID, embedded, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
}

func labelled(cites []models.Cite, label map[string]string) []string {
	out := make([]string, len(cites))
	for i, c := range cites {
		if l, ok := label[c.ID]; ok {
			out[i] = l
		} else {
			out[i] = "unknown:" + c.ID
		}
	}
	return out
}

func TestVectorSearchRanksByCosineAndNotByDistanceLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, label := seedSearch(t, target, []searchSpan{spanOther})

	got, err := s.VectorSearch(ctx, repoID, query(), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "B", "D", "C"}
	if !slices.Equal(labelled(got, label), want) {
		t.Fatalf("ranked %v, want %v (L2 would give [B C D A], inner product [A D B C], physical order [C D B A])",
			labelled(got, label), want)
	}

	// The fixture's own discriminating power, asserted rather than assumed: if
	// the ids ever sorted into the expected order, a query that lost its
	// ORDER BY could return them in id order and still pass.
	ids := make([]models.Cite, len(got))
	copy(ids, got)
	sort.Slice(ids, func(i, j int) bool { return ids[i].ID < ids[j].ID })
	if slices.Equal(labelled(ids, label), want) {
		t.Fatalf("span ids sort into the expected order %v; change the fixture's texts", want)
	}
	// And so does path order: alpha, beta, mu, zeta is D C B A.
	paths := slices.Clone(got)
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	if slices.Equal(labelled(paths, label), want) {
		t.Fatalf("path order is the expected order %v; change the fixture's paths", want)
	}
}

// The score in the response is a similarity, not the distance the ORDER BY
// runs on. A pinned pair of exact values, because "sorted descending" holds for
// the distance too and the ranking tests therefore cannot see this.
func TestVectorSearchScoresAreCosineSimilarityLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, label := seedSearch(t, target, []searchSpan{spanOther})

	got, err := s.VectorSearch(ctx, repoID, query(), 10)
	if err != nil {
		t.Fatal(err)
	}
	score := map[string]float32{}
	for _, c := range got {
		score[label[c.ID]] = c.Score
	}
	// Exact: 10·e0 is parallel to e0, so dot/(|a||b|) is 10/10, and e1 is
	// orthogonal, so it is 0/1. Both are exact in binary floating point.
	if score["A"] != 1 {
		t.Fatalf("A scored %v, want 1", score["A"])
	}
	if score["C"] != 0 {
		t.Fatalf("C scored %v, want 0", score["C"])
	}
}

// The second repo's span is an exact copy of the query, so it beats every span
// in the target repo. A second repo whose spans were poorer matches would leave
// the target repo's order intact and this test would pass without the filter.
func TestVectorSearchIsScopedToOneRepoLive(t *testing.T) {
	ctx := context.Background()
	// No A here: A is parallel to the query too, and a tie at distance 0 would
	// let the other repo's span land at either rank.
	s, repoID, otherID, label := seedSearch(t, []searchSpan{spanC, spanD, spanB}, []searchSpan{spanOther})

	got, err := s.VectorSearch(ctx, repoID, query(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range got {
		if c.RepoID == otherID {
			t.Fatalf("span %s from repo %s appeared at rank %d; the whole result was %v",
				label[c.ID], otherID, i+1, labelled(got, label))
		}
	}
	if !slices.Equal(labelled(got, label), []string{"B", "D", "C"}) {
		t.Fatalf("ranked %v, want [B D C]", labelled(got, label))
	}
}

func TestVectorSearchRespectsTheLimitLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, label := seedSearch(t, target, []searchSpan{spanOther})

	got, err := s.VectorSearch(ctx, repoID, query(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(labelled(got, label), []string{"A", "B"}) {
		t.Fatalf("limit 2 returned %v, want [A B]", labelled(got, label))
	}
}

// A span with no embedding has a NULL similarity, which has no float
// destination. The claim under test is that the call succeeds at all — written
// with a direct INSERT, because PutSpans always writes a vector and so no
// corpus it produces can reach this.
func TestSpansWithNoEmbeddingAreNotRetrievedLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, label := seedSearch(t, []searchSpan{spanA}, []searchSpan{spanOther})

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO spans (id, repo_id, file_id, path, kind, symbol, start_line,
			end_line, text, digest, embed_model, embed_dim, embedding)
		VALUES ($1, $2, $3, 'zeta.go', 'file', '', 20, 29, 'no vector', $4, $5, $6, NULL)`,
		"span-with-no-embedding", repoID, FileID(repoID, "zeta.go"), Digest("no vector"),
		fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}

	got, err := s.VectorSearch(ctx, repoID, query(), 10)
	if err != nil {
		t.Fatalf("a repo holding one NULL embedding made VectorSearch fail: %v", err)
	}
	if !slices.Equal(labelled(got, label), []string{"A"}) {
		t.Fatalf("got %v, want just [A]", labelled(got, label))
	}
}

// The ORDER BY has to be the form the ANN index can serve. Measured on this
// fixture: with every planner knob left alone, and with seqscan off, the plan
// is spans_path_idx plus a Sort — five rows are cheaper to sort than to walk a
// graph for. So the question this test can answer at this size is whether an
// index path *exists*, not whether it is chosen, and the honest way to ask it
// is to price the alternatives out: no seqscan, no bitmap scan, no sort.
//
// Which is why it asserts both halves. That the shipped ORDER BY reaches the
// index is only interesting next to the counterfactual — the same query sorted
// on the similarity it returns, which cannot reach it under any settings.
func TestVectorSearchOrderByCanUseTheAnnIndexLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, _ := seedSearch(t, target, []searchSpan{spanOther})

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, gucOff := range []string{"enable_seqscan", "enable_bitmapscan", "enable_sort"} {
		if _, err := tx.Exec(ctx, "SET LOCAL "+gucOff+" = off"); err != nil {
			t.Fatal(err)
		}
	}
	explain := func(orderBy string) string {
		t.Helper()
		rows, err := tx.Query(ctx, `
			EXPLAIN SELECT id, 1 - (embedding <=> $2::vector) AS score
			FROM spans
			WHERE repo_id = $1 AND embedding IS NOT NULL
			ORDER BY `+orderBy+`
			LIMIT $3`, repoID, vecLiteral(query()), 10)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(plan, "\n")
	}

	if p := explain(`embedding <=> $2::vector`); !strings.Contains(p, "spans_embedding_idx") {
		t.Fatalf("the shipped ORDER BY does not reach the ANN index:\n%s", p)
	}
	// The counterfactual, run rather than asserted in a comment: the same
	// query sorted on the similarity it returns has no index path at all.
	if p := explain(`score DESC`); strings.Contains(p, "spans_embedding_idx") {
		t.Fatalf("ORDER BY score DESC reached the ANN index after all:\n%s", p)
	}
}

func TestSpanEmbedderNamesWhatTheRepoWasIndexedWithLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, _ := seedSearch(t, target, []searchSpan{spanOther})

	model, dim, err := s.SpanEmbedder(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if model != fakeModel || dim != EmbeddingDim {
		t.Fatalf("got %s/%d, want %s/%d", model, dim, fakeModel, EmbeddingDim)
	}
}

// Direct INSERTs again: PutSpans replaces a repo's spans wholesale, so it
// cannot produce the corpus this guard exists for.
func TestSpanEmbedderRefusesARepoWithTwoModelsLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, _ := seedSearch(t, []searchSpan{spanA}, []searchSpan{spanOther})

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO spans (id, repo_id, file_id, path, kind, symbol, start_line,
			end_line, text, digest, embed_model, embed_dim, embedding)
		VALUES ($1, $2, $3, 'zeta.go', 'file', '', 20, 29, 'other model', $4, $5, $6, $7::vector)`,
		"span-from-another-model", repoID, FileID(repoID, "zeta.go"), Digest("other model"),
		"another-model-768", EmbeddingDim, vecLiteral(query())); err != nil {
		t.Fatal(err)
	}

	model, dim, err := s.SpanEmbedder(ctx, repoID)
	if !errors.Is(err, ErrMixedEmbedders) {
		t.Fatalf("got model %q dim %d err %v, want ErrMixedEmbedders", model, dim, err)
	}
}

func TestSpanEmbedderReportsARepoWithNoSpansLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, _ := seedSearch(t, nil, []searchSpan{spanOther})

	if _, _, err := s.SpanEmbedder(ctx, repoID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for a repo with no spans, got %v", err)
	}
}

// Not a mutation target: a sanity check that the fixture's arithmetic is what
// the table above claims, so a later reader can disagree with the numbers
// rather than re-deriving them.
func TestTheFixtureOrdersDifferUnderEachOperatorLive(t *testing.T) {
	ctx := context.Background()
	s, repoID, _, label := seedSearch(t, target, []searchSpan{spanOther})

	for _, tc := range []struct{ op, want string }{
		{"<=>", "A B D C"},
		{"<->", "B C D A"},
		{"<#>", "A D B C"},
	} {
		rows, err := s.pool.Query(ctx, fmt.Sprintf(`
			SELECT id FROM spans WHERE repo_id = $1
			ORDER BY embedding %s $2::vector`, tc.op), repoID, vecLiteral(query()))
		if err != nil {
			t.Fatal(err)
		}
		var got []models.Cite
		for rows.Next() {
			var c models.Cite
			if err := rows.Scan(&c.ID); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if order := strings.Join(labelled(got, label), " "); order != tc.want {
			t.Fatalf("%s ordered [%s], want [%s]", tc.op, order, tc.want)
		}
	}
}
