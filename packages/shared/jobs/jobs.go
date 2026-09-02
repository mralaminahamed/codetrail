// Package jobs is the indexing queue: a Postgres table, not a broker.
//
// One producer, durable, retryable, and inspectable with psql. A broker would
// be a second system to run and justify for a queue this shape.
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusLeased  Status = "leased"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

var ErrNotFound = errors.New("jobs: not found")

// ErrNotLeased means the job is not the caller's to finish: it does not
// exist, or its lease expired and another worker has already reclaimed it.
var ErrNotLeased = errors.New("jobs: not leased by this worker")

type Job struct {
	ID       string `json:"id"`
	Remote   string `json:"remote"`
	Ref      string `json:"ref"`
	Status   Status `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
	// RepoID is what this job produced, and is how a caller who submitted a
	// repository names it afterwards: the id is hash(key, commit) and only the
	// indexer ever saw the commit. Empty until Complete writes it.
	RepoID string `json:"repo_id,omitempty"`
}

// DefaultRetryBase and DefaultRetryMax bound the wait between attempts. Small
// enough that a forge blip is retried within a poll cycle or two, large enough
// that a repository which does not exist no longer spends all three attempts
// in nine seconds — measured, before Fail set a retry time at all.
//
// DefaultRetryMax governs nothing at the default MAX_ATTEMPTS=3, which only
// ever waits 30s then 60s: the cap first binds on the sixth attempt, so it
// takes MAX_ATTEMPTS of 7 or more to reach it.
const (
	DefaultRetryBase = 30 * time.Second
	DefaultRetryMax  = 10 * time.Minute
)

// RetryBase and RetryMax are fields so a test can watch a retry it would
// otherwise have to wait half a minute for.
type Queue struct {
	pool      *pgxpool.Pool
	RetryBase time.Duration
	RetryMax  time.Duration
}

func New(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool, RetryBase: DefaultRetryBase, RetryMax: DefaultRetryMax}
}

// coalesce because repo_id is NULL on every job that has not completed, and
// "" is what that means to a caller.
const cols = `id, remote, ref, status, attempts, error, coalesce(repo_id, '')`

func scan(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Remote, &j.Ref, &j.Status, &j.Attempts, &j.Error, &j.RepoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// Enqueue adds a job, or returns the active one for the same repository and
// ref. The dedupe is the partial unique index in 0005, so two gateways racing
// on the same submission produce one job rather than one job and one error.
//
// remote must be admit.Remote.URL. The index folds its case, which is what
// makes two spellings of one repository one job; a caller that passes a raw
// submission instead gets a dedupe that only matches exact strings.
func (q *Queue) Enqueue(ctx context.Context, remote, ref string) (Job, error) {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", remote, ref, time.Now().UnixNano()))
	id := hex.EncodeToString(sum[:16])

	// One statement: DO NOTHING plus a follow-up SELECT leaves a window in which
	// the conflicting job goes terminal and the read finds nothing, which would
	// surface as a nonsense ErrNotFound from Enqueue. The no-op DO UPDATE is what
	// makes RETURNING yield the existing row, and the index predicate guarantees
	// that row is active.
	return scan(q.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, remote, ref, status)
		VALUES ($1, $2, $3, 'pending')
		ON CONFLICT (lower(remote), ref) WHERE status IN ('pending','leased')
		DO UPDATE SET updated_at = jobs.updated_at
		RETURNING `+cols, id, remote, ref))
}

// Lease claims the oldest claimable job for d. SKIP LOCKED is what lets two
// indexers poll the same table without handing both the same row.
//
// leased_until means "not before this" in both states: on a leased job it is
// the lease expiry, on a pending one it is the retry time Fail set. A NULL is
// a job that has never been leased, since Lease always writes one.
//
// worker must be unique per process. Complete and Fail authorise on it, so two
// indexers sharing an id can silently finish each other's jobs: the ownership
// check passes and no error is raised anywhere.
func (q *Queue) Lease(ctx context.Context, worker string, d time.Duration) (Job, bool, error) {
	j, err := scan(q.pool.QueryRow(ctx, `
		WITH claimed AS (
			SELECT id FROM jobs
			WHERE status IN ('pending', 'leased')
			  AND (leased_until IS NULL OR leased_until < now())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE jobs SET
			status       = 'leased',
			attempts     = attempts + 1,
			leased_by    = $1,
			leased_until = now() + $2::interval,
			updated_at   = now()
		WHERE id IN (SELECT id FROM claimed)
		RETURNING `+cols,
		worker, fmt.Sprintf("%d milliseconds", d.Milliseconds())))
	if errors.Is(err, ErrNotFound) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// Complete marks the job done and records the repo it produced, but only for
// the worker still holding the lease. A worker whose lease expired has had its
// job reclaimed, and finishing it here would mark done a clone that another
// worker is still running — the exact crash path leases exist to survive.
//
// repoID is written here rather than at enqueue because it is hash(key,
// commit) and the commit is not known until the clone has run.
func (q *Queue) Complete(ctx context.Context, id, worker, repoID string) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs SET
			status='done', leased_by=NULL, leased_until=NULL, error='',
			repo_id=$3, updated_at=now()
		WHERE id=$1 AND leased_by=$2`, id, worker, repoID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotLeased
	}
	return nil
}

// Fail returns the job to the queue after a backoff, or marks it terminally
// failed once it has used its attempts. The reason is written on both paths,
// so a job waiting to retry says why it is waiting; Complete clears it, so a
// job that ends up succeeding keeps no trace of the attempts that did not.
// Lease ownership is enforced as it is in Complete — a zombie requeueing a job
// would let a third worker start it while the live one is still cloning.
//
// The backoff is leased_until on a pending row, which is the column Lease
// already reads: no new column, and the existing jobs_claimable_idx still
// serves the filter. It doubles with attempts and stops at RetryMax; the
// exponent is capped because an interval multiplied by an unbounded float
// overflows rather than saturating.
func (q *Queue) Fail(ctx context.Context, id, worker, reason string, maxAttempts int) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs SET
			status = CASE WHEN attempts >= $4 THEN 'failed' ELSE 'pending' END,
			leased_by = NULL,
			leased_until = CASE WHEN attempts >= $4 THEN NULL
				ELSE now() + least($5::interval * (2 ^ least(greatest(attempts - 1, 0), 16)), $6::interval)
			END,
			error = $3, updated_at = now()
		WHERE id = $1 AND leased_by = $2`, id, worker, reason, maxAttempts,
		fmt.Sprintf("%d milliseconds", q.RetryBase.Milliseconds()),
		fmt.Sprintf("%d milliseconds", q.RetryMax.Milliseconds()))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotLeased
	}
	return nil
}

// Sweep deletes terminal jobs that stopped changing more than keepFor ago and
// returns how many went. Spec §14 left the rule to P3; it is JOB_HISTORY_HOURS,
// 168h by default.
//
// By age, not by count, and never on re-enqueue. Dropping a repository's
// terminal jobs when it is submitted again — the cheap bound — 404s a job id a
// caller still holds, at a moment chosen by an unrelated stranger submitting
// the same URL, and a caller cannot tell that from an id that was never real.
// "Keep the newest N" does the same thing under a flood, to jobs that are
// minutes old and still being polled. An age window is the only one of the
// three that can be stated to a caller: a job id is readable for a week after
// it finishes.
//
// What it does not bound is a flood inside the window; that control is a rate
// limit on submission, which does not exist yet.
//
// status IN ('done','failed') is not a filter on the interesting rows, it is
// the safety property: a pending or leased row is the lease, and deleting one
// hands the same repository to a second worker with no record of the first,
// however old it is.
func (q *Queue) Sweep(ctx context.Context, keepFor time.Duration) (int, error) {
	tag, err := q.pool.Exec(ctx, `
		DELETE FROM jobs
		WHERE status IN ('done', 'failed')
		  AND updated_at < now() - $1::interval`,
		fmt.Sprintf("%d milliseconds", keepFor.Milliseconds()))
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	return scan(q.pool.QueryRow(ctx, `SELECT `+cols+` FROM jobs WHERE id = $1`, id))
}
