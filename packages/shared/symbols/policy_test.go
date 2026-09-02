package symbols

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// The parent environment every hermetic test here is run against. Each of
// these re-opens something Env closes, or breaks the load outright.
var hostileParent = map[string]string{
	"GOPROXY":          "https://proxy.example.invalid",
	"GOFLAGS":          "-mod=vendor",
	"GOPRIVATE":        "*",
	"GOINSECURE":       "*",
	"GONOSUMDB":        "*",
	"GOROOT":           "/nonexistent-goroot",
	"GOEXPERIMENT":     "codetrail_bogus",
	"GOTOOLCHAIN":      "auto",
	"GOWORK":           "/tmp/codetrail-hostile.work",
	"GOPACKAGESDRIVER": "/bin/false",
	"CGO_ENABLED":      "1",
	"CGO_LDFLAGS":      "-lcodetrail_hostile",
	"GOMODCACHE":       "/tmp/codetrail-hostile-modcache",
	"GOCACHE":          "/tmp/codetrail-hostile-gocache",
	"GOPATH":           "/tmp/codetrail-hostile-gopath",
	"GODEBUG":          "gotypesalias=0",
	"LD_PRELOAD":       "/tmp/codetrail-hostile.so",
	"HTTPS_PROXY":      "http://127.0.0.1:1",
	"XDG_CONFIG_HOME":  "/tmp/codetrail-hostile-config",
}

func plantHostileParent(t *testing.T) {
	t.Helper()
	for k, v := range hostileParent {
		t.Setenv(k, v)
	}
}

func testPolicy(t *testing.T) Policy {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go binary on PATH, which this package's tests require: %v", err)
	}
	return Policy{Root: t.TempDir(), Home: t.TempDir(), GoBin: gobin}
}

// The allowlist's claim is that it is closed, not that one entry of it is
// right — so this asserts the whole key set and that no value was copied from
// the parent. It cannot replace the behaviour tests in load_test.go: a mutant
// that appends the allowlist to os.Environ passes this by containing every
// key, and is caught there instead.
func TestTheChildEnvironmentIsAnAllowlist(t *testing.T) {
	plantHostileParent(t)
	p := testPolicy(t)

	want := make(map[string]bool, len(EnvKeys))
	for _, k := range EnvKeys {
		want[k] = true
	}
	seen := make(map[string]string)
	for _, e := range p.Env() {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			t.Fatalf("environment entry %q has no =", e)
		}
		if !want[k] {
			t.Errorf("the child environment carries %s, which is not in EnvKeys", k)
		}
		if _, dup := seen[k]; dup {
			t.Errorf("%s is set twice, so which one wins is os/exec's business, not this policy's", k)
		}
		seen[k] = v
	}
	for _, k := range EnvKeys {
		if _, ok := seen[k]; !ok {
			t.Errorf("EnvKeys names %s and the child environment does not set it", k)
		}
	}
	for k, hostile := range hostileParent {
		if got, ok := seen[k]; ok && got == hostile {
			t.Errorf("%s is %q, which is the parent's value", k, got)
		}
	}
	// Named because each is closed by omission rather than by a safe value,
	// and an entry naming one would survive an append-to-os.Environ mutation.
	for _, k := range []string{"GOFLAGS", "GOPRIVATE", "GOINSECURE", "GOROOT", "GOEXPERIMENT", "GODEBUG", "LD_PRELOAD", "HTTPS_PROXY", "XDG_CONFIG_HOME", "CGO_LDFLAGS"} {
		if v, ok := seen[k]; ok {
			t.Errorf("%s is set to %q; it is meant to be closed by omission", k, v)
		}
	}
}

func TestAProxyOfDirectIsRefused(t *testing.T) {
	p := testPolicy(t)
	for _, proxy := range []string{
		"direct",
		"off,direct",
		"https://proxy.example,direct",
		"https://proxy.example|direct",
		" direct ",
		"http://proxy.example",
		"file:///tmp/proxy",
	} {
		t.Run(proxy, func(t *testing.T) {
			p.Proxy = proxy
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate(%q) returned nil, want an error naming the setting", proxy)
			}
			if !strings.Contains(err.Error(), "GOPROXY") {
				t.Errorf("Validate(%q) said %q, which does not name GOPROXY", proxy, err)
			}
		})
	}
	for _, proxy := range []string{"", ProxyOff, "https://proxy.example", "https://a,https://b", "https://a|off"} {
		t.Run("allowed/"+proxy, func(t *testing.T) {
			p.Proxy = proxy
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate(%q) returned %v, want nil", proxy, err)
			}
		})
	}
}

func TestPolicyRefusesAnEmptyGoBinOrRoot(t *testing.T) {
	good := testPolicy(t)
	for _, tc := range []struct {
		name    string
		mutate  func(*Policy)
		wantErr string
		noTool  bool
	}{
		{"empty GoBin", func(p *Policy) { p.GoBin = "" }, "GoBin is empty", true},
		{"relative GoBin", func(p *Policy) { p.GoBin = "go" }, "not absolute", true},
		{"empty Root", func(p *Policy) { p.Root = "" }, "Root", false},
		{"relative Root", func(p *Policy) { p.Root = "checkout" }, "Root", false},
		{"empty Home", func(p *Policy) { p.Home = "" }, "Home", false},
		{"relative Home", func(p *Policy) { p.Home = "scratch" }, "Home", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate returned nil, want a refusal naming %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate said %q, want it to name %s", err, tc.wantErr)
			}
			if errors.Is(err, ErrNoToolchain) != tc.noTool {
				t.Errorf("errors.Is(err, ErrNoToolchain) is %v, want %v: a missing toolchain downgrades edges and a bad path is a misconfiguration",
					errors.Is(err, ErrNoToolchain), tc.noTool)
			}
		})
	}
}

// The loader runs exec.Command("go"), which resolves against the parent's
// PATH, so a GoBin the parent does not resolve to is a policy about a process
// that will not run.
func TestAGoBinaryThePathDoesNotResolveIsRefused(t *testing.T) {
	p := testPolicy(t)
	p.GoBin = "/usr/bin/definitely-not-the-go-on-path"
	err := p.Validate()
	if err == nil {
		t.Fatal("Validate returned nil for a GoBin the parent's PATH does not resolve to")
	}
	if !errors.Is(err, ErrNoToolchain) {
		t.Errorf("Validate said %q, want it to be an ErrNoToolchain", err)
	}
	if !strings.Contains(err.Error(), "PATH resolves go to") {
		t.Errorf("Validate said %q, want it to name the binary the loader would actually run", err)
	}
}
