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

type Job struct {
	ID       string `json:"id"`
	Remote   string `json:"remote"`
	Ref      string `json:"ref"`
	Status   Status `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

type Queue struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

const cols = `id, remote, ref, status, attempts, error`

func scan(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Remote, &j.Ref, &j.Status, &j.Attempts, &j.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// Enqueue adds a job, or returns the active one for the same remote and ref.
// The dedupe is the partial unique index in 0003, so two gateways racing on
// the same submission produce one job rather than one job and one error.
func (q *Queue) Enqueue(ctx context.Context, remote, ref string) (Job, error) {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", remote, ref, time.Now().UnixNano()))
	id := hex.EncodeToString(sum[:16])

	j, err := scan(q.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, remote, ref, status)
		VALUES ($1, $2, $3, 'pending')
		ON CONFLICT DO NOTHING
		RETURNING `+cols, id, remote, ref))
	if err == nil {
		return j, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Job{}, err
	}
	// ON CONFLICT DO NOTHING returned no row: an active job already exists.
	return scan(q.pool.QueryRow(ctx, `
		SELECT `+cols+` FROM jobs
		WHERE remote = $1 AND ref = $2 AND status IN ('pending','leased')`, remote, ref))
}

// Lease claims the oldest claimable job for d. SKIP LOCKED is what lets two
// indexers poll the same table without handing both the same row.
func (q *Queue) Lease(ctx context.Context, worker string, d time.Duration) (Job, bool, error) {
	j, err := scan(q.pool.QueryRow(ctx, `
		WITH claimed AS (
			SELECT id FROM jobs
			WHERE status = 'pending'
			   OR (status = 'leased' AND leased_until < now())
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

func (q *Queue) Complete(ctx context.Context, id string) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE jobs SET status='done', leased_by=NULL, leased_until=NULL, error='', updated_at=now() WHERE id=$1`, id)
	return err
}

// Fail returns the job to the queue, or marks it terminally failed once it
// has used its attempts. The reason is kept either way: a job that retried
// and then succeeded still explains why it retried.
func (q *Queue) Fail(ctx context.Context, id, reason string, maxAttempts int) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET
			status = CASE WHEN attempts >= $3 THEN 'failed' ELSE 'pending' END,
			leased_by = NULL, leased_until = NULL,
			error = $2, updated_at = now()
		WHERE id = $1`, id, reason, maxAttempts)
	return err
}

func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	return scan(q.pool.QueryRow(ctx, `SELECT `+cols+` FROM jobs WHERE id = $1`, id))
}
