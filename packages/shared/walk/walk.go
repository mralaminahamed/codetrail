// Package walk lists the indexable regular files of a checkout.
//
// In packages/shared rather than under apps/indexer because two apps now walk
// a checkout: the indexer, and the eval harness, which generates its questions
// from the same tree the indexer read (spec §9). Go's internal rule makes
// apps/indexer/internal/walk unimportable from apps/evalrunner, so the choice
// was this or a second copy of Files, ReadRegular, Lines and Indexable — and a
// second copy is the drift the source-binding check exists to catch. Lines is
// named because it WAS the copy: the harness held a correct one while this
// package counted terminators, so the two disagreed about how long a file is. Spec:45-46 lists
// packages/shared's contents and does not name walk; neither does it name
// admit, jobs or testdb, which are already there.
package walk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

var ErrTooManyFiles = errors.New("walk: file count exceeds the cap")

// ErrSkipped: this entry is not one to index, and the walk carries on. It
// covers both reasons ReadRegular declines, because the caller treats them
// alike — a file over the cap and a file that stopped being a file are each
// one entry missing from the index, not a reason to fail the repository.
//
// Exported with ReadRegular: the indexer reads each file a second time to
// chunk it, and has to tell "skip this one" from "the tree moved under us".
var ErrSkipped = errors.New("walk: skip this entry")

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
func Files(ctx context.Context, root string, lim Limits) ([]File, error) {
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
		// The job's one deadline (spec §6) reaches the walk here. Without it a
		// walk of a large tree runs to the end after the job that owns it has
		// expired, and the only thing that ends it is the lease being handed
		// to a second worker while this one is still reading.
		if err := ctx.Err(); err != nil {
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
		//
		// ReadRegular re-decides all of this on the open file descriptor, so
		// the two overlap — but only one way, measured: delete this check and
		// the whole suite still passes, so no test pins this line by itself;
		// delete ReadRegular's fstat instead and two fail. Keep it anyway — it
		// is the cheap path, and a device or fifo in the checkout is then never
		// opened at all.
		if !d.Type().IsRegular() {
			return nil
		}
		// Over the cap is a skip, not a failure: one generated blob should not
		// lose the repository. Over the file count is a failure, because it
		// means the caps were wrong for this repository — so the size check
		// has to come first, or an oversized file would spend cap budget.
		body, size, rerr := ReadRegular(p, lim.MaxFileBytes)
		if errors.Is(rerr, ErrSkipped) {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		if len(out) >= lim.MaxFiles {
			return fmt.Errorf("%w: more than %d", ErrTooManyFiles, lim.MaxFiles)
		}
		out = append(out, File{
			Path:  filepath.ToSlash(rel),
			Bytes: size,
			Lines: Lines(body),
			Lang:  langByExt[strings.ToLower(filepath.Ext(rel))],
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Lines counts the lines in body, the way a citation counts them.
//
// A terminator ENDS a line, it does not add one: counting '\n' alone is one
// too small for every file that does not end in one, and answers 0 for a
// one-line file with no terminator. That is not cosmetic, because files.lines
// is the only thing that says how long a file is while chunk names ranges out
// of the same bytes: "package b\nvar X = 1" recorded 1 line while the chunker
// emitted a span citing 1..1 — a file whose own row said the code it cites is
// not there.
//
// Exported and shared with apps/evalrunner for walk.Indexable's reason: the
// eval harness derives its rows from the same checkout the indexer read, and
// this counter was already reimplemented there, correctly, in a file whose doc
// says it mirrors the indexer's pipeline. Two derivations agree on the day
// they are written.
func Lines(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	n := bytes.Count(body, []byte{'\n'})
	if body[len(body)-1] != '\n' {
		n++
	}
	return n
}

// ReadRegular reads p, re-deciding on the file itself what the listing only
// claimed. It returns ErrSkipped when p is over max or is no longer a plain
// file, and any other error verbatim.
//
// The DirEntry above said "regular file", but that answer came out of the
// parent directory's listing, and nothing holds an attacker-controlled
// checkout still between then and this open. Two swaps are worth the flags:
//
//   - a regular file replaced by a symlink. O_NOFOLLOW refuses it, and the
//     ELOOP that comes back is the same verdict the listing would have given
//     a link. Measured: without it, a racing swapper leaks the contents of a
//     file outside the checkout.
//   - a regular file replaced by a fifo. os.ReadFile on one blocks until
//     someone writes, and the walk's context check fires between entries and
//     cannot interrupt a read already blocked, so that is a worker hung for
//     good. O_NONBLOCK returns instead of waiting, and the fstat then rejects
//     it for what it is.
//
// The fstat is what makes the check trustworthy: it describes the open file
// descriptor, so unlike an lstat on the path it cannot be raced.
//
// This closes the race on p's *final* component only. A parent directory
// swapped for a symlink is a wider hole and is still open: closing it needs
// the whole descent to open each component relative to a pinned root, which
// this does not do. Do not read these flags as closing it.
func ReadRegular(p string, max int64) ([]byte, int64, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, 0, ErrSkipped
		}
		// Every other open failure stays fatal, deliberately. A file that
		// vanished mid-walk means the tree moved under us, and an index that
		// quietly omits it still reports success. That is the same call the
		// unreadable-directory path already makes.
		return nil, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, ErrSkipped
	}
	if info.Size() > max {
		return nil, 0, ErrSkipped
	}
	// Bounded by the cap the size was just checked against, so a file that
	// grows after the fstat cannot make this read unbounded.
	body, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil, 0, err
	}
	return body, info.Size(), nil
}

// Indexable reports whether a file's bytes can become spans.
//
// The UTF-8 and NUL halves are not taste. Measured against pg17: a span text
// carrying a NUL is refused with `invalid byte sequence for encoding "UTF8":
// 0x00` and one carrying an invalid byte with `... 0xff`, and PutSpans writes
// a repo's spans in one transaction — so a single PNG would fail the whole
// job rather than cost one file.
//
// Exported and shared with apps/evalrunner rather than copied: spec §9's
// harness generates its questions from the same checkout the indexer read, and
// a walk that admitted a file the corpus does not contain would key every case
// to lines nothing indexed.
//
// The Lang half is the cost call (Open Question 5): a lockfile or a minified
// bundle has nothing retrievable in it and costs an embedding call per window.
func Indexable(f File, body []byte) bool {
	return f.Lang != "" && utf8.Valid(body) && !bytes.ContainsRune(body, 0)
}
