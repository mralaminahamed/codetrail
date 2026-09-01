-- f1813be moved a repo's identity from the submitted URL to admit.Remote.Key,
-- which folds case. It shipped without a migration, so a row written before it
-- keys on the URL and the same repository at the same commit now hashes to a
-- second id: PutRepo's ON CONFLICT (id) misses the stale row and dies on
-- repos_remote_commit_sha_key instead. Measured — job e797a006 burned all three
-- attempts on SQLSTATE 23505, because retrying never removes the stale row.
--
-- Deleted rather than re-keyed. The corpus is a rebuildable cache that LRU
-- eviction already deletes from, and P1 has no reader for it, so re-indexing is
-- cheaper than a rewrite that has to guess which spelling each old id came
-- from. files and spans go with it by ON DELETE CASCADE; nothing else
-- references repos. jobs does not, and keeps its history: a done job whose rows
-- are gone is a true record of what ran.
DELETE FROM repos;

-- And the second identity that made this fatal instead of an upsert. remote is
-- deliberately not the identity now — it holds the first submitter's spelling
-- for display while Key decides what is one repository — and id is a function
-- of the row's own remote and commit_sha, so two rows agreeing on both agree on
-- id and ON CONFLICT (id) already arbitrates them. This constraint can only
-- fire when that function changes underneath the table, which is what happened.
ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_remote_commit_sha_key;
