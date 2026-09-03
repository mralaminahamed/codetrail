package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Spec §3 says files.blob is git's own content hash, so git is the oracle. A
// hash of our own would agree with nothing: sha256 of the same bytes, or a
// sha1 without the "blob <n>\x00" framing, is a different string entirely.
//
// Here rather than in the indexer because the eval re-hashes a checkout with
// the same function and compares against the column the indexer wrote; the
// oracle belongs beside the derivation both of them share.
func TestBlobHashMatchesGitHashObject(t *testing.T) {
	for _, body := range []string{"", "package main\n", "no trailing newline", "\x00\xff binary"} {
		f := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("git", "hash-object", f).Output()
		if err != nil {
			// git is a hard dependency of the indexer — the clone step shells
			// out to it — so a skip here says something about the environment
			// rather than passing the check.
			t.Skipf("git unavailable: %v", err)
		}
		want := strings.TrimSpace(string(out))
		if got := BlobHash([]byte(body)); got != want {
			t.Fatalf("%q: got %s, git says %s", body, got, want)
		}
	}
}
