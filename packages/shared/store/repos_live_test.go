//go:build live

package store

import (
	"context"
	"errors"
	"slices"
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

// putAt writes a repo and pins indexed_at. The staleness query orders on that
// column, and now() at insert time is not something a test can space out
// reliably enough for "later" to mean anything.
func putAt(t *testing.T, s *Store, id, remote, ref, commit string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	r := models.Repo{ID: id, Remote: remote, Ref: ref, Commit: commit}
	if err := s.PutRepo(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE repos SET indexed_at = $2 WHERE id = $1`, id, at); err != nil {
		t.Fatal(err)
	}
}

// The staleness fixture: one repository whose main branch was indexed twice at
// different commits, and a tag indexed after both. Oldest first, so a query
// that lost its ordering returns the row that happens to be physically first.
const (
	stRemote = "https://github.com/o/staleness"
	stOld    = "1111111111111111111111111111111111111111"
	stNew    = "2222222222222222222222222222222222222222"
	stTag    = "3333333333333333333333333333333333333333"
)

func stID(commit string) string { return RepoID("github.com/o/staleness", commit) }

// The one positive staleness claim the corpus supports: the same ref indexed
// later at a different commit is proof the ref moved.
func TestNewerCommitFindsALaterIndexOfTheSameRefLive(t *testing.T) {
	ctx := context.Background()
	s := fresh(t, stID(stOld), stID(stNew))
	base := time.Now().UTC().Truncate(time.Microsecond)
	putAt(t, s, stID(stOld), stRemote, "main", stOld, base.AddDate(0, 0, -92))
	putAt(t, s, stID(stNew), stRemote, "main", stNew, base.AddDate(0, 0, -1))

	commit, at, err := s.NewerCommit(ctx, stID(stOld))
	if err != nil {
		t.Fatal(err)
	}
	if commit != stNew {
		t.Fatalf("newer commit %q, want %q", commit, stNew)
	}
	if want := base.AddDate(0, 0, -1); !at.Equal(want) {
		t.Fatalf("newer indexed at %v, want %v", at, want)
	}
}

// "The later commit" has to be the *latest* one when the ref moved twice.
//
// Until this fixture, no test ever put two candidate rows in the table, so the
// whole ORDER BY was deletable: the note beside a citation would then name an
// intermediate commit as the one that superseded it, which is a wrong claim to
// a user rather than a missing one. The two intermediates straddle stNew in
// commit_sha order and sit either side of it in insertion order, so ordering on
// the wrong column, in the wrong direction, or not at all each names one of
// them.
func TestNewerCommitNamesTheLatestOfSeveralLive(t *testing.T) {
	ctx := context.Background()
	const (
		stLowCommit  = "0000000000000000000000000000000000000000"
		stHighCommit = "9999999999999999999999999999999999999999"
	)
	s := fresh(t, stID(stOld), stID(stLowCommit), stID(stHighCommit), stID(stNew))
	base := time.Now().UTC().Truncate(time.Microsecond)
	// Written oldest-answer-first, so the row a query with no ORDER BY reaches
	// first is the wrong one.
	putAt(t, s, stID(stOld), stRemote, "main", stOld, base.AddDate(0, 0, -92))
	putAt(t, s, stID(stLowCommit), stRemote, "main", stLowCommit, base.AddDate(0, 0, -30))
	putAt(t, s, stID(stHighCommit), stRemote, "main", stHighCommit, base.AddDate(0, 0, -60))
	putAt(t, s, stID(stNew), stRemote, "main", stNew, base.AddDate(0, 0, -1))

	commit, at, err := s.NewerCommit(ctx, stID(stOld))
	if err != nil {
		t.Fatal(err)
	}
	if commit != stNew {
		t.Fatalf("three later commits: named %q as the latest, want %q", commit, stNew)
	}
	if want := base.AddDate(0, 0, -1); !at.Equal(want) {
		t.Fatalf("named %v as when it was indexed, want %v", at, want)
	}
}

// Two commits of one ref indexed in the same transaction tie on indexed_at, and
// then the commit_sha decides. Deterministically, because a note that names a
// different superseding commit on each read is two claims about one repository.
//
// Four tied rows rather than two: with a single sort key Postgres may return a
// tied pair in either order, and a pair that happens to come back in commit
// order proves nothing.
func TestNewerCommitBreaksAnIndexTieByCommitLive(t *testing.T) {
	ctx := context.Background()
	tied := []string{
		"7777777777777777777777777777777777777777",
		"4444444444444444444444444444444444444444",
		"8888888888888888888888888888888888888888",
		"5555555555555555555555555555555555555555",
	}
	ids := []string{stID(stOld)}
	for _, c := range tied {
		ids = append(ids, stID(c))
	}
	s := fresh(t, ids...)
	base := time.Now().UTC().Truncate(time.Microsecond)
	putAt(t, s, stID(stOld), stRemote, "main", stOld, base.AddDate(0, 0, -92))
	for _, c := range tied {
		putAt(t, s, stID(c), stRemote, "main", c, base)
	}

	commit, _, err := s.NewerCommit(ctx, stID(stOld))
	if err != nil {
		t.Fatal(err)
	}
	// The lowest of the four, which is neither the first written nor the last.
	if want := slices.Min(tied); commit != want {
		t.Fatalf("four commits indexed at one instant named %q, want %q", commit, want)
	}
}

// A citation into v1.0.0 is not stale because main was indexed afterwards: a
// tag does not move. Without the third row this is invisible.
func TestNewerCommitIgnoresADifferentRefLive(t *testing.T) {
	ctx := context.Background()
	s := fresh(t, stID(stOld), stID(stTag))
	base := time.Now().UTC().Truncate(time.Microsecond)
	putAt(t, s, stID(stOld), stRemote, "v1.0.0", stOld, base.AddDate(0, 0, -92))
	putAt(t, s, stID(stTag), stRemote, "main", stTag, base)

	commit, _, err := s.NewerCommit(ctx, stID(stOld))
	if err != nil {
		t.Fatal(err)
	}
	if commit != "" {
		t.Fatalf("a later index of another ref reported %q as newer", commit)
	}
}

// repos.remote holds the first submitter's spelling, so one repository indexed
// at two commits can carry two spellings. Identity folds case; this query has
// to fold it the same way or a repository's own history looks like two.
func TestNewerCommitFoldsTheRemotesCaseLive(t *testing.T) {
	ctx := context.Background()
	s := fresh(t, stID(stOld), stID(stNew))
	base := time.Now().UTC().Truncate(time.Microsecond)
	putAt(t, s, stID(stOld), "https://github.com/O/Staleness", "main", stOld, base.AddDate(0, 0, -92))
	putAt(t, s, stID(stNew), stRemote, "main", stNew, base.AddDate(0, 0, -1))

	commit, _, err := s.NewerCommit(ctx, stID(stOld))
	if err != nil {
		t.Fatal(err)
	}
	if commit != stNew {
		t.Fatalf("newer commit %q across two spellings of one remote, want %q", commit, stNew)
	}
}

// The newest index of a ref has nothing newer. Without this the query could
// return any row for the same ref and every citation would read as superseded.
func TestTheNewestIndexHasNoNewerCommitLive(t *testing.T) {
	ctx := context.Background()
	s := fresh(t, stID(stOld), stID(stNew))
	base := time.Now().UTC().Truncate(time.Microsecond)
	putAt(t, s, stID(stOld), stRemote, "main", stOld, base.AddDate(0, 0, -92))
	putAt(t, s, stID(stNew), stRemote, "main", stNew, base.AddDate(0, 0, -1))

	commit, at, err := s.NewerCommit(ctx, stID(stNew))
	if err != nil {
		t.Fatal(err)
	}
	if commit != "" || !at.IsZero() {
		t.Fatalf("the newest index reports %q at %v as newer than itself", commit, at)
	}
}

// Why the "different commit" predicate cannot be reached through PutRepo: id
// is hash(key, commit), so a re-index at the same commit updates one row.
func TestReIndexingTheSameCommitIsOneRowLive(t *testing.T) {
	ctx := context.Background()
	id := stID(stOld)
	s := fresh(t, id)
	putAt(t, s, id, stRemote, "main", stOld, time.Now().UTC().AddDate(0, 0, -92))
	putAt(t, s, id, "https://github.com/O/Staleness", "main", stOld, time.Now().UTC())

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM repos WHERE commit_sha = $1`, stOld).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("two indexes of one commit left %d rows", n)
	}
}

// A note reading "superseded by dfd11cc" beside a citation at dfd11cc is a
// staleness claim that contradicts itself, so the query says "different
// commit" rather than borrowing the guarantee from RepoID's construction three
// files away. The row is inserted directly because — see the test above —
// PutRepo cannot produce this state.
func TestNewerCommitNeverNamesTheCitationsOwnCommitLive(t *testing.T) {
	ctx := context.Background()
	const twin = "twin-of-the-same-commit"
	s := fresh(t, stID(stOld), twin)
	base := time.Now().UTC().Truncate(time.Microsecond)
	putAt(t, s, stID(stOld), stRemote, "main", stOld, base.AddDate(0, 0, -92))
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO repos (id, remote, ref, commit_sha, indexed_at) VALUES ($1, $2, 'main', $3, $4)`,
		twin, stRemote, stOld, base); err != nil {
		t.Fatal(err)
	}

	commit, _, err := s.NewerCommit(ctx, stID(stOld))
	if err != nil {
		t.Fatal(err)
	}
	if commit != "" {
		t.Fatalf("a later index of the same commit reported %q as newer", commit)
	}
}
