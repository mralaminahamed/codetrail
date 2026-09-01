//go:build live

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// fresh connects and clears the rows this test owns, before it runs and after.
//
// Cleanups run LIFO and after every defer, so `defer s.Close()` with a
// t.Cleanup delete would close the pool first and the delete would run against
// a dead one. That was not hypothetical: written that way, the delete silently
// failed, rows from earlier runs accumulated, and "two identical writes leave
// two files" failed on a third file a previous run had left. Registering Close
// first is what puts it last.
func fresh(t *testing.T, ids ...string) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	clear := func() {
		for _, id := range ids {
			if _, err := s.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, id); err != nil {
				// Loud: a cleanup that fails quietly is what let the rows pile
				// up in the first place.
				t.Errorf("clearing %s: %v", id, err)
			}
		}
	}
	clear()
	t.Cleanup(clear)
	return s
}

func TestPutRepoIsIdempotentLive(t *testing.T) {
	ctx := context.Background()
	const remote, commit = "https://github.com/a/idem", "cafe1234cafe1234cafe1234cafe1234cafe1234"
	id := RepoID(remote, commit)
	s := fresh(t, id)
	r := models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit}
	files := []models.File{
		{ID: FileID(id, "main.go"), RepoID: id, Path: "main.go", Blob: "b1", Lang: "go", Lines: 3},
		{ID: FileID(id, "a/b.go"), RepoID: id, Path: "a/b.go", Blob: "b2", Lang: "go", Lines: 1},
	}
	// Twice: a retried job after a crash must converge, not duplicate.
	for i := range 2 {
		if err := s.PutRepo(ctx, r, files); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 files after two identical writes, got %d", n)
	}
	if got := fileCount(t, s, id); got != 2 {
		t.Fatalf("want file_count 2, got %d", got)
	}

	// A third write with one more file: converging is not the same as being
	// frozen. The row count and the repo's own count both have to follow.
	files = append(files, models.File{
		ID: FileID(id, "c.go"), RepoID: id, Path: "c.go", Blob: "b3", Lang: "go", Lines: 9,
	})
	if err := s.PutRepo(ctx, r, files); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("want 3 files after a re-index that found one more, got %d", n)
	}
	if got := fileCount(t, s, id); got != 3 {
		t.Fatalf("file_count stayed at %d after a re-index found 3 files", got)
	}
}

// file_count is what an operator reads to see whether an index did anything.
func fileCount(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT file_count FROM repos WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Deleting a repo must take its files with it — eviction is one DELETE, and
// an orphan sweep is a second system to disagree with the first.
func TestDeletingARepoCascadesLive(t *testing.T) {
	ctx := context.Background()
	const remote, commit = "https://github.com/a/casc", "beef1234beef1234beef1234beef1234beef1234"
	id := RepoID(remote, commit)
	s := fresh(t, id)
	r := models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit}
	if err := s.PutRepo(ctx, r, []models.File{
		{ID: FileID(id, "x.go"), RepoID: id, Path: "x.go", Blob: "b", Lang: "go", Lines: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want the files cascaded away, %d remain", n)
	}
}

// GetRepo and TouchRepo are the LRU clock Evict orders on: a repo that was
// never indexed has to be distinguishable from one that was, and a touch has
// to move the column eviction will order by.
func TestGetRepoAndTouchRepoLive(t *testing.T) {
	ctx := context.Background()
	const remote, commit = "https://github.com/a/touch", "1234abcd1234abcd1234abcd1234abcd1234abcd"
	id := RepoID(remote, commit)
	s := fresh(t, id)

	if _, err := s.GetRepo(ctx, "no-such-repo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for an unknown id, got %v", err)
	}

	r := models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit, SizeBytes: 4242}
	if err := s.PutRepo(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRepo(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote != remote || got.Commit != commit || got.Ref != "main" {
		t.Fatalf("read back %+v", got)
	}
	// size_bytes is known only while the checkout exists, and the checkout is
	// deleted the moment the job ends. A column nothing writes reads 0 for
	// every repo, which invites a size-based eviction that ranks nonsense.
	if got.SizeBytes != 4242 {
		t.Fatalf("want size_bytes 4242, got %d", got.SizeBytes)
	}
	r.SizeBytes = 8484
	if err := s.PutRepo(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetRepo(ctx, id); err != nil || got.SizeBytes != 8484 {
		t.Fatalf("a re-index must update size_bytes: %d, %v", got.SizeBytes, err)
	}
	if got.IndexedAt.IsZero() {
		t.Fatal("indexed_at did not come back")
	}

	before := lastQueried(t, s, id)
	// now() is transaction time, so a touch in the same transaction as the
	// insert would not move. These are separate statements, but sleep past the
	// clock's resolution anyway so the comparison means something.
	time.Sleep(2 * time.Millisecond)
	if err := s.TouchRepo(ctx, id); err != nil {
		t.Fatal(err)
	}
	if after := lastQueried(t, s, id); !after.After(before) {
		t.Fatalf("last_queried_at did not advance: %v then %v", before, after)
	}
}

func lastQueried(t *testing.T, s *Store, id string) time.Time {
	t.Helper()
	var ts time.Time
	if err := s.pool.QueryRow(context.Background(),
		`SELECT last_queried_at FROM repos WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

// IDs are content-addressed, which is what makes the write above idempotent.
func TestIDsAreDeterministicAndDistinct(t *testing.T) {
	a := RepoID("https://github.com/a/b", "sha1")
	if a != RepoID("https://github.com/a/b", "sha1") {
		t.Fatal("RepoID is not deterministic")
	}
	if a == RepoID("https://github.com/a/b", "sha2") {
		t.Fatal("a different commit must be a different repo row")
	}
	if a == RepoID("https://github.com/a/c", "sha1") {
		t.Fatal("a different remote must be a different repo row")
	}
	if FileID(a, "x.go") == FileID(a, "y.go") {
		t.Fatal("different paths must be different files")
	}
	// The separator is what stops two different splits of the same bytes from
	// colliding: without it ("ab","c") and ("a","bc") hash alike.
	if RepoID("ab", "c") == RepoID("a", "bc") {
		t.Fatal("concatenation collides: the field separator is gone")
	}
}
