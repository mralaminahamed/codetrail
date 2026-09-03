//go:build live

package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// seedCorpusRepo writes one repository with the given files, so a test can
// hold two repositories that share a path.
func seedCorpusRepo(t *testing.T, s *Store, name string, files map[string]string) string {
	t.Helper()
	remote, commit := "https://github.com/corpus/"+name, Digest(name)[:40]
	repoID := RepoID(remote, commit)
	rows := make([]models.File, 0, len(files))
	for path, blob := range files {
		rows = append(rows, models.File{
			ID: FileID(repoID, path), RepoID: repoID, Path: path, Blob: blob, Lang: "go", Lines: 9,
		})
	}
	if err := s.PutRepo(context.Background(),
		models.Repo{ID: repoID, Remote: remote, Ref: "main", Commit: commit}, rows); err != nil {
		t.Fatal(err)
	}
	return repoID
}

// The corpus fixture holds two repositories with a file at the *same path* and
// different bytes, so a read that lost its repo filter changes the answer
// rather than adding a row nobody compares.
func TestFileBlobsAreScopedToOneRepoLive(t *testing.T) {
	ctx := context.Background()
	target := RepoID("https://github.com/corpus/target", Digest("target")[:40])
	other := RepoID("https://github.com/corpus/other", Digest("other")[:40])
	s := fresh(t, target, other)
	seedCorpusRepo(t, s, "target", map[string]string{"store.go": "aaa", "read.go": "bbb"})
	seedCorpusRepo(t, s, "other", map[string]string{"store.go": "ZZZ"})

	got, err := s.FileBlobs(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"store.go": "aaa", "read.go": "bbb"}
	if len(got) != len(want) {
		t.Fatalf("FileBlobs returned %d entries %v; want %d — the other repository holds store.go too", len(got), got, len(want))
	}
	for p, b := range want {
		if got[p] != b {
			t.Errorf("FileBlobs[%q] is %q, want %q", p, got[p], b)
		}
	}
}

// The expected order is (path, start_line, id) and the fixture disagrees with
// every cheaper order: not insertion order, and not id order. P3 measured that
// an unordered span read comes back in *path* order via spans_path_idx, so the
// two spans that share a path are what make the start_line key observable.
func TestSpanRangesComeBackInAStableOrderLive(t *testing.T) {
	ctx := context.Background()
	repoID := RepoID("https://github.com/corpus/order", Digest("order")[:40])
	s := fresh(t, repoID)
	seedCorpusRepo(t, s, "order", map[string]string{"a.go": "a", "b.go": "b"})

	// Inserted deliberately out of the expected order.
	inserted := []EmbeddedSpan{
		mkSpan(repoID, FileID(repoID, "a.go"), "a.go", 40, 79, "a second", unit(1)),
		mkSpan(repoID, FileID(repoID, "b.go"), "b.go", 1, 39, "b first", unit(2)),
		mkSpan(repoID, FileID(repoID, "a.go"), "a.go", 1, 39, "a first", unit(3)),
	}
	if err := s.PutSpans(ctx, repoID, inserted, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}

	got, err := s.SpanRanges(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range got {
		lines = append(lines, fmt.Sprintf("%s:%d-%d", r.Path, r.StartLine, r.EndLine))
	}
	want := []string{"a.go:1-39", "a.go:40-79", "b.go:1-39"}
	if len(lines) != len(want) {
		t.Fatalf("SpanRanges returned %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("SpanRanges[%d] is %s, want %s", i, lines[i], want[i])
		}
	}

	// P3's fixture rule 2: the disagreement is asserted here rather than
	// constructed and trusted, because a fixture that happens to agree with a
	// cheaper order proves nothing about the ORDER BY.
	insertedOrder := []string{"a.go:40-79", "b.go:1-39", "a.go:1-39"}
	if equalStrings(want, insertedOrder) {
		t.Error("the expected order is insertion order and cannot discriminate")
	}
	byID := map[string]string{}
	for i, sp := range inserted {
		byID[sp.ID] = insertedOrder[i]
	}
	var idOrder []string
	for _, id := range sortedKeys(byID) {
		idOrder = append(idOrder, byID[id])
	}
	if equalStrings(want, idOrder) {
		t.Errorf("the expected order is id order (%v) and cannot discriminate", idOrder)
	}
}

// The probe reads every byte of text a repository has, so the pager has to
// advance. A fixture that fits in one page cannot tell a working cursor from
// one that returns the first page forever — and the silent half is the
// dangerous one, because the probe then passes on a corpus whose leak is on
// page two.
func TestSpanTextsPageThroughTheWholeCorpusLive(t *testing.T) {
	ctx := context.Background()
	repoID := RepoID("https://github.com/corpus/pages", Digest("pages")[:40])
	other := RepoID("https://github.com/corpus/pages-other", Digest("pages-other")[:40])
	s := fresh(t, repoID, other)
	seedCorpusRepo(t, s, "pages", map[string]string{"a.go": "a"})
	seedCorpusRepo(t, s, "pages-other", map[string]string{"a.go": "a"})

	const n = 7
	var spans []EmbeddedSpan
	for i := range n {
		spans = append(spans, mkSpan(repoID, FileID(repoID, "a.go"), "a.go", i*10+1, i*10+9,
			fmt.Sprintf("span number %d", i), unit(i)))
	}
	if err := s.PutSpans(ctx, repoID, spans, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	// The other repository holds text this walk must never return.
	if err := s.PutSpans(ctx, other, []EmbeddedSpan{
		mkSpan(other, FileID(other, "a.go"), "a.go", 1, 9, "the other repository's text", unit(9)),
	}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	var pages int
	after := ""
	for {
		rows, next, err := s.SpanTexts(ctx, repoID, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if pages > n+2 {
			t.Fatalf("the pager did not terminate after %d pages", pages)
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Errorf("span %s came back twice", r.ID)
			}
			seen[r.ID] = true
			if r.Text == "the other repository's text" {
				t.Errorf("the walk crossed a repository boundary at %s", r.ID)
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	if len(seen) != n {
		t.Errorf("the walk saw %d of %d spans over %d pages", len(seen), n, pages)
	}
	if pages < 4 {
		t.Errorf("%d spans at a page size of 2 came back in %d pages; the fixture no longer spans several pages", n, pages)
	}
	if _, _, err := s.SpanTexts(ctx, repoID, "", 0); err == nil {
		t.Error("a page size of 0 was accepted; it is a walk that never advances")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
