package symbols

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ProxyOff is the default, and it is the whole security posture of this
// package: a repository whose packages import only the standard library
// type-checks, and one that imports anything else does not (spec §6 already
// calls that an expected outcome).
const ProxyOff = "off"

// ErrNoToolchain is Validate's answer when no usable go binary is named. It is
// its own error because a missing toolchain downgrades a repository's edges,
// while any other invalid policy is an operator misconfiguration.
var ErrNoToolchain = errors.New("symbols: no usable go binary")

// Policy is the environment the type-checker is allowed to have. P1 forks git
// at a stranger's URL; this forks go inside a stranger's source tree, and go
// is a program whose purpose is to fetch things and compile them.
type Policy struct {
	// Root is the checkout, the directory ./... is resolved against.
	Root string
	// Home is the job's scratch directory. Every cache the go command writes
	// lives under it, so nothing a stranger's build wrote outlives the job and
	// no two jobs share a cache one of them filled.
	Home string
	// GoBin is the go binary, resolved once at boot with exec.LookPath.
	GoBin string
	// Proxy is ProxyOff or a module proxy the operator trusts.
	Proxy string
}

// Env is the child's entire environment, built from nothing rather than from
// os.Environ. clone.Run appends to the parent's, which is right for git: the
// four settings that matter are the four it sets last. It is wrong for go,
// which reads a dozen it would inherit.
//
// Everything absent here — GOFLAGS, GOPRIVATE, GOINSECURE, GOEXPERIMENT,
// GOROOT, GODEBUG, LD_PRELOAD, HTTPS_PROXY — is closed *by omission*. That is
// deliberate and not laziness: an entry setting one of them to a safe value
// would still be safe under an append-to-os.Environ mutation, and omission is
// not.
//
// The entries that are here are the ones whose default is unsafe:
//
// GOPROXY: a module fetch is driven by require lines in a stranger's go.mod,
// so with fetching on, the hosts this process contacts are chosen by whoever
// submitted the repository — the SSRF P1's admission allowlist exists to
// refuse, reached by a road it cannot see.
//
// GOVCS=*:off: with a direct proxy, or any matching GOPRIVATE, the go command
// fetches modules with git, at a host a require line names.
//
// GOTOOLCHAIN=local: a "go 1.99.0" directive otherwise downloads a toolchain
// from the proxy before any policy about modules applies.
//
// GOWORK=off: a go.work inside the checkout can name directories outside it.
//
// GOENV=off: ~/.config/go/env is a second copy of every setting above, and it
// survives an empty environment.
//
// GOPACKAGESDRIVER=off: with no value, go/packages looks up a binary named
// gopackagesdriver on the *parent's* PATH and runs it in place of the go
// command. Nothing in the child's environment closes that road; this does.
//
// CGO_ENABLED=0: type-checking import "C" runs cgo, and #cgo LDFLAGS is
// execution on a stranger's terms.
//
// PATH is the toolchain's directory, and it is worth being exact about what
// that buys. It does not choose the go binary — the loader runs
// exec.Command("go"), which resolves against the *parent's* PATH, which is why
// Validate pins the two together — and it is not a boundary by itself, since
// that directory is often /usr/bin. Git and a C compiler are kept out of reach
// by GOVCS, GOPROXY and CGO_ENABLED, each of which is its own lock.
//
// The git variables are clone.Run's, kept even though GOVCS already refuses
// every VCS fetch: two independent controls is the right number for the one
// thing here that would be a real vulnerability.
func (p Policy) Env() []string {
	proxy := p.Proxy
	if proxy == "" {
		proxy = ProxyOff
	}
	return []string{
		"PATH=" + filepath.Dir(p.GoBin),
		"HOME=" + p.Home,
		"GOPROXY=" + proxy,
		"GOVCS=*:off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOENV=off",
		"GOPACKAGESDRIVER=off",
		"CGO_ENABLED=0",
		"GOMODCACHE=" + p.cache("modcache"),
		"GOCACHE=" + p.cache("buildcache"),
		"GOPATH=" + p.cache("gopath"),
		"GOTMPDIR=" + p.cache("tmp"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
}

// EnvKeys is the whole set of names Env sets, exported so a test can pin that
// the allowlist is closed rather than that one entry of it is present.
var EnvKeys = []string{
	"PATH", "HOME", "GOPROXY", "GOVCS", "GOTOOLCHAIN", "GOWORK", "GOENV",
	"GOPACKAGESDRIVER", "CGO_ENABLED", "GOMODCACHE", "GOCACHE", "GOPATH",
	"GOTMPDIR", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "GIT_CONFIG_NOSYSTEM",
	"GIT_CONFIG_GLOBAL",
}

func (p Policy) cache(name string) string { return filepath.Join(p.Home, "go", name) }

// dirs are created before the load: the go command creates GOCACHE and
// GOMODCACHE itself but not GOTMPDIR, and without it every load fails with
// "creating work dir".
func (p Policy) dirs() []string {
	return []string{p.cache("modcache"), p.cache("buildcache"), p.cache("gopath"), p.cache("tmp")}
}

// Validate fails closed, as clone.Run's caps do: an unset field is a refusal
// rather than a default.
func (p Policy) Validate() error {
	if p.GoBin == "" {
		return fmt.Errorf("%w: GoBin is empty", ErrNoToolchain)
	}
	if !filepath.IsAbs(p.GoBin) {
		return fmt.Errorf("%w: GoBin %q is not absolute", ErrNoToolchain, p.GoBin)
	}
	// The loader runs exec.Command("go", …), which resolves the name against
	// the parent's PATH and ignores Env's. A GoBin the parent's PATH does not
	// resolve to describes a binary that will not run, so the policy is not
	// about the process it claims to constrain.
	found, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoToolchain, err)
	}
	if found != p.GoBin {
		return fmt.Errorf("%w: GoBin is %s but PATH resolves go to %s", ErrNoToolchain, p.GoBin, found)
	}
	if !filepath.IsAbs(p.Root) {
		return fmt.Errorf("symbols: Root %q must be an absolute path", p.Root)
	}
	if !filepath.IsAbs(p.Home) {
		return fmt.Errorf("symbols: Home %q must be an absolute path", p.Home)
	}
	return p.validateProxy()
}

// ValidateProxy is validateProxy without the rest of a Policy, so a caller
// reading the setting from its environment can refuse it at boot rather than
// once per job.
func ValidateProxy(proxy string) error { return Policy{Proxy: proxy}.validateProxy() }

// validateProxy refuses direct anywhere in the list. GOPROXY separates its
// elements with , and |, and a direct element turns a stranger's require line
// into an outbound git to a host of their choosing — which is what GOVCS also
// refuses, and the reason the refusal is spelled twice.
func (p Policy) validateProxy() error {
	if p.Proxy == "" || p.Proxy == ProxyOff {
		return nil
	}
	for _, e := range strings.FieldsFunc(p.Proxy, func(r rune) bool { return r == ',' || r == '|' }) {
		switch e = strings.TrimSpace(e); {
		case e == "direct":
			return fmt.Errorf("symbols: GOPROXY %q contains direct, which fetches from hosts a stranger's go.mod names", p.Proxy)
		case e == ProxyOff:
		case strings.HasPrefix(e, "https://"):
		default:
			return fmt.Errorf("symbols: GOPROXY element %q must be https:// or off, in %q", e, p.Proxy)
		}
	}
	return nil
}
