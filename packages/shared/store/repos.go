package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

var ErrNotFound = errors.New("store: not found")

// RepoID and FileID are content-addressed, which is what makes a retried
// index converge on the same rows instead of duplicating them.
func RepoID(remote, commit string) string { return hash(remote, commit) }
func FileID(repoID, path string) string   { return hash(repoID, path) }

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
func (s *Store) PutRepo(ctx context.Context, r models.Repo, files []models.File) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO repos (id, remote, ref, commit_sha, file_count, size_bytes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			ref = EXCLUDED.ref, file_count = EXCLUDED.file_count,
			size_bytes = EXCLUDED.size_bytes, indexed_at = now()`,
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

// TouchRepo records a query against a repo. This is the LRU clock: eviction
// reads exactly this column.
func (s *Store) TouchRepo(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE repos SET last_queried_at = now() WHERE id = $1`, id)
	return err
}
