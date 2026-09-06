// Package clone fetches a repository into a scratch directory under caps.
//
// This is the only code in codetrail that runs a subprocess against an
// untrusted input, so every knob here is a containment decision rather than a
// performance one.
package clone

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var ErrTooLarge = errors.New("clone: repository exceeds the size cap")

type Limits struct {
	MaxBytes int64
	Deadline time.Duration
}

type Result struct {
	Dir    string
	Commit string
	Bytes  int64
}

// Run clones remote at ref into dir. Every path that can leave dir on disk
// removes it before returning — but not for good: a descendant that outlived
// the process-group kill can write the checkout back afterwards. That is the
// same escape WaitDelay below refuses to assume away, so the caller wants its
// own defer os.RemoveAll(dir) behind this one.
func Run(ctx context.Context, remote, ref, dir string, lim Limits) (Result, error) {
	// Both caps fail closed and alike: a zero MaxBytes is a refusal, not
	// "unlimited", and a zero Deadline is not "no deadline". Refusing before
	// the subprocess starts is what keeps a misconfigured worker from cloning
	// without the cap it was meant to have.
	if lim.MaxBytes <= 0 {
		return Result{}, fmt.Errorf("clone: MaxBytes must be positive, got %d", lim.MaxBytes)
	}
	if lim.Deadline <= 0 {
		return Result{}, fmt.Errorf("clone: Deadline must be positive, got %s", lim.Deadline)
	}

	ctx, cancel := context.WithTimeout(ctx, lim.Deadline)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Result{}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	// --depth 1 keeps history out of the fetch and --single-branch keeps every
	// other ref out of it. --filter=blob:none does not shrink this fetch — the
	// checkout pulls back every blob it skipped — it bounds one that stops
	// checking out the whole tree.
	args := []string{
		"clone", "--quiet", "--depth", "1", "--single-branch",
		"--filter=blob:none", "--no-tags",
	}
	if ref != "" && ref != "HEAD" {
		args = append(args, "--branch", ref)
	}
	// -- so that no remote can be read as an option. admit accepts only
	// https:// URLs, which already rules that out; the separator is here so
	// this line does not rest on a policy enforced in another package.
	args = append(args, "--", remote, dir)

	cmd := gitCmd(ctx, filepath.Dir(dir), args...)

	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("clone: %w: %s", err, strings.TrimSpace(string(out)))
	}

	size, err := dirSize(dir)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	// Checked after the fetch, because git has no byte cap to give it —
	// blob:limit caps one blob, maxInputSize is receive-pack's — so nothing
	// here bounds what reaches the disk while git runs except the deadline.
	if size > lim.MaxBytes {
		cleanup()
		return Result{}, fmt.Errorf("%w: %d bytes > %d", ErrTooLarge, size, lim.MaxBytes)
	}

	sha, err := head(ctx, dir)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	return Result{Dir: dir, Commit: sha, Bytes: size}, nil
}

// head reads the commit that was actually fetched.
//
// Through gitCmd, like every other fork in this package. It was hand-written
// once and got none of the containment: no environment overrides, no process
// group, no WaitDelay — inside the binary spec §4 designates as the
// untrusted-input boundary. The last of those is the one that bites here,
// because cmd.Output() is precisely the unbounded wait gitCmd's own doc
// describes: a descendant that escapes the group kill holds the output pipe
// open, and waiting on that pipe has no timeout of its own.
func head(ctx context.Context, dir string) (string, error) {
	cmd := gitCmd(ctx, filepath.Dir(dir), "-C", dir, "rev-parse", "HEAD")
	// Stdout carries the sha, so the error has to carry stderr: an empty
	// repository clones cleanly and fails only here, and "exit status 128" on
	// its own does not say that is what happened.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// dirSize counts regular files only, and does not follow symlinks: what a link
// points at is not the repository's bytes, and counting it would let a link to
// a host file decide whether the cap trips.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// gitCmd is the containment, written once.
//
// Run and Resolve both fork git against a stranger's URL, and two hand-written
// copies of this agree on the day they are written — the argument this codebase
// already makes for RepoID and for chunk.Decl. Extracting it changes nothing
// about what it does, and what it does is worth restating so the extraction
// cannot quietly narrow it:
//
//   - append(os.Environ(), …) with FIVE overrides. The parent environment is
//     INHERITED and only those five are closed. That is the right call for git
//     — P4 records why it is the wrong call for `go` — and the helper keeps it.
//   - GIT_TERMINAL_PROMPT=0 and GIT_ASKPASS=/bin/false, so a private or mistyped
//     URL fails rather than blocking forever waiting for a credential nobody is
//     there to type. ls-remote against a private repository is precisely the
//     call that prompts.
//   - GIT_CONFIG_NOSYSTEM=1 closes the system file and outranks an inherited
//     GIT_CONFIG_SYSTEM; /dev/null as the global file closes both ~/.gitconfig
//     and XDG's copy, which rewriting HOME alone does not.
//   - Its own process group, so a deadline kills git's children too.
//   - WaitDelay, because a descendant that escapes the group kill can hold the
//     output pipe open and waiting on that pipe has no timeout of its own.
func gitCmd(ctx context.Context, home string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME="+home,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// ErrAmbiguousRef is a ref that matched no head, or more than one.
var ErrAmbiguousRef = errors.New("clone: ref did not resolve to exactly one head")

// Resolve asks the remote what commit ref points at, without fetching anything.
//
// It exists so RepoID(key, sha) is computable BEFORE any bytes are cloned, which
// is what lets a re-submission of an already-indexed commit complete without a
// clone at all.
//
// IT MATCHES refs/heads/<ref> BYTE-EXACTLY AND REFUSES ZERO-OR-MANY, and that is
// the whole security content of this function. `git ls-remote --heads <url>
// <ref>` is a TAIL match, not an exact one, and the SUBMITTER OWNS THE BRANCH
// LAYOUT of the repository being matched against. Reproduced on a local fixture:
//
//	$ git ls-remote --heads ./origin main
//	c4c487e…  refs/heads/a/main
//	2c5189b…  refs/heads/main
//
// a/main sorts FIRST, so a Resolve that read line one would return the wrong
// commit; and with main absent entirely the command returns a/main and exits 0,
// so there is no error to notice either. The fast path would then compute
// RepoID(key, wrongSHA), and if that row exists the job completes `done` against
// a corpus the caller never asked for — with every citation rendering a
// permalink at that commit, against the design's "immutable by construction".
// That is the P1 identity blocker wearing a different hat, and it is invisible
// to a single-branch fixture, which matches once and looks correct forever.
//
// Zero or many is an error, and the caller falls through to the clone, which
// resolves the ref the way git itself does. This function never guesses.
//
// An empty ref or "HEAD" resolves the remote's default branch through --symref,
// where the exactly-one rule is the same: one row whose ref column is byte-equal
// to "HEAD". A TAG resolves to nothing here and falls through to the clone: an
// annotated tag has both refs/tags/<t> and a peeled refs/tags/<t>^{}, and
// "exactly one" is a rule worth keeping crisp rather than special-casing.
//
// NO DEADLINE OF ITS OWN. spec §6's rule is one deadline for the whole job, and
// a fresh timeout here of the same duration as the caller's would never fire —
// the semantic no-op clone.Run's own nested WithTimeout already is under the
// indexer's jobCtx. The caller's context is the budget.
func Resolve(ctx context.Context, remote, ref string, lim Limits) (string, error) {
	if lim.Deadline <= 0 {
		return "", fmt.Errorf("clone: Deadline must be positive, got %s", lim.Deadline)
	}
	want := "refs/heads/" + ref
	if ref == "" || ref == "HEAD" {
		ref, want = "HEAD", "HEAD"
	}
	// -- so that no remote can be read as an option, exactly as Run does.
	// --symref so HEAD names a branch; the symref line is skipped below.
	cmd := gitCmd(ctx, os.TempDir(), "ls-remote", "--symref", "--", remote, ref)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("clone: ls-remote: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return pickHead(out, want)
}

// pickHead is Resolve's parser, split out so the exactly-one rule is testable
// against output git cannot be persuaded to produce.
//
// The two guards are redundant BY CONSTRUCTION and both are kept: with a
// byte-exact match, "many" needs two rows carrying identical ref names, which
// no real remote emits — so `!= 1` rather than `== 0` is unobservable against
// a live fixture. It is the guard that would still hold if the match above were
// ever loosened to a prefix or a suffix, which is exactly the loosening this
// function exists to prevent, so it is tested here rather than left to a
// fixture that cannot reach it.
func pickHead(out []byte, want string) (string, error) {
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// "ref: refs/heads/main\tHEAD" — the symref line, which carries no sha.
		if line == "" || strings.HasPrefix(line, "ref:") {
			continue
		}
		sha, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		// Byte-equal, never a suffix or a contains.
		if strings.TrimSpace(name) != want {
			continue
		}
		found = append(found, strings.TrimSpace(sha))
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%w: %q matched %d heads", ErrAmbiguousRef, want, len(found))
	}
	sha := found[0]
	// The same normalisation clone.head produces, so RepoID cannot disagree
	// between the two paths: 40 lowercase hex, trimmed. "abc\n", "abc" and
	// "ABC" are three repositories to a hash that writes a 0x00 after every
	// part.
	if !isFullSHA(sha) {
		return "", fmt.Errorf("clone: ls-remote answered %q, want 40 lowercase hex", sha)
	}
	return sha, nil
}

// isFullSHA is what head() already guarantees by construction and what
// ls-remote's output has to be held to explicitly.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
