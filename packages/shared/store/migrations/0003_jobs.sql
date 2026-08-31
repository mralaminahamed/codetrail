-- The indexing queue. A table rather than a broker: one producer, durable,
-- retryable, and inspectable with psql.
CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,
    remote       TEXT NOT NULL,
    ref          TEXT NOT NULL,
    status       TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    leased_by    TEXT,
    leased_until TIMESTAMPTZ,
    error        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Submitting a repository that is already queued returns the existing job
-- rather than cloning it twice. Partial, so a finished job does not block a
-- later re-index of the same remote.
CREATE UNIQUE INDEX IF NOT EXISTS jobs_active_idx ON jobs (remote, ref)
    WHERE status IN ('pending', 'leased');

-- Lease() orders by created_at among claimable rows.
CREATE INDEX IF NOT EXISTS jobs_claimable_idx ON jobs (status, leased_until, created_at);
