//go:build live

package store

import (
	"context"
	"errors"
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

// The listing is the corpus in the order eviction reads it, so its tail is what
// goes next. The fixture's expected order disagrees with insertion order and
// with id order, and both disagreements are asserted rather than constructed.
func TestListReposIsMostRecentlyUsedFirstLive(t *testing.T) {
	ctx := context.Background()
	names := []string{"lru-one", "lru-two", "lru-three"}
	ids := make([]string, len(names))
	for i, n := range names {
		ids[i] = repoIDFor(n)
	}
	s := fresh(t, ids...)
	for _, n := range names {
		seedReadRepo(t, s, n, []string{"a.go"}, 1)
	}
	// Wound backwards, so the newest-used is the one inserted first.
	for i, id := range ids {
		if _, err := s.pool.Exec(ctx,
			`UPDATE repos SET last_queried_at = now() - ($2 * interval '1 hour') WHERE id = $1`,
			id, i); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.ListRepos(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(ids))
	for _, r := range rows {
		for _, id := range ids {
			if r.ID == id {
				got = append(got, r.ID)
			}
		}
	}
	if len(got) != 3 {
		t.Fatalf("listed %d of the fixture's 3 repos", len(got))
	}
	for i, want := range ids {
		if got[i] != want {
			t.Fatalf("position %d is %s, want %s (order %v)", i, got[i], want, got)
		}
	}
	// The property the assertion above depends on: if the fixture's ids happen
	// to sort into the expected order, a query with no ORDER BY could pass.
	if ids[0] < ids[1] && ids[1] < ids[2] {
		t.Fatal("fixture: the expected order agrees with id order")
	}

	// The clock, not the index time, and it is on the row a caller reads.
	for _, r := range rows {
		if r.ID == ids[0] && !r.LastUsedAt.After(r.IndexedAt.Add(-time.Second)) {
			t.Errorf("last_used_at %v is not a clock reading", r.LastUsedAt)
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
