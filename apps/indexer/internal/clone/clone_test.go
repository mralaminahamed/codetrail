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
//
// It ignores SIGTERM first, and an ignored disposition survives exec, so the
// sleep inherits it: a fixture that dies on any signal cannot tell a group
// SIGKILL from a group SIGTERM, and would let the weaker signal pass.
const hangingGit = "#!/bin/sh\ntrap \"\" TERM\nsleep 300 &\necho $! > \"$SHIM_RECORD\"\nwait\n"

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

// headRecordingGit clones successfully and records what the rev-parse was
// handed, which is the one fork in this package no test used to watch.
const headRecordingGit = "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\n" +
	"if [ \"$1\" = clone ]; then mkdir -p \"$last\" && echo x > \"$last/f\" && exit 0; fi\n" +
	"{ printf 'ARG:%s\\n' \"$@\"; env; } > \"$SHIM_RECORD\"\nexit 1\n"

// headEscapingGit clones successfully, then puts the rev-parse's child in a
// new session where it goes on holding the output pipe head() is reading.
const headEscapingGit = "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\n" +
	"if [ \"$1\" = clone ]; then mkdir -p \"$last\" && echo x > \"$last/f\" && exit 0; fi\n" +
	"setsid sleep 300 &\necho $! > \"$SHIM_RECORD\"\nwait\n"

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

// The rev-parse gets the same containment the clone does. It is a separate
// test because it was a separate fork: head() built its own exec.Cmd and
// therefore ran with none of gitCmd's five overrides, no process group and no
// WaitDelay, while TestCloneEnvironmentIsPinned watched only the clone and
// passed throughout.
func TestHeadRunsUnderTheSameGitEnvironmentAsClone(t *testing.T) {
	record := shimGit(t, headRecordingGit)
	dst := filepath.Join(t.TempDir(), "checkout")

	if _, err := Run(context.Background(), "file:///src", "HEAD", dst, limits(time.Minute)); err == nil {
		t.Fatal("the shim fails every rev-parse; want an error")
	}
	argv, env := readRecord(t, record)
	// The control: without this the assertions below would pass against the
	// clone's own record if the rev-parse never ran at all.
	if !slices.Contains(argv, "rev-parse") {
		t.Fatalf("the record is not the rev-parse's: %v", argv)
	}
	for k, want := range map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "/bin/false",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"HOME":                filepath.Dir(dst),
	} {
		if env[k] != want {
			t.Errorf("the rev-parse saw %s=%q, want %q", k, env[k], want)
		}
	}
	// The parent environment is still inherited: gitCmd closes five variables
	// and no more, and routing head through it must not have narrowed that.
	if env["PATH"] == "" {
		t.Error("the rev-parse inherited no PATH, so its environment is no longer append(os.Environ(), …)")
	}
}

// The half of gitCmd that only this call site can lose, and the reason its doc
// says WaitDelay is there: cmd.Output() waits on the output pipe, and waiting
// on that pipe has no timeout of its own. With a descendant outside the
// process group holding it open, a hand-built exec.Cmd does not return late —
// it does not return at all.
func TestCloneReturnsWhenTheRevParseLeaksAChild(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is needed to put a child outside the process group")
	}
	record := shimGit(t, headEscapingGit)
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
		t.Fatal("Run never returned: the rev-parse's escaped child held the output pipe, and head() waited on it forever")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the escaped child died anyway, so this run proves nothing about WaitDelay")
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

// An empty repository is a normal thing for a stranger to submit: an
// allowlisted forge serves one happily, the clone succeeds, and only the head
// lookup fails — so that failure has to carry git's own message rather than a
// bare exit status.
func TestCloneOnAnEmptyRepositoryExplainsItself(t *testing.T) {
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main", ".")
	dst := filepath.Join(t.TempDir(), "checkout")

	_, err := Run(context.Background(), "file://"+src, "HEAD", dst, limits(time.Minute))
	if err == nil {
		t.Fatal("want an error from the head lookup")
	}
	if !strings.Contains(err.Error(), "fatal:") {
		t.Fatalf("the failure must carry git's own message, got %q", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("the empty checkout must not be left on disk")
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

// ---- Resolve --------------------------------------------------------------

// twoBranches builds a repository carrying BOTH refs/heads/main and
// refs/heads/a/main, at different commits.
//
// This is the whole point of the fixture. `git ls-remote --heads <url> main` is
// a TAIL match, so it returns both, and a/main sorts FIRST — a Resolve that
// read line one would return the wrong commit. A single-branch fixture matches
// once and looks correct forever.
func twoBranches(t *testing.T) (url, mainSHA, aMainSHA string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
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
	run("init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "one")
	mainSHA = run("rev-parse", "HEAD")

	run("checkout", "-q", "-b", "a/main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "two")
	aMainSHA = run("rev-parse", "HEAD")
	run("checkout", "-q", "main")

	if mainSHA == aMainSHA {
		t.Fatal("the two branches are at one commit; this fixture cannot separate them")
	}
	return "file://" + dir, mainSHA, aMainSHA
}

func TestResolveReturnsFortyLowercaseHexJustAsRevParseDoes(t *testing.T) {
	url, want, _ := twoBranches(t)
	got, err := Resolve(context.Background(), url, "main", Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
	// The normalisation clone.head produces, asserted rather than assumed:
	// RepoID hashes this string with a 0x00 after every part, so "abc\n", "abc"
	// and "ABC" are three repositories.
	if len(got) != 40 || got != strings.ToLower(got) || strings.TrimSpace(got) != got {
		t.Errorf("Resolve returned %q, want 40 lowercase hex with no whitespace", got)
	}
}

// The measured trap: a/main sorts first and a first-line read returns it.
func TestResolveMatchesTheRefExactlyAndNotByTail(t *testing.T) {
	url, mainSHA, aMainSHA := twoBranches(t)
	lim := Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second}

	// The fixture is proved able to trap: ls-remote really does return both,
	// with a/main first.
	out, err := exec.Command("git", "ls-remote", "--heads", "--", url, "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ls-remote returned %d rows, want 2: this fixture cannot trap a tail match\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "refs/heads/a/main") {
		t.Fatalf("a/main does not sort first here, so a first-line read would be right by accident:\n%s", out)
	}

	got, err := Resolve(context.Background(), url, "main", lim)
	if err != nil {
		t.Fatal(err)
	}
	if got == aMainSHA {
		t.Errorf("Resolve(\"main\") = %s (refs/heads/a/main), want %s", got, mainSHA)
	}
	if got != mainSHA {
		t.Errorf("Resolve = %q, want %q", got, mainSHA)
	}
	// And the other branch resolves to its own commit, so the match is exact in
	// both directions rather than merely preferring the shorter name.
	got, err = Resolve(context.Background(), url, "a/main", lim)
	if err != nil {
		t.Fatal(err)
	}
	if got != aMainSHA {
		t.Errorf("Resolve(\"a/main\") = %q, want %q", got, aMainSHA)
	}
}

func TestResolveRefusesWhenTheRefMatchesNoneOrMany(t *testing.T) {
	url, _, _ := twoBranches(t)
	lim := Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second}
	for _, ref := range []string{
		// Exists only as a/main. ls-remote returns it and exits 0, so there is
		// no error to notice — which is why the count is what refuses.
		"nosuchbranch",
		"a",
		// A tag would need a peeled ^{} row too; exactly-one stays crisp.
		"v1.0.0",
	} {
		t.Run(ref, func(t *testing.T) {
			got, err := Resolve(context.Background(), url, ref, lim)
			if err == nil {
				t.Fatalf("Resolve(%q) = %q, want a refusal", ref, got)
			}
			if !errors.Is(err, ErrAmbiguousRef) {
				t.Errorf("Resolve(%q) = %v, want ErrAmbiguousRef", ref, err)
			}
		})
	}
}

func TestResolveReadsTheDefaultBranchForHead(t *testing.T) {
	url, mainSHA, _ := twoBranches(t)
	lim := Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second}
	for _, ref := range []string{"", "HEAD"} {
		got, err := Resolve(context.Background(), url, ref, lim)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		if got != mainSHA {
			t.Errorf("Resolve(%q) = %q, want the default branch %q", ref, got, mainSHA)
		}
	}
}

// The environment neutering, on the SAME helper Run uses. A fake git first on
// the child's PATH records what it was handed, and a control invocation proves
// the recorder fires — so a missing variable is a finding rather than an
// absence.
func TestResolveRunsUnderTheSameGitEnvironmentAsClone(t *testing.T) {
	bin := t.TempDir()
	out := filepath.Join(t.TempDir(), "env.txt")
	script := "#!/bin/sh\nenv > " + out + "\nexit 7\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := Resolve(context.Background(), "https://example.com/a/b", "main",
		Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second}); err == nil {
		t.Fatal("the fake git succeeded; this test measures nothing")
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		// The control: the recorder has to have fired, or every assertion below
		// would pass against an empty file.
		t.Fatalf("the fake git never wrote its environment: %v", err)
	}
	env := string(raw)
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the child's environment has no %s:\n%s", want, env)
		}
	}
	// The parent environment is INHERITED and only those five are closed. That
	// is the right call for git and P4 records why it is the wrong call for go;
	// asserted here so extracting gitCmd cannot have narrowed it.
	if !strings.Contains(env, "PATH=") {
		t.Errorf("the child inherited no PATH, so the environment is no longer append(os.Environ(), …)")
	}
}

// spec §6's rule: one deadline for the whole job. Resolve derives none of its
// own, so the CALLER's context is what ends it.
//
// Called DIRECTLY with a short context. A test that drove it through runJob
// would be measuring jobCtx, and a mutation adding a fresh WithTimeout of the
// same duration would be a semantic no-op — the void kill clone.Run's own
// nested deadline already is under the indexer's caller.
func TestResolveRunsOnTheCallersBudget(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nsleep 5\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	// A Limits deadline an order of magnitude longer than the caller's, so
	// whichever ends the call is unambiguous.
	_, err := Resolve(ctx, "https://example.com/a/b", "main",
		Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the blocking git succeeded")
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned after %v, want the caller's ~500ms deadline", elapsed.Round(10*time.Millisecond))
	}
}

// The other half, and without it the mutation is VOID.
//
// "Resolve derives its own WithTimeout(ctx, lim.Deadline)" is a SEMANTIC NO-OP
// whenever lim.Deadline is longer than the caller's remaining budget — which is
// the indexer's actual configuration, since jobCtx is built with exactly
// lim.clone.Deadline. Measured: with a 30s Limits deadline under a 500ms
// caller, the mutant passes the test above unchanged.
//
// So this pins the inverse: under an UNBOUNDED parent, lim.Deadline must NOT
// end the call. A Resolve that imposed it would return at 300ms here.
func TestResolveDoesNotImposeItsOwnDeadline(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nsleep 1\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	_, err := Resolve(context.Background(), "https://example.com/a/b", "main",
		Limits{MaxBytes: 1 << 20, Deadline: 300 * time.Millisecond})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the fake git succeeded")
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("returned after %v: lim.Deadline ended the call, but spec §6 says the job's one deadline does",
			elapsed.Round(10*time.Millisecond))
	}
}

func TestResolveRefusesAZeroDeadline(t *testing.T) {
	if _, err := Resolve(context.Background(), "https://example.com/a/b", "main", Limits{}); err == nil {
		t.Error("a zero Deadline was accepted")
	}
}

// The exactly-one rule, against output no real remote emits.
//
// "many" is unreachable through a live fixture: with a byte-exact ref match it
// needs two rows carrying identical names, and git does not produce those. That
// makes `len(found) != 1` a guard a mutation could weaken to `== 0` with every
// live test still passing — measured, it survived — so it is tested here on
// synthetic bytes instead.
func TestPickHeadRefusesZeroOrManyAndMatchesExactly(t *testing.T) {
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for name, tc := range map[string]struct {
		out  string
		want string
		sha  string
		err  bool
	}{
		"exactly one": {
			out:  a + "\trefs/heads/a/main\n" + b + "\trefs/heads/main\n",
			want: "refs/heads/main", sha: b,
		},
		"the tail match is not taken": {
			out:  a + "\trefs/heads/a/main\n",
			want: "refs/heads/main", err: true,
		},
		"many": {
			out:  a + "\trefs/heads/main\n" + b + "\trefs/heads/main\n",
			want: "refs/heads/main", err: true,
		},
		"none": {out: "", want: "refs/heads/main", err: true},
		"the symref line is skipped": {
			out:  "ref: refs/heads/main\tHEAD\n" + b + "\tHEAD\n",
			want: "HEAD", sha: b,
		},
		"a short sha is refused": {
			out:  "abc\trefs/heads/main\n",
			want: "refs/heads/main", err: true,
		},
		"an uppercase sha is refused": {
			out:  strings.ToUpper(a) + "\trefs/heads/main\n",
			want: "refs/heads/main", err: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := pickHead([]byte(tc.out), tc.want)
			if tc.err {
				if err == nil {
					t.Fatalf("pickHead = %q, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pickHead: %v", err)
			}
			if got != tc.sha {
				t.Errorf("pickHead = %q, want %q", got, tc.sha)
			}
		})
	}
}
