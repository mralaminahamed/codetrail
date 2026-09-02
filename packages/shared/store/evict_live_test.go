//go:build live

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// evictFresh clears every repo, because eviction is a whole-corpus operation:
// a row left by another test is a row this one's counts have to explain.
//
// Safe only because TestMain gave this suite a database of its own. Run against
// the DSN a reader points at, this DELETE is what took their corpus.
func evictFresh(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	clear := func() {
		for _, table := range []string{"repos", "evicted_repos"} {
			if _, err := s.pool.Exec(ctx, `DELETE FROM `+table); err != nil {
				t.Errorf("clearing %s: %v", table, err)
			}
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

	// Queried in a different order than they were indexed, deliberately. With
	// the two agreeing — as they do if these are touched in seeding order — an
	// implementation ordering by indexed_at passes this test while evicting on
	// the wrong clock: measured, that mutant survives the agreeing fixture and
	// dies against this one. So the victim below is the newest indexed repo.
	touch(t, s, third, first, second)

	n, err := s.Evict(ctx, 2, keepEveryTombstone)
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

	// Again, with the clock moved so the survivors rank the other way round.
	// One eviction can be reproduced by ordering on any column that happens to
	// agree with it — `ORDER BY id DESC` passes the round above, measured,
	// because these content-addressed ids sort reverse to insertion. Two
	// evictions whose victims disagree on every static column cannot be: the
	// first drops the newest indexed repo, this one drops the oldest.
	touch(t, s, first, second)

	n, err = s.Evict(ctx, 1, keepEveryTombstone)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 evicted on the second round, got %d", n)
	}
	if _, err := s.GetRepo(ctx, first); err == nil {
		t.Fatal("the repo that fell to least recently queried should be gone")
	}
	if _, err := s.GetRepo(ctx, second); err != nil {
		t.Fatalf("the most recently queried repo should have been kept: %v", err)
	}
}

// Eviction must take the files with it, or the disk never actually frees.
func TestEvictCascadesLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	doomed := seed(t, s, "d")
	if _, err := s.Evict(ctx, 0, keepEveryTombstone); err != nil {
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
	n, err := s.Evict(ctx, 10, keepEveryTombstone)
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
	if _, err := s.Evict(ctx, -1, keepEveryTombstone); err == nil {
		t.Fatal("a negative keep was accepted")
	}
	if got, err := s.CountRepos(ctx); err != nil || got != 1 {
		t.Fatalf("want the repo still there, got %d (%v)", got, err)
	}
}

// Indexing counts as recency, or the worker undoes its own work: runJob writes
// the rows and then calls Evict, so a repo that was the least recently queried
// when the job started is deleted by the eviction at the end of that same job —
// a clone, a walk and a transaction spent on rows that do not survive the next
// statement.
func TestAReindexRenewsTheLeaseOnTheCorpusLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	stale := seed(t, s, "stale")
	hot := seed(t, s, "hot")
	touch(t, s, stale, hot) // stale is now the least recently queried

	// The same write runJob does immediately before it evicts. Same remote and
	// commit, so this is a re-index of that row, not a new one.
	time.Sleep(2 * time.Millisecond)
	if got := seed(t, s, "stale"); got != stale {
		t.Fatalf("the fixture re-indexed a different repo: %s != %s", got, stale)
	}

	if _, err := s.Evict(ctx, 1, keepEveryTombstone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRepo(ctx, stale); err != nil {
		t.Fatalf("the repo this job just indexed was evicted by that same job: %v", err)
	}
	if _, err := s.GetRepo(ctx, hot); err == nil {
		t.Fatal("the re-indexed repo did not overtake the one nothing had touched since")
	}
}

// keepEveryTombstone is larger than anything these fixtures evict, so a test
// that is not about the bound never trips it.
const keepEveryTombstone = 100

type tomb struct {
	remote, ref, commit string
	indexedAt           time.Time
	evictedAt           time.Time
}

func tombstoneOf(t *testing.T, s *Store, id string) (tomb, bool) {
	t.Helper()
	var tb tomb
	err := s.pool.QueryRow(context.Background(),
		`SELECT remote, ref, commit_sha, indexed_at, evicted_at FROM evicted_repos WHERE id = $1`, id).
		Scan(&tb.remote, &tb.ref, &tb.commit, &tb.indexedAt, &tb.evictedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return tomb{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return tb, true
}

// tombstoneIDs is the whole table, newest first — the order the bound keeps.
func tombstoneIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT id FROM evicted_repos ORDER BY evicted_at DESC, id DESC`)
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

// The tombstone is what makes 410 possible: after the DELETE and its cascade,
// nothing else in the database remembers the repo existed (spec §10).
//
// Two repos, one evicted: a fixture with one repo cannot tell a tombstone
// written for the right repo from one written for whatever row was to hand.
func TestEvictionLeavesATombstoneLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	doomed := seed(t, s, "doomed")
	kept := seed(t, s, "kept")
	touch(t, s, doomed, kept) // doomed is now the least recently queried

	if n, err := s.Evict(ctx, 1, keepEveryTombstone); err != nil || n != 1 {
		t.Fatalf("Evict reported %d (%v), want 1", n, err)
	}

	tb, ok := tombstoneOf(t, s, doomed)
	if !ok {
		t.Fatal("the evicted repo left no tombstone: it is now indistinguishable from one never submitted")
	}
	if tb.remote != "https://github.com/evict/doomed" || tb.ref != "main" ||
		tb.commit != fmt.Sprintf("%040d", len("doomed")) {
		t.Errorf("the tombstone describes some other repo: %+v", tb)
	}
	if tb.indexedAt.IsZero() || !tb.evictedAt.After(tb.indexedAt) {
		t.Errorf("want indexed_at from the repo row and evicted_at after it, got %v and %v", tb.indexedAt, tb.evictedAt)
	}
	if _, ok := tombstoneOf(t, s, kept); ok {
		t.Error("a repo that was never evicted was tombstoned")
	}

	gone, err := s.RepoGone(ctx, doomed)
	if err != nil || !gone {
		t.Errorf("RepoGone(evicted) = %v (%v), want true", gone, err)
	}
	if gone, err := s.RepoGone(ctx, kept); err != nil || gone {
		t.Errorf("RepoGone(a repo that is still here) = %v (%v), want false", gone, err)
	}
}

// Written in the same statement as the delete, so the count Evict returns is
// the tombstone insert's. A repo evicted, re-indexed at the same commit and
// evicted again hashes to the same id and hits the conflict: ON CONFLICT DO
// NOTHING would report that second eviction as 0 repos removed while removing
// one.
func TestReEvictingAKnownRepoStillCountsAndRefreshesTheTombstoneLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	id := seed(t, s, "twice")
	if n, err := s.Evict(ctx, 0, keepEveryTombstone); err != nil || n != 1 {
		t.Fatalf("first Evict reported %d (%v), want 1", n, err)
	}
	first, ok := tombstoneOf(t, s, id)
	if !ok {
		t.Fatal("the first eviction left no tombstone")
	}

	// The same repository at the same commit, so this is the row that was
	// tombstoned coming back rather than a new one.
	time.Sleep(2 * time.Millisecond)
	if again := seed(t, s, "twice"); again != id {
		t.Fatalf("the fixture re-indexed a different repo: %s != %s", again, id)
	}

	n, err := s.Evict(ctx, 0, keepEveryTombstone)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Evict reported %d, want 1", n)
	}
	// The eviction itself happened either way: the mutation this separates is a
	// wrong count, not a failed delete.
	if _, err := s.GetRepo(ctx, id); err == nil {
		t.Error("the repo survived the second eviction")
	}
	second, ok := tombstoneOf(t, s, id)
	if !ok {
		t.Fatal("the second eviction left no tombstone")
	}
	if !second.evictedAt.After(first.evictedAt) {
		t.Errorf("the tombstone still says %v, not the later eviction at %v", first.evictedAt, second.evictedAt)
	}
	if got := len(tombstoneIDs(t, s)); got != 1 {
		t.Errorf("one repo evicted twice left %d tombstones", got)
	}
}

// 410 is "we had it and dropped it". An id nothing ever indexed is a 404, and
// the two must not collapse.
func TestRepoGoneIsFalseForARepoThatNeverExistedLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	seed(t, s, "present")
	if _, err := s.Evict(ctx, 0, keepEveryTombstone); err != nil {
		t.Fatal(err)
	}
	gone, err := s.RepoGone(ctx, RepoID("https://github.com/evict/never", fmt.Sprintf("%040d", 9)))
	if err != nil {
		t.Fatal(err)
	}
	if gone {
		t.Fatal("an id nobody ever submitted answered gone, which is a 410 for a repo that never existed")
	}
}

// A repo id is hash(key, commit), so re-indexing the same commit re-uses the
// id and the tombstone outlives the eviction it recorded. A repo that is here
// is not gone, whatever the tombstone table still says.
func TestRepoGoneIsFalseForARepoThatWasIndexedAgainLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	id := seed(t, s, "back")
	if _, err := s.Evict(ctx, 0, keepEveryTombstone); err != nil {
		t.Fatal(err)
	}
	if again := seed(t, s, "back"); again != id {
		t.Fatalf("the fixture re-indexed a different repo: %s != %s", again, id)
	}
	if _, ok := tombstoneOf(t, s, id); !ok {
		t.Fatal("the fixture is not exercising the case: the tombstone is gone")
	}

	gone, err := s.RepoGone(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if gone {
		t.Fatal("a repo that is in the corpus right now answered gone, which is a 410 for a readable repo")
	}
}

// The tombstone table is a record of what used to be here, not a ledger. Past
// the bound an id goes back to 404, which is honest once we no longer remember.
//
// keep+1 evictions, one per statement so each gets its own evicted_at: a
// fixture that evicts twice cannot reach a bound of three.
func TestTombstonesAreBoundedToTheNewestLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	const keep = 3
	var ids []string
	for _, name := range []string{"one", "two", "three", "four"} {
		id := seed(t, s, name)
		ids = append(ids, id)
		time.Sleep(2 * time.Millisecond)
		if n, err := s.Evict(ctx, 0, keep); err != nil || n != 1 {
			t.Fatalf("evicting %s reported %d (%v), want 1", name, n, err)
		}
	}

	got := tombstoneIDs(t, s)
	if len(got) != keep {
		t.Fatalf("%d evictions under a bound of %d left %d tombstones", len(ids), keep, len(got))
	}
	// Newest first, and the oldest is the one that went.
	want := []string{ids[3], ids[2], ids[1]}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("tombstone %d is %s, want %s", i, got[i], id)
		}
	}
	if gone, err := s.RepoGone(ctx, ids[0]); err != nil || gone {
		t.Errorf("RepoGone(the trimmed repo) = %v (%v), want false: past the bound we no longer remember", gone, err)
	}
	if gone, err := s.RepoGone(ctx, ids[1]); err != nil || !gone {
		t.Errorf("RepoGone(the oldest kept repo) = %v (%v), want true", gone, err)
	}
}

// migrate() records a name and skips the body, so the idempotency test in
// store_live_test.go proves the ledger works and nothing about this file. The
// body is re-run here instead, against a jobs table that already holds rows and
// a tombstone table that already holds a tombstone — the case a deployed
// database is always in and a fresh one never is.
//
// In one rolled-back transaction, because it drops a column and a table the
// rest of this binary reads.
func TestMigration0009AppliesToAPopulatedDatabaseTwiceLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body, err := migrationFS.ReadFile("migrations/0009_history.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// Back to before 0009, then populated, so both statements run against rows.
	for _, stmt := range []string{
		`ALTER TABLE jobs DROP COLUMN IF EXISTS repo_id`,
		`DROP TABLE IF EXISTS evicted_repos`,
		`INSERT INTO jobs (id, remote, ref, status)
			VALUES ('m9', 'https://github.com/a/m9', 'main', 'done')`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	for i := range 2 {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		var repoID *string
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT repo_id, status FROM jobs WHERE id = 'm9'`).Scan(&repoID, &status); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if status != "done" {
			t.Fatalf("run %d: the existing job row now reads %q", i+1, status)
		}
		switch i {
		case 0:
			// A job that finished before the column existed has no repo, and
			// NULL is what that means.
			if repoID != nil {
				t.Errorf("run 1: the backfilled column reads %q, want NULL", *repoID)
			}
			// What a live database holds by the time the migration runs again.
			for _, stmt := range []string{
				`UPDATE jobs SET repo_id = 'r9' WHERE id = 'm9'`,
				`INSERT INTO evicted_repos (id, remote, ref, commit_sha, indexed_at)
					VALUES ('t9', 'https://github.com/a/t9', 'main', 'c0ffee', now())`,
			} {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
		case 1:
			// A migration that dropped and re-added the column, or the table,
			// would have erased both of these while still applying cleanly.
			if repoID == nil || *repoID != "r9" {
				t.Errorf("run 2: the repo a completed job named is %v, want r9", repoID)
			}
			var tombstones int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM evicted_repos`).Scan(&tombstones); err != nil {
				t.Fatal(err)
			}
			if tombstones != 1 {
				t.Errorf("run 2: %d tombstones survived, want 1", tombstones)
			}
		}
	}
}
