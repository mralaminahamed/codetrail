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
// removes it again on error, so a failed job leaves nothing behind for the
// disk quota to trip over later.
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

	cmd := exec.CommandContext(ctx, "git", args...)
	// A private or mistyped URL must fail rather than block forever waiting for
	// a credential nobody is there to type. GIT_CONFIG_NOSYSTEM closes the
	// system file and outranks an inherited GIT_CONFIG_SYSTEM; /dev/null as the
	// global file closes both ~/.gitconfig and XDG's copy, which rewriting HOME
	// alone does not.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME="+filepath.Dir(dir),
	)
	// Its own process group, so the deadline kills git's children too — a
	// killed parent otherwise leaves a fetch running against the cap.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// And a bound on what the group kill misses: a descendant that escapes it
	// can hold the output pipe open, and waiting on that pipe has no timeout of
	// its own. Returning late beats not returning.
	cmd.WaitDelay = 5 * time.Second

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

func head(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse: %w", err)
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
