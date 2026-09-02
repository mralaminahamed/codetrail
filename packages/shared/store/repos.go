package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

var ErrNotFound = errors.New("store: not found")

// RepoID and FileID are content-addressed, which is what makes a retried
// index converge on the same rows instead of duplicating them.
//
// RepoID takes admit.Remote.Key, not the submitted URL. The forge decides what
// is one repository and it folds owner and name case, so keying on the URL let
// two spellings of one repository hold two rows at the same commit and two
// slots against the eviction quota. The key is the identity; repos.remote
// still holds a spelling someone actually submitted, for display.
func RepoID(key, commit string) string  { return hash(key, commit) }
func FileID(repoID, path string) string { return hash(repoID, path) }

// The trailing NUL after each part is a field separator, so ("ab","c") and
// ("a","bc") do not hash alike. Neither a URL nor a git path can contain a NUL,
// so no input can forge the separator.
func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// PutRepo writes a repo and its files in one transaction. Upserts throughout,
// so a job retried after a crash converges rather than failing on a conflict.
//
// It adds and updates; it does not delete. A second index of the same commit
// that produces a smaller file set — a cap lowered between the two runs, or a
// walk that skipped what it read last time — leaves the first run's rows
// behind, and file_count then disagrees with the row count. Recorded rather
// than fixed: P1 has no reader for either.
//
// It winds last_queried_at, which is what stops the indexer deleting its own
// work: runJob evicts straight after this write, so a re-index of the least
// recently used repo would otherwise drop the rows it just committed. Indexing
// is a use — someone asked for this repository — so it renews the lease the
// same way a query does.
func (s *Store) PutRepo(ctx context.Context, r models.Repo, files []models.File) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO repos (id, remote, ref, commit_sha, file_count, size_bytes, last_queried_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (id) DO UPDATE SET
			ref = EXCLUDED.ref, file_count = EXCLUDED.file_count,
			size_bytes = EXCLUDED.size_bytes, indexed_at = now(),
			last_queried_at = now()`,
		r.ID, r.Remote, r.Ref, r.Commit, len(files), r.SizeBytes); err != nil {
		return fmt.Errorf("repo: %w", err)
	}
	for _, f := range files {
		if _, err := tx.Exec(ctx, `
			INSERT INTO files (id, repo_id, path, blob, lang, lines)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO UPDATE SET
				blob = EXCLUDED.blob, lang = EXCLUDED.lang, lines = EXCLUDED.lines`,
			f.ID, f.RepoID, f.Path, f.Blob, f.Lang, f.Lines); err != nil {
			return fmt.Errorf("file %s: %w", f.Path, err)
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) GetRepo(ctx context.Context, id string) (models.Repo, error) {
	var r models.Repo
	err := s.pool.QueryRow(ctx,
		`SELECT id, remote, ref, commit_sha, size_bytes, indexed_at FROM repos WHERE id = $1`, id).
		Scan(&r.ID, &r.Remote, &r.Ref, &r.Commit, &r.SizeBytes, &r.IndexedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Repo{}, ErrNotFound
	}
	return r, err
}

// NewerCommit reports whether this repository's own ref is also indexed here
// at a different, later commit. That is the one staleness claim the corpus can
// prove: the ref moved, and here is where it moved to.
//
// It is not a freshness check and an empty result is not "up to date". A ref
// that moved on the forge and was never re-indexed leaves no trace in this
// table, and asking the forge would put a network call to a stranger's host on
// the gateway's read path. rag.Staleness is where that distinction is worded.
//
// The remotes are compared case-folded, the same fold jobs_active_idx uses and
// for the same reason: repos.remote holds the first submitter's spelling for
// display while identity is admit.Remote.Key, so two commits of one repository
// can carry two spellings. An id that names no row has no newer commit rather
// than an error — every caller is citing a repo row it has already read.
func (s *Store) NewerCommit(ctx context.Context, repoID string) (string, time.Time, error) {
	var (
		commit string
		at     time.Time
	)
	err := s.pool.QueryRow(ctx, `
		WITH me AS (
			SELECT remote, ref, commit_sha, indexed_at FROM repos WHERE id = $1
		)
		SELECT r.commit_sha, r.indexed_at
		FROM repos r, me
		WHERE lower(r.remote) = lower(me.remote)
		  AND r.ref = me.ref
		  AND r.commit_sha <> me.commit_sha
		  AND r.indexed_at > me.indexed_at
		ORDER BY r.indexed_at DESC, r.commit_sha
		LIMIT 1`, repoID).Scan(&commit, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return commit, at, err
}

// TouchRepo records a query against a repo. This is the LRU clock: eviction
// reads exactly this column, and PutRepo is the only other thing that winds it.
//
// The column is named for the query that was expected to be its only writer.
// It holds "last used", indexing included. P3 gave it its reader — the gateway
// touches on every successful read — and left the name alone: a rename is a
// migration and a rewrite of every query that reads it, for a word.
func (s *Store) TouchRepo(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE repos SET last_queried_at = now() WHERE id = $1`, id)
	return err
}
