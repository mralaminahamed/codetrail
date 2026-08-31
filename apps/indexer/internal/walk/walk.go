// Package walk lists the indexable regular files of a checkout.
package walk

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrTooManyFiles = errors.New("walk: file count exceeds the cap")

type Limits struct {
	MaxFiles     int
	MaxFileBytes int64
}

type File struct {
	Path  string
	Bytes int64
	Lines int
	Lang  string
}

var langByExt = map[string]string{
	".go": "go", ".md": "markdown", ".sql": "sql",
	".ts": "typescript", ".tsx": "typescript", ".js": "javascript",
	".yml": "yaml", ".yaml": "yaml", ".json": "json",
}

// Files walks root and returns its regular files, repo-relative.
//
// It uses WalkDir, whose DirEntry comes from ReadDir and therefore describes
// the link itself rather than its target — the same guarantee as Lstat. A
// repository can contain `link -> /etc/passwd`, and following it would read
// and index the host's files.
func Files(root string, lim Limits) ([]File, error) {
	var out []File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			// The object database is enormous and meaningless to index.
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// IsRegular is true only when no type bit is set, so this rejects every
		// non-directory that is not a plain file — symlink, socket, device,
		// fifo, and whatever else the filesystem reports — without having to
		// enumerate them.
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if lim.MaxFileBytes > 0 && info.Size() > lim.MaxFileBytes {
			return nil
		}
		if lim.MaxFiles > 0 && len(out) >= lim.MaxFiles {
			return fmt.Errorf("%w: more than %d", ErrTooManyFiles, lim.MaxFiles)
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, File{
			Path:  filepath.ToSlash(rel),
			Bytes: info.Size(),
			Lines: bytes.Count(body, []byte{'\n'}),
			Lang:  langByExt[strings.ToLower(filepath.Ext(rel))],
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
