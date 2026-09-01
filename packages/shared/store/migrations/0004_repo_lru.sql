-- The eviction clock and what an operator reads to see whether an index did
-- anything. Guarded like 0003: applying a migration twice is the normal path,
-- not an edge case, and the ledger should not be the only thing preventing it.
ALTER TABLE repos ADD COLUMN IF NOT EXISTS last_queried_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE repos ADD COLUMN IF NOT EXISTS size_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS file_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'ready';
CREATE INDEX IF NOT EXISTS repos_lru_idx ON repos (last_queried_at);
