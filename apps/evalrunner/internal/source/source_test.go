package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// The indexer writes a row with an *empty* blob for a file that stopped being
// a readable regular file between the walk and the read — its `vanished`
// counter. Hashing the bytes here instead would make corpus.Verify refuse a
// corpus the indexer built correctly, and the refusal would name a path
// nothing is wrong with.
func TestAnUnreadFilesBlobIsEmptyJustAsTheIndexerLeavesIt(t *testing.T) {
	body := []byte("package p\n")
	got := Blobs([]File{
		{Path: "a.go", Lang: "go", Body: body, Read: true},
		{Path: "gone.go", Lang: "go", Read: false},
	})
	if want := store.BlobHash(body); got["a.go"] != want {
		t.Errorf("a.go's blob is %q, want %q", got["a.go"], want)
	}
	if got["gone.go"] != "" {
		t.Errorf("gone.go's blob is %q, want the empty string the indexer stores", got["gone.go"])
	}
	if _, ok := got["gone.go"]; !ok {
		t.Error("gone.go has no entry at all; the indexer keeps the row, so the file count still describes what the walk saw")
	}
}

// The generated set comes from the files the indexer would have chunked, by
// the indexer's own rule.
func TestIndexableIsTheIndexersOwnRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    File
		want bool
	}{
		{"go source", File{Path: "a.go", Lang: "go", Body: []byte("package p\n"), Read: true}, true},
		{"markdown", File{Path: "R.md", Lang: "markdown", Body: []byte("# hi\n"), Read: true}, true},
		{"unknown extension", File{Path: "a.bin", Lang: "", Body: []byte("x"), Read: true}, false},
		{"a NUL byte", File{Path: "a.go", Lang: "go", Body: []byte("a\x00b"), Read: true}, false},
		{"invalid utf-8", File{Path: "a.go", Lang: "go", Body: []byte{0xff}, Read: true}, false},
		{"never read", File{Path: "a.go", Lang: "go", Read: false}, false},
	} {
		if got := tc.f.Indexable(); got != tc.want {
			t.Errorf("%s: Indexable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// walk.Files, not filepath.Walk. A plain walk would generate cases from files
// the corpus does not contain, and would follow the escaping symlink P1 built
// a fixture for.
func TestReadWalksTheCheckoutTheWayTheIndexerDoes(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"a.go":     "package p\n",
		"NOTES.md": "# notes\n",
		"logo.png": "\x89PNG\x00\x01",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	files, err := Read(context.Background(), root, Limits())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if _, ok := byPath["escape.go"]; ok {
		t.Error("the walk followed a symlink out of the checkout")
	}
	if !byPath["a.go"].Indexable() || !byPath["NOTES.md"].Indexable() {
		t.Errorf("a.go and NOTES.md are %v and %v", byPath["a.go"].Indexable(), byPath["NOTES.md"].Indexable())
	}
	// A PNG is walked and gets a file row, and is not chunked: the same split
	// the indexer makes, and the reason the source-binding check compares
	// every walked file rather than only the ones that generate cases.
	png, ok := byPath["logo.png"]
	if !ok {
		t.Fatal("logo.png was not walked; its row is part of the corpus the blobs are compared against")
	}
	if png.Indexable() {
		t.Error("logo.png is indexable")
	}
	if Blobs(files)["logo.png"] == "" {
		t.Error("logo.png has no blob; the indexer hashes every file it read")
	}
}

// The defaults have to be the indexer's, or the harness reads a different file
// set from the one that was indexed.
func TestLimitsMirrorTheIndexersDefaults(t *testing.T) {
	l := Limits()
	if l.MaxFiles != 20000 || l.MaxFileBytes != 1<<20 {
		t.Errorf("Limits() is %+v, want MAX_REPO_FILES=20000 and MAX_FILE_BYTES=1MiB", l)
	}
}
