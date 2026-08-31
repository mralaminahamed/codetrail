package walk

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func paths(fs []File) map[string]bool {
	m := map[string]bool{}
	for _, f := range fs {
		m[f.Path] = true
	}
	return m
}

func TestListsRegularFilesWithRelativePaths(t *testing.T) {
	root := tree(t, map[string]string{
		"main.go":         "package main\n\nfunc main() {}\n",
		"internal/a/b.go": "package a\n",
		"README.md":       "# hi\n",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	for _, want := range []string{"main.go", "internal/a/b.go", "README.md"} {
		if !p[want] {
			t.Fatalf("missing %q, got %v", want, p)
		}
	}
	for _, f := range got {
		if filepath.IsAbs(f.Path) {
			t.Fatalf("paths must be repo-relative, got %q", f.Path)
		}
	}
}

// THE security test. A repository can contain `link -> /etc/passwd`; a walker
// that follows it reads and indexes the host's files.
func TestNeverFollowsSymlinks(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Skip("symlinks unavailable on this platform")
	}
	if err := os.Symlink("/etc", filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	if p["escape.txt"] || p["etc"] {
		t.Fatalf("a symlink was walked: %v", p)
	}
	for _, f := range got {
		if f.Path != "main.go" {
			t.Fatalf("unexpected entry %q — only regular files may be listed", f.Path)
		}
	}
}

// .git holds the object database; indexing it is pointless and enormous.
func TestSkipsDotGit(t *testing.T) {
	root := tree(t, map[string]string{
		"main.go":              "package main\n",
		".git/config":          "[core]\n",
		".git/objects/aa/bbbb": "binary",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if len(f.Path) >= 4 && f.Path[:4] == ".git" {
			t.Fatalf("walked into .git: %q", f.Path)
		}
	}
}

func TestCountsLinesAndDetectsLanguage(t *testing.T) {
	root := tree(t, map[string]string{
		"a.go":  "package a\nfunc F() {}\n",
		"b.md":  "# t\n",
		"c.bin": "\x00\x01\x02",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]File{}
	for _, f := range got {
		by[f.Path] = f
	}
	if by["a.go"].Lang != "go" {
		t.Fatalf("want lang go, got %q", by["a.go"].Lang)
	}
	if by["a.go"].Lines != 2 {
		t.Fatalf("want 2 lines, got %d", by["a.go"].Lines)
	}
	if by["b.md"].Lang != "markdown" {
		t.Fatalf("want lang markdown, got %q", by["b.md"].Lang)
	}
}

// A file over the cap is skipped, not fatal: one enormous generated file
// should not lose the repository.
func TestSkipsOversizedFiles(t *testing.T) {
	big := make([]byte, 4096)
	root := tree(t, map[string]string{"small.go": "package a\n", "huge.go": string(big)})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	if p["huge.go"] {
		t.Fatal("an over-cap file must be skipped")
	}
	if !p["small.go"] {
		t.Fatal("an over-cap file must not lose the rest of the repository")
	}
}

// Too many files IS fatal: it means the caps were wrong for this repository,
// and half an index is worse than none.
func TestRefusesTooManyFiles(t *testing.T) {
	files := map[string]string{}
	for i := range 20 {
		files[string(rune('a'+i))+".go"] = "package a\n"
	}
	_, err := Files(tree(t, files), Limits{MaxFiles: 5, MaxFileBytes: 1 << 20})
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("want ErrTooManyFiles, got %v", err)
	}
}
