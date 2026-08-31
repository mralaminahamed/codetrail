package walk

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skip("symlinks unavailable on this platform")
	}
}

// Not a guard in this file: filepath.WalkDir reads each directory's own
// listing and never resolves a link, so nothing here can be mutated to make
// this test fail for the reason it states — under the realistic follow-links
// mutant (d.Type/d.Info -> os.Stat) it passes outright, and under the
// IsRegular mutant it dies on EISDIR from reading `vendor`, not on its own
// assertion. It is a regression pin on that stdlib property, which the whole
// package rests on. The target is populated so that a regression leaks
// something visible rather than nothing.
func TestNeverDescendsIntoALinkedDirectory(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	outside := tree(t, map[string]string{"leak.go": "package leak\n", "deep/er.go": "package er\n"})
	link(t, outside, filepath.Join(root, "vendor"))

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if p := paths(got); len(p) != 1 || !p["main.go"] {
		t.Fatalf("walked a linked directory: %v", p)
	}
}

func TestSurvivesASymlinkLoop(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	link(t, filepath.Join(root, "b"), filepath.Join(root, "a"))
	link(t, filepath.Join(root, "a"), filepath.Join(root, "b"))

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if p := paths(got); len(p) != 1 || !p["main.go"] {
		t.Fatalf("want only main.go, got %v", p)
	}
}

func TestALinkBackIntoTheTreeDoesNotRecurse(t *testing.T) {
	root := tree(t, map[string]string{"sub/x.go": "package x\n"})
	link(t, root, filepath.Join(root, "sub", "up"))

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if p := paths(got); len(p) != 1 || !p["sub/x.go"] {
		t.Fatalf("want only sub/x.go, got %v", p)
	}
}

// /dev/zero never ends; a walker that reads it does not come back.
func TestSkipsADeviceAndADanglingLink(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	link(t, "/dev/zero", filepath.Join(root, "zero"))
	link(t, "/nonexistent/nope", filepath.Join(root, "dangling"))

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if p := paths(got); len(p) != 1 || !p["main.go"] {
		t.Fatalf("want only main.go, got %v", p)
	}
}

// Reading a fifo blocks until someone writes to it. Nobody will.
func TestSkipsAFifo(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	if err := exec.Command("mkfifo", filepath.Join(root, "pipe")).Run(); err != nil {
		t.Skip("mkfifo unavailable")
	}
	done := make(chan struct{})
	var got []File
	var ferr error
	go func() {
		got, ferr = Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the walk blocked on a fifo")
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
	if p := paths(got); len(p) != 1 || !p["main.go"] {
		t.Fatalf("want only main.go, got %v", p)
	}
}

// Two guards would each stop this on their own — the name check it reaches
// first, and the IsRegular check it would reach if the name check were gone —
// so it kills neither in isolation and is not evidence for either. It pins the
// composite: whatever `.git` is, a populated gitdir outside the checkout stays
// out of the index.
func TestSkipsADotGitThatIsASymlink(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	real := tree(t, map[string]string{"config": "[core]\n", "objects/aa/bbbb": "binary"})
	link(t, real, filepath.Join(root, ".git"))

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if p := paths(got); len(p) != 1 || !p["main.go"] {
		t.Fatalf("walked a linked .git: %v", p)
	}
}

// A .git *file* is a gitlink: its one line points at a gitdir elsewhere on
// this host — git writes a relative path for a submodule (`gitdir:
// ../.git/modules/mod`) and an absolute one elsewhere, and neither is the
// repository's content or fit for a public index.
func TestSkipsADotGitThatIsAFile(t *testing.T) {
	root := tree(t, map[string]string{
		".git":    "gitdir: /home/victim/private/.git\n",
		"main.go": "package main\n",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	if p[".git"] {
		t.Fatalf("a gitlink file was indexed: %v", p)
	}
	// Skipping the entry, not the directory it sits in: a SkipDir here would
	// drop the whole checkout and still satisfy the assertion above.
	if !p["main.go"] {
		t.Fatalf("skipping the gitlink cost the rest of the checkout: %v", p)
	}
}

// The skip is on the exact name: .github and .gitignore are repository content.
func TestDoesNotSkipDotGitLookalikes(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":               "vendor\n",
		".github/workflows/ci.yml": "on: push\n",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	for _, want := range []string{".gitignore", ".github/workflows/ci.yml"} {
		if !p[want] {
			t.Fatalf("missing %q, got %v", want, p)
		}
	}
}

// Git stores path bytes verbatim, so a repository can carry a name that is not
// UTF-8. encoding/json does not reject one — it substitutes U+FFFD — so an
// indexed path would come back over the API naming a file that does not exist.
func TestSkipsPathsThatAreNotUTF8(t *testing.T) {
	root := tree(t, map[string]string{
		"main.go":            "package main\n",
		"bad\xff\xfename.go": "package bad\n",
		"d\xffir/inner.go":   "package inner\n",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if !utf8.ValidString(f.Path) {
			t.Fatalf("emitted a path that is not UTF-8: %q", f.Path)
		}
	}
	if p := paths(got); len(p) != 1 || !p["main.go"] {
		t.Fatalf("want only main.go, got %v", p)
	}
}

// A cap of zero is a refusal, not permission to index a repository of any
// size: an int-valued config knob that parses to 0 must not disarm the cap.
func TestRefusesNonPositiveCaps(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	for _, lim := range []Limits{
		{},
		{MaxFiles: 0, MaxFileBytes: 1 << 20},
		{MaxFiles: -1, MaxFileBytes: 1 << 20},
		{MaxFiles: 100, MaxFileBytes: 0},
		{MaxFiles: 100, MaxFileBytes: -1},
	} {
		_, err := Files(root, lim)
		if err == nil {
			t.Fatalf("%+v was accepted", lim)
		}
		// And it must be refused as a misconfiguration, not reported as
		// ErrTooManyFiles: a zero cap that trips the counter on the first file
		// would blame the repository for the operator's mistake.
		if errors.Is(err, ErrTooManyFiles) {
			t.Fatalf("%+v was refused as a repository verdict: %v", lim, err)
		}
	}
}

// The size cap means "over", not "at": a file of exactly MaxFileBytes stays.
func TestSizeCapIsExclusive(t *testing.T) {
	root := tree(t, map[string]string{
		"at.go":   string(make([]byte, 1024)),
		"over.go": string(make([]byte, 1025)),
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	if !p["at.go"] {
		t.Fatal("a file of exactly MaxFileBytes must be kept")
	}
	if p["over.go"] {
		t.Fatal("a file of MaxFileBytes+1 must be skipped")
	}
}

// And the file cap the same way: exactly MaxFiles is fine, one more is fatal.
func TestFileCapIsReachableButNotExceedable(t *testing.T) {
	five := map[string]string{}
	for i := range 5 {
		five[string(rune('a'+i))+".go"] = "package a\n"
	}
	got, err := Files(tree(t, five), Limits{MaxFiles: 5, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatalf("exactly MaxFiles must be accepted, got %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5 files, got %d", len(got))
	}
	six := map[string]string{}
	for i := range 6 {
		six[string(rune('a'+i))+".go"] = "package a\n"
	}
	if _, err := Files(tree(t, six), Limits{MaxFiles: 5, MaxFileBytes: 1 << 20}); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("MaxFiles+1 must be fatal, got %v", err)
	}
}

func TestRecordsByteSizeAndLowercasesTheExtension(t *testing.T) {
	root := tree(t, map[string]string{"MAIN.GO": "package main\n", "s.md": "ab"})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]File{}
	for _, f := range got {
		by[f.Path] = f
	}
	if by["MAIN.GO"].Lang != "go" {
		t.Fatalf("an uppercase extension must still detect, got %q", by["MAIN.GO"].Lang)
	}
	if by["MAIN.GO"].Bytes != 13 {
		t.Fatalf("want 13 bytes, got %d", by["MAIN.GO"].Bytes)
	}
	if by["s.md"].Bytes != 2 {
		t.Fatalf("want 2 bytes, got %d", by["s.md"].Bytes)
	}
}

// A directory the walk cannot read is fatal, not a silent gap: a half index
// that claims to be whole is worse than a job that failed.
func TestAnUnreadableDirectoryIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory regardless of mode")
	}
	root := tree(t, map[string]string{"main.go": "package main\n", "locked/y.go": "package y\n"})
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	if _, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20}); err == nil {
		t.Fatal("an unreadable directory must fail the walk")
	}
}

// A repo-relative path is not enough on its own: ".." would be repo-relative
// and still name a file outside the checkout.
func TestPathsStayInsideTheCheckout(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n", "a/b/c.go": "package c\n"})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if filepath.IsAbs(f.Path) || f.Path == ".." || strings.HasPrefix(f.Path, "../") {
			t.Fatalf("path escapes the checkout: %q", f.Path)
		}
	}
}

// The brief's symlink test carries both a file link and a link to /etc, and
// the /etc one errors first (EISDIR) — so a follow-the-link mutant dies there
// before the assertion that names the escape ever runs. This isolates the file
// link, whose target is a real file outside the tree with known content, so
// the failure says what actually went wrong.
func TestALinkToAFileOutsideTheTreeIsNotIndexed(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link(t, outside, filepath.Join(root, "escape.txt"))

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if p := paths(got); len(p) != 1 || !p["main.go"] {
		t.Fatalf("a link to a file outside the checkout was indexed: %v", p)
	}
}

// The DirEntry's "regular file" came from the parent directory's listing, and
// nothing holds an attacker-controlled checkout still afterwards. These pin
// what readRegular re-decides on the file itself, deterministically; the two
// racing tests below show the same swaps happening for real.
func TestReadRegularRefusesALinkAndReadsNothingThroughIt(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "bait.txt")
	link(t, outside, p)

	body, size, err := readRegular(p, 1<<20)
	if !errors.Is(err, errSkipFile) {
		t.Fatalf("want errSkipFile, got body=%q size=%d err=%v", body, size, err)
	}
}

func TestReadRegularRefusesAFifoWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pipe")
	if err := exec.Command("mkfifo", p).Run(); err != nil {
		t.Skip("mkfifo unavailable")
	}
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() {
		_, _, err := readRegular(p, 1<<20)
		ch <- res{err}
	}()
	select {
	case r := <-ch:
		if !errors.Is(r.err, errSkipFile) {
			t.Fatalf("want errSkipFile, got %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readRegular blocked on a fifo")
	}
}

// /dev/zero never reaches EOF: a read of it returns only when memory runs out.
func TestReadRegularRefusesADevice(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("/dev/zero unavailable")
	}
	if _, _, err := readRegular("/dev/zero", 1<<20); !errors.Is(err, errSkipFile) {
		t.Fatalf("want errSkipFile, got %v", err)
	}
}

// The decision, made explicitly rather than inherited: only "this stopped
// being a file to index" is a skip. A file that vanished mid-walk means the
// tree moved under us, and an index that quietly omits it still reports
// success — so it stays fatal, as an unreadable directory already does.
func TestReadRegularTreatsAVanishedFileAsFatal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gone.go")
	_, _, err := readRegular(p, 1<<20)
	if err == nil {
		t.Fatal("want an error for a file that is not there")
	}
	if errors.Is(err, errSkipFile) {
		t.Fatalf("a vanished file must not be silently skipped, got %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

// swapper flips path between two forms until stop closes, using rename so the
// path is never briefly absent — the race under test is the swap, not a gap.
func swapper(t *testing.T, dir, name string, make1, make2 func(tmp string), stop <-chan struct{}) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	target := filepath.Join(dir, name)
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tmp := filepath.Join(dir, fmt.Sprintf(".swap%d", i%2))
			os.RemoveAll(tmp)
			if i%2 == 0 {
				make1(tmp)
			} else {
				make2(tmp)
			}
			os.Rename(tmp, target)
		}
	}()
	return done
}

// Measured before the fix: a racing swap leaked the outside file's contents
// through os.ReadFile. This asserts only on an actual leak, so losing the race
// makes it pass rather than flake.
func TestAFileSwappedForALinkMidWalkIsNotRead(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	outside := filepath.Join(t.TempDir(), "secret.txt")
	// 64 lines, so a leaked read is unmistakable next to the bait's one.
	if err := os.WriteFile(outside, []byte(strings.Repeat("SECRET\n", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".probe")); err != nil {
		t.Skip("symlinks unavailable on this platform")
	}
	os.Remove(filepath.Join(root, ".probe"))

	stop := make(chan struct{})
	done := swapper(t, root, "bait.txt",
		func(tmp string) { os.WriteFile(tmp, []byte("package bait\n"), 0o644) },
		func(tmp string) { os.Symlink(outside, tmp) },
		stop)
	defer func() { close(stop); <-done }()

	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
		if err != nil {
			continue // a swap can legitimately race the walk into an error
		}
		for _, f := range got {
			if f.Path == "bait.txt" && f.Lines != 1 {
				t.Fatalf("walk %d read through a link swapped in mid-walk: %+v", i, f)
			}
		}
	}
}

// And the swap that a "copy a regular file in" attacker cannot do: os.ReadFile
// on a fifo never returns, and Files has no deadline of its own.
func TestAFileSwappedForAFifoMidWalkDoesNotHang(t *testing.T) {
	if err := exec.Command("mkfifo", filepath.Join(t.TempDir(), "x")).Run(); err != nil {
		t.Skip("mkfifo unavailable")
	}
	root := tree(t, map[string]string{"main.go": "package main\n"})
	stop := make(chan struct{})
	done := swapper(t, root, "bait.txt",
		func(tmp string) { os.WriteFile(tmp, []byte("package bait\n"), 0o644) },
		func(tmp string) { exec.Command("mkfifo", tmp).Run() },
		stop)
	defer func() { close(stop); <-done }()

	walks := make(chan struct{})
	go func() {
		defer close(walks)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
		}
	}()
	select {
	case <-walks:
	case <-time.After(60 * time.Second):
		t.Fatal("the walk hung on a fifo swapped in mid-walk")
	}
}

// The cap is checked against fstat's size, and fstat is not always right: a
// procfs file reports size 0 and then reads out real bytes. Nothing in a
// checkout can be one — O_NOFOLLOW refuses a link to it — but it makes the
// point deterministically that the read must be bounded by the cap and not by
// what the stat claimed, which otherwise only matters when a file grows
// between the two.
func TestReadIsBoundedByTheCapNotByTheStatSize(t *testing.T) {
	const proc = "/proc/self/environ"
	info, err := os.Stat(proc)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Skip("no size-0 regular file with content available here")
	}
	body, size, err := readRegular(proc, 8)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Fatalf("want the stat size, got %d", size)
	}
	if len(body) > 8 {
		t.Fatalf("read %d bytes past a cap of 8", len(body))
	}
}
