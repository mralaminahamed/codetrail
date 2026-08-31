package clone

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fixture builds a real git repository on disk and returns a file:// URL.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "fixture")
	return "file://" + dir
}

func TestCloneReturnsTheCommitAndContents(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	dst := filepath.Join(t.TempDir(), "checkout")

	got, err := Run(context.Background(), remote, "HEAD", dst, Limits{MaxBytes: 1 << 20, Deadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Commit) != 40 {
		t.Fatalf("want a full commit sha, got %q", got.Commit)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "main.go")); err != nil || string(b) != "package main\n" {
		t.Fatalf("checkout is wrong: %v %q", err, b)
	}
}

// The cap has to be enforced, not merely declared.
func TestCloneRefusesAnOversizedRepository(t *testing.T) {
	big := make([]byte, 256*1024)
	for i := range big {
		big[i] = 'x'
	}
	remote := fixture(t, map[string]string{"big.txt": string(big)})
	dst := filepath.Join(t.TempDir(), "checkout")

	_, err := Run(context.Background(), remote, "HEAD", dst, Limits{MaxBytes: 64 * 1024, Deadline: time.Minute})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// And it must not leave the oversized checkout on disk.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("an over-cap clone must be removed, not left on disk")
	}
}

// A hung clone must be killed by the deadline rather than held forever.
func TestCloneRespectsTheDeadline(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	dst := filepath.Join(t.TempDir(), "checkout")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	if _, err := Run(ctx, remote, "HEAD", dst, Limits{MaxBytes: 1 << 20, Deadline: time.Minute}); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
}

// A nonexistent remote must fail promptly, not block on a credential prompt.
func TestCloneFailsOnAnUnreachableRemote(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "checkout")
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "file:///nonexistent-"+t.Name(), "HEAD", dst,
			Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("clone hung — GIT_TERMINAL_PROMPT is probably not set")
	}
}

// --- helpers for the tests below ---------------------------------------

func repoDir(remote string) string { return strings.TrimPrefix(remote, "file://") }

// gitIn runs git in dir and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const defaultLimit = 1 << 20

func limits(d time.Duration) Limits { return Limits{MaxBytes: defaultLimit, Deadline: d} }

// shimGit puts a fake git first on PATH. Most of what this package decides —
// the argv, the environment, the process group — has no visible effect on a
// local fixture, so the tests below watch what git was actually handed.
func shimGit(t *testing.T, script string) (record string) {
	t.Helper()
	bin := t.TempDir()
	record = filepath.Join(t.TempDir(), "record")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIM_RECORD", record)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// recordingGit writes its argv and environment, then fails.
const recordingGit = "#!/bin/sh\n{ printf 'ARG:%s\\n' \"$@\"; env; } > \"$SHIM_RECORD\"\nexit 1\n"

// hangingGit spawns a child that outlives it and then blocks, so that killing
// only the direct child leaves the grandchild running — and holding the pipe
// Run is reading. It records the child's pid so the test can bury it.
const hangingGit = "#!/bin/sh\nsleep 300 &\necho $! > \"$SHIM_RECORD\"\nwait\n"

// escapingGit puts its child in a new session, outside the process group the
// deadline kills, where it goes on holding the output pipe Run is reading.
const escapingGit = "#!/bin/sh\nsetsid sleep 300 &\necho $! > \"$SHIM_RECORD\"\nwait\n"

// writingGit creates the checkout it was asked for and then fails, so that the
// clone-failure cleanup path has something to remove.
const writingGit = "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\nmkdir -p \"$last\" && echo x > \"$last/f\"\nexit 1\n"

// headFailingGit clones successfully but fails every other subcommand, which
// is how the tests reach the rev-parse failure path.
const headFailingGit = "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\n" +
	"if [ \"$1\" = clone ]; then mkdir -p \"$last\" && echo x > \"$last/f\" && exit 0; fi\nexit 1\n"

// unreadableGit leaves behind a directory the size walk cannot read, which is
// how the tests reach the dirSize failure path.
const unreadableGit = "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\n" +
	"if [ \"$1\" = clone ]; then mkdir -p \"$last/locked\" && echo x > \"$last/f\" &&" +
	" chmod 000 \"$last/locked\" && exit 0; fi\nexit 1\n"

// sizedGit produces a checkout of exactly SHIM_BYTES bytes and a plausible
// head, so that the cap can be exercised at its boundary.
const sizedGit = "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\n" +
	"if [ \"$1\" = clone ]; then mkdir -p \"$last\" &&" +
	" head -c \"$SHIM_BYTES\" /dev/zero > \"$last/f\" && exit 0; fi\n" +
	"echo 1111111111111111111111111111111111111111\n"

func readRecord(t *testing.T, path string) (argv []string, env map[string]string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the shim recorded nothing: %v", err)
	}
	env = map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "ARG:"); ok {
			argv = append(argv, rest)
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return argv, env
}

// --- the containment decisions the tests above do not reach ------------

// The argv is a containment decision and most of it is invisible against a
// local fixture, so pin it exactly rather than by sampling.
func TestCloneArgvIsPinned(t *testing.T) {
	record := shimGit(t, recordingGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(time.Minute)); err == nil {
		t.Fatal("the shim exits 1; want an error")
	}
	argv, _ := readRecord(t, record)
	want := []string{
		"clone", "--quiet", "--depth", "1", "--single-branch",
		"--filter=blob:none", "--no-tags", "--", "file:///src", dst,
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", argv, want)
	}
}

func TestCloneArgvCarriesANonHeadRef(t *testing.T) {
	record := shimGit(t, recordingGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), "file:///src", "v1.2.3", dst, limits(time.Minute)); err == nil {
		t.Fatal("the shim exits 1; want an error")
	}
	argv, _ := readRecord(t, record)
	want := []string{
		"clone", "--quiet", "--depth", "1", "--single-branch",
		"--filter=blob:none", "--no-tags", "--branch", "v1.2.3", "--", "file:///src", dst,
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", argv, want)
	}
}

// The -- separator is what keeps a remote out of git's option parser. admit
// accepts only https:// URLs, so this shape cannot arrive through the API
// today; pin the separator anyway.
func TestCloneTreatsAnOptionLikeRemoteAsARepository(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "checkout")

	_, err := Run(context.Background(), "--version", "HEAD", dst, limits(time.Minute))
	if err == nil {
		t.Fatal("want an error")
	}
	// Without the separator git 2.43 answers "error: unknown option `version'"
	// and exits before it ever looks for a repository.
	if strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("git parsed the remote as an option: %v", err)
	}
}

func TestCloneEnvironmentIsPinned(t *testing.T) {
	record := shimGit(t, recordingGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(time.Minute)); err == nil {
		t.Fatal("the shim exits 1; want an error")
	}
	_, env := readRecord(t, record)
	for k, want := range map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "/bin/false",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"HOME":                filepath.Dir(dst),
	} {
		if env[k] != want {
			t.Errorf("child saw %s=%q, want %q", k, env[k], want)
		}
	}
}

// The deadline has to reach git's children. A killed parent that orphans a
// running fetch is still consuming the cap — and, here, still holding the
// output pipe that Run is waiting on.
func TestCloneDeadlineKillsGitsChildren(t *testing.T) {
	record := shimGit(t, hangingGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(2*time.Second))
		done <- err
	}()
	// Bury the child before waiting on Run, so that a failure here does not
	// leak the very process it is complaining about.
	pid := readPID(t, record)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from the deadline")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run never returned: git's child outlived it and is holding the output pipe")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d outlived the deadline: git's children are not in the killed process group", pid)
}

// The process group is not a guarantee that nothing escapes it. If something
// does, it still holds the output pipe, and without WaitDelay Run does not
// return late — it does not return at all.
func TestCloneReturnsWhenAChildEscapesTheProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is needed to put a child outside the process group")
	}
	record := shimGit(t, escapingGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(2*time.Second))
		done <- err
	}()
	pid := readPID(t, record)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from the deadline")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned: a child outside the process group held the output pipe")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the escaped child died anyway, so this run proves nothing about WaitDelay")
	}
}

func readPID(t *testing.T, record string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(record)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shim never recorded a pid (last read %q, %v)", b, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every failure path must remove the scratch directory: a failed job that
// leaves its checkout behind fills the disk one failure at a time.
func TestCloneRemovesTheCheckoutWhenGitFails(t *testing.T) {
	shimGit(t, writingGit)
	scratch := t.TempDir()
	dst := filepath.Join(scratch, "checkout")
	// Cleanup must take its own checkout and nothing else: the scratch root is
	// shared with whatever else the worker has in flight.
	sibling := filepath.Join(scratch, "another-job")
	if err := os.WriteFile(sibling, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(time.Minute)); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a failed clone must not leave its checkout on disk")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("cleanup removed more than its own checkout: %v", err)
	}
}

func TestCloneRemovesTheCheckoutWhenTheHeadLookupFails(t *testing.T) {
	shimGit(t, headFailingGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(time.Minute)); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a checkout whose head lookup failed must not be left on disk")
	}
}

func TestCloneRemovesTheCheckoutWhenItsSizeCannotBeMeasured(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable directory, so the walk would not fail")
	}
	shimGit(t, unreadableGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(time.Minute)); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a checkout that could not be measured must not be left on disk")
	}
}

// Both caps fail closed, alike, and before git is ever started: an unset
// deadline is not an unbounded one, and an unset size cap is not "unlimited".
func TestCloneRefusesANonPositiveDeadline(t *testing.T) {
	record := shimGit(t, recordingGit)
	for _, d := range []time.Duration{0, -time.Second} {
		dst := filepath.Join(t.TempDir(), "checkout")

		_, err := Run(context.Background(), "file:///src", "HEAD", dst,
			Limits{MaxBytes: defaultLimit, Deadline: d})
		if err == nil {
			t.Fatalf("Deadline %s: want a refusal", d)
		}
		if !strings.Contains(err.Error(), "Deadline") {
			t.Errorf("Deadline %s: the refusal must name the cap: %v", d, err)
		}
		assertGitNeverRan(t, record, dst)
	}
}

func TestCloneRefusesANonPositiveSizeCap(t *testing.T) {
	record := shimGit(t, recordingGit)
	for _, max := range []int64{0, -1} {
		dst := filepath.Join(t.TempDir(), "checkout")

		_, err := Run(context.Background(), "file:///src", "HEAD", dst,
			Limits{MaxBytes: max, Deadline: time.Minute})
		if err == nil {
			t.Fatalf("MaxBytes %d: want a refusal", max)
		}
		if !strings.Contains(err.Error(), "MaxBytes") {
			t.Errorf("MaxBytes %d: the refusal must name the cap: %v", max, err)
		}
		assertGitNeverRan(t, record, dst)
	}
}

func assertGitNeverRan(t *testing.T, record, dst string) {
	t.Helper()
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Error("git ran anyway: a cap that fails closed must refuse before the subprocess")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("nothing may be left behind")
	}
}

// ErrTooLarge means exceeds: a repository sitting exactly on the cap is not
// over it.
func TestCloneAcceptsARepositoryExactlyAtTheCap(t *testing.T) {
	t.Setenv("SHIM_BYTES", "4096")
	shimGit(t, sizedGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	got, err := Run(context.Background(), "file:///src", "HEAD", dst,
		Limits{MaxBytes: 4096, Deadline: time.Minute})
	if err != nil {
		t.Fatalf("a checkout exactly at the cap must be accepted: %v", err)
	}
	if got.Bytes != 4096 {
		t.Fatalf("Bytes = %d, want 4096", got.Bytes)
	}
}

// The scratch parent is Run's to create, and it is not for other users to read:
// a checkout of a stranger's repository sits in it.
func TestCloneCreatesThePrivateScratchParent(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	parent := filepath.Join(t.TempDir(), "scratch")
	dst := filepath.Join(parent, "checkout")

	if _, err := Run(context.Background(), remote, "HEAD", dst, limits(time.Minute)); err != nil {
		t.Fatalf("Run must create a missing scratch parent: %v", err)
	}
	fi, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("scratch parent mode %o, want 700", perm)
	}
}

// --depth 1: a long history must not arrive with the tip.
func TestCloneFetchesOnlyTheTipCommit(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	src := repoDir(remote)
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main // 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "commit", "-qam", "second")
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), remote, "HEAD", dst, limits(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := gitIn(t, dst, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("want a depth-1 checkout, got %s commits", n)
	}
}

func TestCloneChecksOutTheRequestedRef(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	src := repoDir(remote)
	gitIn(t, src, "checkout", "-qb", "sidebranch")
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main // side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "commit", "-qam", "side")
	gitIn(t, src, "checkout", "-q", "main")
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), remote, "sidebranch", dst, limits(time.Minute)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "main.go"))
	if err != nil || string(b) != "package main // side\n" {
		t.Fatalf("want the sidebranch content, got %v %q", err, b)
	}
}

// The Result is what the rest of the pipeline indexes from, so pin all three
// fields rather than the sha's length alone.
func TestCloneResultDescribesTheCheckout(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	dst := filepath.Join(t.TempDir(), "checkout")

	got, err := Run(context.Background(), remote, "HEAD", dst, limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != dst {
		t.Errorf("Dir = %q, want %q", got.Dir, dst)
	}
	if want := gitIn(t, repoDir(remote), "rev-parse", "HEAD"); got.Commit != want {
		t.Errorf("Commit = %q, want the remote's head %q", got.Commit, want)
	}
	want, err := dirSize(dst)
	if err != nil {
		t.Fatal(err)
	}
	if want == 0 || got.Bytes != want {
		t.Errorf("Bytes = %d, want %d", got.Bytes, want)
	}
}

func TestDirSizeCountsRegularFilesOnly(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(outside, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := dirSize(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != 15 {
		t.Fatalf("want 15 bytes of regular files, got %d", got)
	}
}
