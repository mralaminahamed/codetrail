//go:build live

package jobs

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

func queue(t *testing.T) *Queue {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// A skipped live suite prints the same "ok" as one that ran, so in CI a
		// dropped DATABASE_URL would look green with zero live coverage.
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_URL unset in CI — the live suite must never silently skip")
		}
		t.Skip("set DATABASE_URL to run")
	}
	st, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	// Each test owns the table: leases and dedupe are global properties.
	if _, err := st.Pool().Exec(context.Background(), `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	return New(st.Pool())
}

// leaseRow reads the bookkeeping Job does not carry. A row that still names a
// worker after it stopped being leased misleads anyone reading the table.
func leaseRow(t *testing.T, q *Queue, id string) (by *string, until *time.Time) {
	t.Helper()
	if err := q.pool.QueryRow(context.Background(),
		`SELECT leased_by, leased_until FROM jobs WHERE id = $1`, id).Scan(&by, &until); err != nil {
		t.Fatal(err)
	}
	return by, until
}

func TestEnqueueThenLeaseThenComplete(t *testing.T) {
	ctx := context.Background()
	q := queue(t)

	j, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusPending {
		t.Fatalf("want pending, got %q", j.Status)
	}
	if j.Remote != "https://github.com/a/b" || j.Ref != "main" {
		t.Fatalf("Enqueue returned the wrong columns: %+v", j)
	}
	if j.Attempts != 0 || j.Error != "" {
		t.Fatalf("a new job has no attempts and no error: %+v", j)
	}

	got, ok, err := q.Lease(ctx, "worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("want a leased job, got ok=%v err=%v", ok, err)
	}
	if got.ID != j.ID || got.Status != StatusLeased {
		t.Fatalf("leased the wrong job: %+v", got)
	}
	if got.Remote != j.Remote || got.Ref != j.Ref {
		t.Fatalf("Lease returned the wrong columns: %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("want attempts 1, got %d", got.Attempts)
	}
	by, until := leaseRow(t, q, j.ID)
	if by == nil || *by != "worker-1" {
		t.Fatalf("want the lease recorded against worker-1, got %v", by)
	}
	if until == nil || !until.After(time.Now()) {
		t.Fatalf("want a lease expiring in the future, got %v", until)
	}

	if err := q.Complete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	after, err := q.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusDone {
		t.Fatalf("want done, got %q", after.Status)
	}
	if by, until := leaseRow(t, q, j.ID); by != nil || until != nil {
		t.Fatalf("a finished job still holds a lease: by=%v until=%v", by, until)
	}
}

// Submitting the same repository twice while it is queued must return the
// job that already exists, not clone it twice.
func TestEnqueueIsIdempotentWhileActive(t *testing.T) {
	ctx := context.Background()
	q := queue(t)

	first, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("want the same job back, got %s then %s", first.ID, second.ID)
	}

	// Once terminal, the same remote may be queued again — that is a re-index.
	if err := q.Complete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	third, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("a finished job must not block a re-index")
	}
}

// The dedupe key is (remote, ref), not remote alone: indexing main and a tag
// of the same repository is two jobs.
func TestEnqueueSeparatesRefsOfTheSameRemote(t *testing.T) {
	ctx := context.Background()
	q := queue(t)

	main, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := q.Enqueue(ctx, "https://github.com/a/b", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if main.ID == tag.ID {
		t.Fatal("a different ref of the same remote is a different job")
	}
	if main.Ref != "main" || tag.Ref != "v2" {
		t.Fatalf("refs came back wrong: %q and %q", main.Ref, tag.Ref)
	}
}

// Two workers leasing at once must not get the same job. This is what
// FOR UPDATE SKIP LOCKED buys, and it cannot be tested without concurrency.
func TestLeaseHandsEachJobToOneWorker(t *testing.T) {
	ctx := context.Background()
	q := queue(t)
	const n = 8
	for i := range n {
		if _, err := q.Enqueue(ctx, "https://github.com/a/r"+string(rune('a'+i)), "main"); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, ok, err := q.Lease(ctx, "worker", time.Minute)
			if err != nil || !ok {
				return
			}
			mu.Lock()
			seen[j.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	for id, count := range seen {
		if count != 1 {
			t.Fatalf("job %s was leased %d times", id, count)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no job was leased at all")
	}
}

// Oldest first, so a queue under sustained load still drains rather than
// starving whatever was submitted first.
func TestLeaseTakesTheOldestFirst(t *testing.T) {
	ctx := context.Background()
	q := queue(t)

	oldest, err := q.Enqueue(ctx, "https://github.com/a/oldest", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, "https://github.com/a/newest", "main"); err != nil {
		t.Fatal(err)
	}

	got, ok, err := q.Lease(ctx, "w", time.Minute)
	if err != nil || !ok {
		t.Fatalf("want a leased job, got ok=%v err=%v", ok, err)
	}
	if got.ID != oldest.ID {
		t.Fatalf("want the oldest job %s, got %s (%s)", oldest.ID, got.ID, got.Remote)
	}
}

// SKIP LOCKED is a progress guarantee as much as a safety one: a row another
// indexer is mid-claim on must be stepped over, not waited on. Two explicit
// transactions pin that without depending on goroutine scheduling.
func TestLeaseSkipsALockedRowRatherThanBlocking(t *testing.T) {
	ctx := context.Background()
	q := queue(t)

	first, err := q.Enqueue(ctx, "https://github.com/a/first", "main")
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.Enqueue(ctx, "https://github.com/a/second", "main")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	if err := tx.QueryRow(ctx,
		`SELECT id FROM jobs WHERE status = 'pending' ORDER BY created_at FOR UPDATE LIMIT 1`).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if locked != first.ID && locked != second.ID {
		t.Fatalf("locked an unexpected row: %s", locked)
	}

	// Without SKIP LOCKED this waits on the held lock until the deadline.
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, ok, err := q.Lease(deadline, "worker-2", time.Minute)
	if err != nil {
		t.Fatalf("Lease waited on a row another transaction holds: %v", err)
	}
	if !ok {
		t.Fatal("want the other claimable job, got none")
	}
	if got.ID == locked {
		t.Fatalf("leased %s while another transaction held it", got.ID)
	}
}

// A dead worker's job must come back. Without this a crash loses work
// silently, which is the failure nobody notices until a queue stops draining.
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	q := queue(t)
	j, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	// A lease that has already expired: the worker took it and died.
	if _, ok, err := q.Lease(ctx, "dead-worker", -time.Second); err != nil || !ok {
		t.Fatalf("first lease failed: ok=%v err=%v", ok, err)
	}
	got, ok, err := q.Lease(ctx, "live-worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("want the expired lease reclaimed, got ok=%v err=%v", ok, err)
	}
	if got.ID != j.ID {
		t.Fatalf("reclaimed the wrong job: %s", got.ID)
	}
	if got.Attempts != 2 {
		t.Fatalf("want attempts 2 after a reclaim, got %d", got.Attempts)
	}
}

// A live lease is not claimable: the reclaim must key off expiry, not merely
// on the row being leased.
func TestLiveLeaseIsNotReclaimed(t *testing.T) {
	ctx := context.Background()
	q := queue(t)
	if _, err := q.Enqueue(ctx, "https://github.com/a/b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := q.Lease(ctx, "worker-1", time.Minute); err != nil || !ok {
		t.Fatalf("first lease failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := q.Lease(ctx, "worker-2", time.Minute); err != nil || ok {
		t.Fatalf("want no job while the lease is live, got ok=%v err=%v", ok, err)
	}
}

// Failures retry until the cap, then stop. A job that retries forever is a
// job that fails forever, loudly and expensively.
func TestFailRetriesUntilTheCap(t *testing.T) {
	ctx := context.Background()
	q := queue(t)
	j, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	const max = 2

	if _, _, err := q.Lease(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ctx, j.ID, "clone failed", max); err != nil {
		t.Fatal(err)
	}
	mid, _ := q.Get(ctx, j.ID)
	if mid.Status != StatusPending {
		t.Fatalf("attempt 1 of %d must go back to pending, got %q", max, mid.Status)
	}
	if mid.Error != "clone failed" {
		t.Fatalf("want the retry reason kept, got %q", mid.Error)
	}
	if by, until := leaseRow(t, q, j.ID); by != nil || until != nil {
		t.Fatalf("a requeued job still holds a lease: by=%v until=%v", by, until)
	}

	if _, _, err := q.Lease(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ctx, j.ID, "clone failed again", max); err != nil {
		t.Fatal(err)
	}
	end, _ := q.Get(ctx, j.ID)
	if end.Status != StatusFailed {
		t.Fatalf("want failed at the cap, got %q", end.Status)
	}
	if end.Error != "clone failed again" {
		t.Fatalf("want the terminal reason recorded, got %q", end.Error)
	}
}

func TestLeaseOnAnEmptyQueue(t *testing.T) {
	_, ok, err := queue(t).Lease(context.Background(), "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("want no job from an empty queue")
	}
}

func TestGetUnknownJob(t *testing.T) {
	_, err := queue(t).Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
