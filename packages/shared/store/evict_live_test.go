//go:build live

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// evictFresh clears every repo, because eviction is a whole-corpus operation:
// a row left by another test is a row this one's counts have to explain.
func evictFresh(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	clear := func() {
		if _, err := s.pool.Exec(ctx, `DELETE FROM repos`); err != nil {
			t.Errorf("clearing repos: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	return s
}

func seed(t *testing.T, s *Store, name string) string {
	t.Helper()
	ctx := context.Background()
	remote := "https://github.com/evict/" + name
	commit := fmt.Sprintf("%040d", len(name))
	id := RepoID(remote, commit)
	if err := s.PutRepo(ctx, models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit},
		[]models.File{{ID: FileID(id, "a.go"), RepoID: id, Path: "a.go", Lang: "go", Lines: 1}}); err != nil {
		t.Fatal(err)
	}
	return id
}

// touch moves the LRU clock through TouchRepo, which is what ties eviction to
// the column a query actually writes. now() is transaction time and the
// timestamps only have to be distinct, so sleep past the clock's resolution
// rather than trusting three round-trips to take a microsecond each.
func touch(t *testing.T, s *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		time.Sleep(2 * time.Millisecond)
		if err := s.TouchRepo(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvictKeepsTheMostRecentlyQueriedLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	first := seed(t, s, "a")
	second := seed(t, s, "bb")
	third := seed(t, s, "ccc")

	// Queried in a different order than they were indexed, deliberately: with
	// the two orders agreeing, ordering by indexed_at — or by nothing at all,
	// since ids happen to sort — would pass this test while evicting on the
	// wrong clock. The least recently queried repo is the newest indexed one.
	touch(t, s, third, first, second)

	n, err := s.Evict(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 evicted, got %d", n)
	}
	if _, err := s.GetRepo(ctx, third); err == nil {
		t.Fatal("the least recently queried repo should be gone")
	}
	for _, id := range []string{first, second} {
		if _, err := s.GetRepo(ctx, id); err != nil {
			t.Fatalf("%s should have been kept: %v", id, err)
		}
	}
	if got, err := s.CountRepos(ctx); err != nil || got != 2 {
		t.Fatalf("want 2 repos left, got %d (%v)", got, err)
	}
}

// Eviction must take the files with it, or the disk never actually frees.
func TestEvictCascadesLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	doomed := seed(t, s, "d")
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = $1`, doomed).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want the files gone, %d remain", n)
	}
}

func TestEvictIsANoOpUnderTheLimitLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	seed(t, s, "only")
	n, err := s.Evict(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want nothing evicted, got %d", n)
	}
	if got, err := s.CountRepos(ctx); err != nil || got != 1 {
		t.Fatalf("want the repo untouched, got %d (%v)", got, err)
	}
}

// A negative keep is a caller bug, and the corpus is what it would cost. It
// reaches Postgres as a negative OFFSET, which errors ("OFFSET must not be
// negative", measured against pg17) rather than deleting anything — this pins
// that, because a keep clamped to zero here would silently empty the table.
func TestEvictRefusesANegativeKeepLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	seed(t, s, "survivor")
	if _, err := s.Evict(ctx, -1); err == nil {
		t.Fatal("a negative keep was accepted")
	}
	if got, err := s.CountRepos(ctx); err != nil || got != 1 {
		t.Fatalf("want the repo still there, got %d (%v)", got, err)
	}
}
