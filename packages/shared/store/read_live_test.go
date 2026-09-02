//go:build live

package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// seedReadRepo writes one repo, its files and its spans. Two of these stand
// side by side in every test below, because a query that lost its repo filter
// is invisible against a single-repository corpus.
func seedReadRepo(t *testing.T, s *Store, name string, paths []string, spansPerFile int) string {
	t.Helper()
	ctx := context.Background()
	remote, commit := "https://github.com/read/"+name, Digest(name)[:40]
	repoID := RepoID(remote, commit)

	files := make([]models.File, len(paths))
	var spans []EmbeddedSpan
	for i, p := range paths {
		files[i] = models.File{ID: FileID(repoID, p), RepoID: repoID, Path: p, Blob: "b" + p, Lang: "go", Lines: 40}
		for j := range spansPerFile {
			start := 1 + j*10
			spans = append(spans, mkSpan(repoID, files[i].ID, p, start, start+9,
				name+" "+p+" span "+string(rune('a'+j)), unit(i*3+j)))
		}
	}
	if err := s.PutRepo(ctx, models.Repo{ID: repoID, Remote: remote, Ref: "main", Commit: commit}, files); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSpans(ctx, repoID, spans, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	return repoID
}

func repoIDFor(name string) string {
	return RepoID("https://github.com/read/"+name, Digest(name)[:40])
}

// The listing is the corpus most recently used first, so its tail is what
// eviction takes next.
//
// The fixture's expected order agrees with none of the orders a wrong query
// could return the same four rows in — insertion, id, the remote's path, or
// indexed_at — and each disagreement is checked below rather than assumed. The
// version of this test that shipped with P3 wound the clock backwards, so its
// expected order *was* insertion order: it killed ORDER BY ASC and survived
// deleting the ORDER BY altogether.
//
// Two rows share a clock reading, so the id tiebreak is exercised too.
func TestListReposIsMostRecentlyUsedFirstLive(t *testing.T) {
	ctx := context.Background()
	names := []string{"lru-one", "lru-two", "lru-three", "lru-four"}
	// Two clocks per repo, wound to different permutations: a query reading
	// indexed_at where it means last_queried_at then returns a different order
	// rather than the same one, and no row's two timestamps are equal.
	base := time.Now().UTC().Truncate(time.Microsecond)
	ago := func(n int) time.Time { return base.Add(time.Duration(-n) * time.Minute) }
	lastUsed := map[string]time.Time{
		"lru-one": ago(60), "lru-two": ago(180),
		// Tied, which is what the id tiebreak decides between.
		"lru-three": base, "lru-four": base,
	}
	indexed := map[string]time.Time{
		"lru-one": ago(90), "lru-two": ago(240),
		"lru-three": ago(120), "lru-four": ago(150),
	}
	// Most recently used first; lru-four's id sorts under lru-three's.
	want := []string{"lru-four", "lru-three", "lru-one", "lru-two"}

	byID := slices.Clone(names)
	slices.SortFunc(byID, func(a, b string) int { return strings.Compare(repoIDFor(a), repoIDFor(b)) })
	byPath := slices.Sorted(slices.Values(names))
	byIndexed := slices.Clone(names)
	slices.SortFunc(byIndexed, func(a, b string) int { return indexed[b].Compare(indexed[a]) })
	for _, other := range []struct {
		what  string
		order []string
	}{
		{"insertion", names}, {"id", byID},
		{"the remote's path", byPath}, {"indexed_at", byIndexed},
	} {
		if slices.Equal(want, other.order) {
			t.Fatalf("fixture: the expected order agrees with %s order (%v)", other.what, other.order)
		}
	}

	ids := make([]string, len(names))
	name := make(map[string]string, len(names))
	for i, n := range names {
		ids[i] = repoIDFor(n)
		name[ids[i]] = n
	}
	s := fresh(t, ids...)
	for _, n := range names {
		seedReadRepo(t, s, n, []string{"a.go"}, 1)
	}
	for _, n := range names {
		if _, err := s.pool.Exec(ctx,
			`UPDATE repos SET last_queried_at = $2, indexed_at = $3 WHERE id = $1`,
			repoIDFor(n), lastUsed[n], indexed[n]); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.ListRepos(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(names))
	for _, r := range rows {
		if n, ok := name[r.ID]; ok {
			got = append(got, n)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}

	// The values, not merely the order. Selecting indexed_at into both columns
	// leaves the order alone, and asserting that last_used_at is within a
	// second of indexed_at cannot see the difference either — which is how that
	// mutation survived P3.
	for _, r := range rows {
		n, ok := name[r.ID]
		if !ok {
			continue
		}
		if !r.LastUsedAt.Equal(lastUsed[n]) {
			t.Errorf("%s last_used_at %v, want %v", n, r.LastUsedAt, lastUsed[n])
		}
		if !r.IndexedAt.Equal(indexed[n]) {
			t.Errorf("%s indexed_at %v, want %v", n, r.IndexedAt, indexed[n])
		}
	}

	// And the limit bounds it.
	short, err := s.ListRepos(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) != 1 {
		t.Fatalf("limit 1 returned %d rows", len(short))
	}
}

// Three numbers, and each of them counts this repository only. The other
// repository holds strictly more of everything, so a missing WHERE would be
// read as a bigger corpus rather than as an error.
func TestRepoStatsCountsThisRepositoryOnlyLive(t *testing.T) {
	ctx := context.Background()
	mine, other := repoIDFor("stats-mine"), repoIDFor("stats-other")
	s := fresh(t, mine, other)
	seedReadRepo(t, s, "stats-other", []string{"x.go", "y.go", "z.go"}, 3)
	// Three files, two of which produce spans: files_with_spans is a separate
	// number because chunk.Chunks drops token-less regions, so it cannot be
	// derived from the other two.
	seedReadRepo(t, s, "stats-mine", []string{"a.go", "b.go"}, 2)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO files (id, repo_id, path, blob, lang, lines)
		VALUES ($1, $2, 'empty.go', 'b3', 'go', 0)`, FileID(mine, "empty.go"), mine); err != nil {
		t.Fatal(err)
	}

	got, err := s.RepoStats(ctx, mine)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{Files: 3, Spans: 4, FilesWithSpans: 2}
	if got != want {
		t.Fatalf("stats %+v, want %+v", got, want)
	}
	// A repo with nothing in it is three zeros, not an error: an unindexed repo
	// is a real state.
	empty, err := s.RepoStats(ctx, "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if (empty != Stats{}) {
		t.Fatalf("stats for an unknown repo %+v", empty)
	}
}

// A span id is a primary key, so an unscoped read would serve another
// repository's code to a caller who named this one.
func TestGetSpanIsScopedToItsRepositoryLive(t *testing.T) {
	ctx := context.Background()
	mine, other := repoIDFor("span-mine"), repoIDFor("span-other")
	s := fresh(t, mine, other)
	seedReadRepo(t, s, "span-mine", []string{"a.go"}, 1)
	seedReadRepo(t, s, "span-other", []string{"a.go"}, 1)

	want := mkSpan(mine, FileID(mine, "a.go"), "a.go", 1, 10, "span-mine a.go span a", nil)
	got, err := s.GetSpan(ctx, mine, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Values, not presence: a row served with another span's lines is a
	// citation pointing at the wrong code.
	if got.Text != want.Text || got.Digest != want.Digest ||
		got.StartLine != want.StartLine || got.EndLine != want.EndLine ||
		got.Path != want.Path || got.Symbol != want.Symbol || got.Kind != want.Kind {
		t.Fatalf("span %+v, want %+v", got, want.Span)
	}

	// The same id under the other repository is not found, not served.
	if _, err := s.GetSpan(ctx, other, want.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a span of %s was served under %s: %v", mine, other, err)
	}
	if _, err := s.GetSpan(ctx, mine, "no-such-span"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown span: %v", err)
	}
}
