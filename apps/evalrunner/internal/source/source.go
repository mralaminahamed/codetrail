// Package source reads the checkout the golden questions are generated from.
//
// It walks with the indexer's own walk.Files and filters with its own
// walk.Indexable, not with filepath.Walk. Verified: Indexable is
// `f.Lang != "" && utf8.Valid(body) && !bytes.ContainsRune(body, 0)`, so a
// plain walk generates cases from files the corpus does not contain — and
// walk.Files is also what refuses to follow the escaping symlink P1 built a
// fixture for, which a harness pointed at a stranger's checkout has exactly as
// much reason to care about as the indexer does.
//
// The walk is the belt; corpus.Verify's blob comparison is the braces.
package source

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/walk"
)

// Limits mirrors the indexer's defaults, so a harness pointed at a checkout
// reads the same file set the indexer wrote. MAX_REPO_FILES and MAX_FILE_BYTES
// are 20000 and 1 MiB there.
func Limits() walk.Limits { return walk.Limits{MaxFiles: 20000, MaxFileBytes: 1 << 20} }

// File is one walked file and the bytes the indexer would have read from it.
type File struct {
	Path string
	Lang string
	Body []byte
	// Read is false when walk.ReadRegular refused the entry between the walk
	// and the read. The indexer keeps the row, with an empty blob and no
	// spans, and so does this — otherwise the source-binding check would
	// refuse a corpus the indexer built correctly.
	Read bool
}

// Indexable reports whether this file's bytes become spans, by the indexer's
// own rule.
func (f File) Indexable() bool {
	return f.Read && walk.Indexable(walk.File{Path: f.Path, Lang: f.Lang}, f.Body)
}

// Read walks root and reads every regular file the indexer would have read, in
// the walk's own order.
func Read(ctx context.Context, root string, lim walk.Limits) ([]File, error) {
	walked, err := walk.Files(ctx, root, lim)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(walked))
	for _, w := range walked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := File{Path: w.Path, Lang: w.Lang}
		body, _, err := walk.ReadRegular(filepath.Join(root, filepath.FromSlash(w.Path)), lim.MaxFileBytes)
		switch {
		case err == nil:
			f.Body, f.Read = body, true
		case errors.Is(err, walk.ErrSkipped):
			// Left unread, exactly as the indexer leaves it.
		default:
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Blobs is path to git blob hash, matching files.blob for every row the
// indexer wrote. An unread file's blob is the empty string, which is what the
// indexer stores for one that vanished between the walk and the read.
func Blobs(files []File) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		if !f.Read {
			out[f.Path] = ""
			continue
		}
		out[f.Path] = store.BlobHash(f.Body)
	}
	return out
}
