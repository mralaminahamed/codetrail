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
	"unicode/utf8"
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
// It uses WalkDir, which lstats root and reads every descendant out of its
// parent's directory listing. Neither resolves a link, so the DirEntry
// describes the link itself and never its target. A repository can contain
// `link -> /etc/passwd`, and following it would read and index the host's
// files.
func Files(root string, lim Limits) ([]File, error) {
	// Both caps fail closed, the way clone's do: zero is a refusal, not
	// "unlimited". An int-valued config knob that parses to 0 must not arrive
	// here as permission to index a repository of any size.
	if lim.MaxFiles <= 0 {
		return nil, fmt.Errorf("walk: MaxFiles must be positive, got %d", lim.MaxFiles)
	}
	if lim.MaxFileBytes <= 0 {
		return nil, fmt.Errorf("walk: MaxFileBytes must be positive, got %d", lim.MaxFileBytes)
	}

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
		// Git stores path bytes verbatim, so a checkout can hold a name that is
		// not UTF-8. encoding/json does not reject one, it substitutes U+FFFD,
		// so such a path would be served back naming a file that does not
		// exist. Measured, not assumed: marshalling a name with invalid bytes
		// returns no error and a string that no longer round-trips.
		if !utf8.ValidString(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// As a directory .git is the object database: enormous, and meaningless
		// to index. As a regular file it is a gitlink, whose one line points at
		// a gitdir elsewhere on this host — also not the repository's content.
		// The name is matched exactly, so .github and .gitignore are unaffected.
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
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
		// Over the cap is a skip, not a failure: one generated blob should not
		// lose the repository. Over the file count is a failure, because it
		// means the caps were wrong for this repository.
		if info.Size() > lim.MaxFileBytes {
			return nil
		}
		if len(out) >= lim.MaxFiles {
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
