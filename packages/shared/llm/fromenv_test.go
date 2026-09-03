package llm

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unconfigure clears every LLM_* setting so a test starts from a real default
// rather than from whatever the shell that ran `go test` happened to export.
func unconfigure(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY", "LLM_API_KEY_FILE"} {
		t.Setenv(k, "")
	}
}

func TestABaseUrlOutsideTheAllowlistRefusesToBoot(t *testing.T) {
	unconfigure(t)
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_API_KEY", canary)
	for _, raw := range []string{
		"https://evil.example",
		"https://api.anthropic.com.evil.example",
		"https://evil.example/api.anthropic.com",
		// A userinfo host is the classic allowlist bypass: the "host" a careless
		// reader sees is the userinfo, not the authority.
		"https://api.anthropic.com@evil.example/",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("LLM_BASE_URL", raw)
			m, err := FromEnv(time.Second)
			if err == nil {
				t.Fatalf("FromEnv returned a client (%v) for %s, want a refusal naming LLM_BASE_URL", m, raw)
			}
			if !strings.Contains(err.Error(), "LLM_BASE_URL") {
				t.Errorf("the error does not name the setting: %v", err)
			}
		})
	}
	t.Setenv("LLM_BASE_URL", defaultBaseURL)
	if _, err := FromEnv(time.Second); err != nil {
		t.Errorf("the allowed host was refused: %v", err)
	}
}

func TestAnHttpBaseUrlRefusesToBoot(t *testing.T) {
	unconfigure(t)
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_API_KEY", canary)
	t.Setenv("LLM_BASE_URL", "http://api.anthropic.com")
	_, err := FromEnv(time.Second)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("FromEnv = %v, want a refusal naming https", err)
	}
}

func TestABaseUrlThatIsNotAnAddressRefusesToBoot(t *testing.T) {
	unconfigure(t)
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_API_KEY", canary)
	// url.Parse accepts this as a relative path, which is why the scheme and the
	// host are what decide rather than the parse error.
	for _, raw := range []string{"not a url at all", "api.anthropic.com", "/v1/messages"} {
		t.Setenv("LLM_BASE_URL", raw)
		if _, err := FromEnv(time.Second); err == nil {
			t.Errorf("FromEnv accepted %q", raw)
		}
	}
}

// The fatal RoundTripper, PAIRED WITH A CONTROL THAT DIALS, so the zero is
// evidence rather than an absence.
func TestProviderNoneBuildsNoClientAndReadsNoKey(t *testing.T) {
	unconfigure(t)
	dials := installDialCounter(t)

	m, err := FromEnv(time.Second)
	if err != nil {
		t.Errorf("FromEnv with no configuration failed: %v", err)
	}
	if m != nil {
		t.Errorf("FromEnv returned a non-nil Model for provider %q", "none")
	}
	if *dials != 0 {
		t.Errorf("provider none dialled %d times", *dials)
	}

	// Explicitly, as well as by default, AND with a key present: a mutant that
	// builds a client anyway would otherwise fail for want of a key rather than
	// for building one, and "it errored" is not the claim being made.
	t.Setenv("LLM_PROVIDER", "none")
	t.Setenv("LLM_API_KEY", canary)
	if m, err := FromEnv(time.Second); err != nil || m != nil {
		t.Errorf("FromEnv(none) with a key present = (%v, %v), want (nil, nil)", m, err)
	}

	// The control: the counter does fire when something dials.
	if _, err := http.Get("http://127.0.0.1:1/"); err == nil {
		t.Errorf("the control request unexpectedly succeeded")
	}
	if *dials != 1 {
		t.Fatalf("the dial counter recorded %d dials for the control request, want 1: a zero above would prove nothing", *dials)
	}
}

func TestProviderFakeBuildsNoClientAndReadsNoKey(t *testing.T) {
	unconfigure(t)
	dials := installDialCounter(t)
	t.Setenv("LLM_PROVIDER", "fake")

	m, err := FromEnv(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("FromEnv(fake) returned no model")
	}
	if _, ok := m.(*Fake); !ok {
		t.Errorf("FromEnv(fake) returned %T", m)
	}
	if *dials != 0 {
		t.Errorf("provider fake dialled %d times", *dials)
	}
}

func TestAnUnknownProviderNamesTheLegalValues(t *testing.T) {
	unconfigure(t)
	// One letter wrong.
	t.Setenv("LLM_PROVIDER", "antropic")
	// A KEY IS SET. Without it a fall-through to anthropic would fail for want
	// of a key and the test would pass while the typo silently enabled a paid
	// provider — which is the whole failure this refusal exists to prevent.
	t.Setenv("LLM_API_KEY", canary)
	m, err := FromEnv(time.Second)
	if err == nil {
		t.Fatalf("booted with provider %q, giving %v", "antropic", m)
	}
	for _, want := range Providers {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
}

func TestProviderAnthropicWithNoKeyRefusesToBoot(t *testing.T) {
	unconfigure(t)
	t.Setenv("LLM_PROVIDER", "anthropic")
	if _, err := FromEnv(time.Second); err == nil {
		t.Errorf("booted with no key")
	} else if !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// The egress claim is exactly as strong as this: the allowlist is in FromEnv,
// so a second non-test caller of NewAnthropic would bypass it. Asserted rather
// than assumed, the same shape of guard P4 used to pin that chunk.Decl had not
// been bypassed.
func TestFromEnvIsTheOnlyNonTestCallerOfNewAnthropic(t *testing.T) {
	root := repoRoot(t)
	// --untracked, so a new file that has not been staged yet is still searched:
	// a guard that only sees the index would pass on the very commit that
	// introduced a second call site.
	out, err := exec.Command("git", "-C", root, "grep", "-n", "--untracked", "--", "NewAnthropic(").Output()
	if err != nil {
		t.Fatalf("git grep: %v", err)
	}
	var offenders []string
	found := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		file := line[:strings.Index(line, ":")]
		switch {
		case strings.HasSuffix(file, "_test.go"):
			continue
		case strings.HasSuffix(file, ".md"):
			continue
		case file == "packages/shared/llm/anthropic.go":
			// The declaration itself.
			continue
		case file == "packages/shared/llm/fromenv.go":
			found++
			continue
		}
		offenders = append(offenders, line)
	}
	if len(offenders) != 0 {
		t.Errorf("NewAnthropic has a second non-test caller, which bypasses the host allowlist:\n%s",
			strings.Join(offenders, "\n"))
	}
	if found == 0 {
		t.Errorf("FromEnv does not call NewAnthropic at all; this guard would be vacuous")
	}
}

func TestDotEnvVariantsAreIgnoredAndTheExampleIsNot(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{".env", ".env.local", ".env.production", ".envrc"} {
		if err := exec.Command("git", "-C", root, "check-ignore", "-q", name).Run(); err != nil {
			t.Errorf("%s is not ignored", name)
		}
	}
	if err := exec.Command("git", "-C", root, "check-ignore", "-q", ".env.example").Run(); err == nil {
		t.Errorf(".env.example is ignored, so a committed example is impossible")
	}
	// And no .env of any kind is tracked.
	out, err := exec.Command("git", "-C", root, "ls-files", ".env*").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Fields(string(out)) {
		if f != ".env.example" {
			t.Errorf("%s is tracked", f)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root", root)
	}
	return root
}

// installDialCounter swaps the default transport for one that counts and then
// refuses. Restored by t.Cleanup.
func installDialCounter(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := http.DefaultTransport
	prevClient := http.DefaultClient.Transport
	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		n++
		return nil, errNoDialling
	})
	http.DefaultClient.Transport = http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = prev
		http.DefaultClient.Transport = prevClient
	})
	return &n
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var errNoDialling = errNoDial{}

type errNoDial struct{}

func (errNoDial) Error() string { return "llm test: nothing in this suite may dial" }
