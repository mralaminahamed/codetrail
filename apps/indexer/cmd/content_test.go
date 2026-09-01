package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/walk"
)

// Spec §3 says files.blob is git's own content hash, so git is the oracle. A
// hash of our own would agree with nothing: sha256 of the same bytes, or a
// sha1 without the "blob <n>\x00" framing, is a different string entirely.
func TestBlobHashMatchesGitHashObject(t *testing.T) {
	for _, body := range []string{"", "package main\n", "no trailing newline", "\x00\xff binary"} {
		f := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("git", "hash-object", f).Output()
		if err != nil {
			// git is a hard dependency of this binary — the clone step shells
			// out to it — so a skip here says something about the environment
			// rather than passing the check.
			t.Skipf("git unavailable: %v", err)
		}
		want := strings.TrimSpace(string(out))
		if got := blobHash([]byte(body)); got != want {
			t.Fatalf("%q: got %s, git says %s", body, got, want)
		}
	}
}

// Measured against pg17, which is what compose runs: a TEXT value holding a
// NUL is refused with `invalid byte sequence for encoding "UTF8": 0x00` and one
// holding an invalid byte with `... 0xff`. PutSpans writes a repo's spans in
// one transaction, so without this a single PNG fails the whole job.
func TestIndexableRefusesWhatPostgresRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		lang string
		body string
		want bool
	}{
		{"go source", "go", "package p\n", true},
		{"markdown", "markdown", "# hi\n", true},
		{"utf-8 prose", "markdown", "# héllo ☃\n", true},
		{"invalid utf8", "go", "package p\xff\xfe\n", false},
		{"embedded NUL", "go", "package p\x00\n", false},
		{"unclassified extension", "", "plain text\n", false},
		// The two halves are independent: a lockfile is valid UTF-8 and still
		// not worth an embedding call, and a .go file can hold bytes the
		// column refuses.
		{"unclassified and binary", "", "\x89PNG\x00", false},
	} {
		if got := indexable(walk.File{Lang: tc.lang}, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: indexable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
