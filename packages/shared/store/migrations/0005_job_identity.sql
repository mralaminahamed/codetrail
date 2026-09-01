-- Dedupe on the repository, not on its spelling. A forge matches owner and
-- name case-insensitively, so github.com/octocat/Spoon-Knife and
-- github.com/OctoCat/spoon-knife are one repository and were two queued jobs.
--
-- lower(remote) rather than a stored key column: Enqueue is only ever handed
-- admit's normalised URL, whose segments are ASCII, so this and the key the
-- indexer derives from the same URL fold identically.
-- An existing queue can already hold two spellings of one repository as two
-- active jobs, and the index below cannot be created over them. Retire the
-- later ones: the oldest is the job that will run, and its duplicate would
-- only have cloned the same repository a second time.
UPDATE jobs SET
    status = 'failed', leased_by = NULL, leased_until = NULL,
    error = 'superseded by an earlier job for the same repository', updated_at = now()
WHERE status IN ('pending', 'leased')
  AND id <> (
    SELECT dup.id FROM jobs dup
    WHERE dup.status IN ('pending', 'leased')
      AND lower(dup.remote) = lower(jobs.remote) AND dup.ref = jobs.ref
    ORDER BY dup.created_at, dup.id
    LIMIT 1
  );

DROP INDEX IF EXISTS jobs_active_idx;
CREATE UNIQUE INDEX IF NOT EXISTS jobs_active_idx ON jobs (lower(remote), ref)
    WHERE status IN ('pending', 'leased');
