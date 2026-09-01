package admit

import (
	"errors"
	"testing"
)

func policy() Policy { return NewPolicy(DefaultHosts) }

func TestAcceptsAnAllowlistedHTTPSRemote(t *testing.T) {
	got, err := policy().Check("https://github.com/mralaminahamed/codetrail")
	if err != nil {
		t.Fatalf("want accepted, got %v", err)
	}
	if got.Host != "github.com" || got.Owner != "mralaminahamed" || got.Name != "codetrail" {
		t.Fatalf("parsed wrong: %+v", got)
	}
}

// file:// alone would turn "index a repo" into "read the indexer's disk".
func TestRejectsEverySchemeButHTTPS(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"git://github.com/x/y",
		"ssh://git@github.com/x/y",
		"http://github.com/x/y",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) || e.Rule != RuleScheme {
			t.Fatalf("%s: want a scheme rejection, got %v", raw, err)
		}
	}
}

// An allowlist, not a denylist: the interesting targets are the ones nobody
// thought to deny. These are the shapes an SSRF attempt actually takes.
func TestRejectsHostsOutsideTheAllowlist(t *testing.T) {
	for _, raw := range []string{
		"https://169.254.169.254/latest/meta-data",
		"https://localhost/x/y",
		"https://127.0.0.1/x/y",
		"https://[::1]/x/y",
		"https://10.0.0.5/x/y",
		"https://internal.corp/x/y",
		// A lookalike: the allowlist is exact hosts, not suffixes.
		"https://github.com.evil.example/x/y",
		"https://notgithub.com/x/y",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) || e.Rule != RuleHost {
			t.Fatalf("%s: want a host rejection, got %v", raw, err)
		}
	}
}

// A subdomain of an allowlisted host is a different host.
func TestRejectsSubdomainsOfAllowlistedHosts(t *testing.T) {
	_, err := policy().Check("https://pages.github.com/x/y")
	var e *Error
	if !errors.As(err, &e) || e.Rule != RuleHost {
		t.Fatalf("want a host rejection, got %v", err)
	}
}

// Userinfo can smuggle a different authority past a careless reader; port
// tricks do the same. Neither has a legitimate use here.
func TestRejectsUserinfoAndPorts(t *testing.T) {
	for _, raw := range []string{
		"https://github.com@evil.example/x/y",
		"https://user:pass@github.com/x/y",
		"https://github.com:8080/x/y",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%s: want a rejection, got %v", raw, err)
		}
	}
}

func TestRejectsMalformedAndIncompletePaths(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"not a url",
		"https://github.com",
		"https://github.com/onlyowner",
		"https://github.com//",
		// Deeper than /owner/name: accepting it would silently normalise a
		// browse URL down to a repo the submitter did not name.
		"https://github.com/owner/repo/tree/main",
		// ".git" is a suffix, not a name.
		"https://github.com/owner/.git",
	} {
		if _, err := policy().Check(raw); err == nil {
			t.Fatalf("%q: want a rejection, got none", raw)
		}
	}
}

// Owner and Name are exported, so a later stage may join them into a path.
// ".." is the escape; url.Parse has already decoded %2e%2e by the time we look.
func TestRejectsPathSegmentsThatAreNotPlainNames(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/../repo",
		"https://github.com/%2e%2e/repo",
		"https://github.com/owner/..",
		"https://github.com/./repo",
		"https://github.com/owner/.",
		"https://github.com/a b/c d",
		"https://github.com/own$er/repo",
		"https://github.com/owner/re;po",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) || e.Rule != RuleForm {
			t.Fatalf("%s: want a form rejection, got %v", raw, err)
		}
	}
}

// A validator that refuses everything kills every mutation and is worthless,
// so pin the names real forges actually serve.
func TestAcceptsLegalRepositoryNames(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/owner/repo.js":    "https://github.com/owner/repo.js",
		"https://github.com/owner/my-repo_2":  "https://github.com/owner/my-repo_2",
		"https://github.com/Owner/Repo.git":   "https://github.com/Owner/Repo",
		"https://codeberg.org/some.group/x-1": "https://codeberg.org/some.group/x-1",
	} {
		got, err := policy().Check(raw)
		if err != nil {
			t.Fatalf("%s: want accepted, got %v", raw, err)
		}
		if got.URL != want {
			t.Fatalf("%s normalised to %q, want %q", raw, got.URL, want)
		}
	}
}

// The operator has to see which half of the path was wrong.
func TestSegmentErrorNamesTheOffendingSegment(t *testing.T) {
	_, err := policy().Check("https://github.com/good/ba d")
	if err == nil {
		t.Fatal("want an error")
	}
	if !contains(err.Error(), "ba d") {
		t.Fatalf("error %q does not name the offending segment", err.Error())
	}
}

// The default allowlist is a product decision, not an incidental default, so
// pin its contents: a forge whose typical URL this policy refuses does not
// belong in it, and the next person should have to change a test to add one.
func TestDefaultHostsArePinned(t *testing.T) {
	want := []string{"github.com", "codeberg.org"}
	if len(DefaultHosts) != len(want) {
		t.Fatalf("DefaultHosts = %v, want %v", DefaultHosts, want)
	}
	for i := range want {
		if DefaultHosts[i] != want[i] {
			t.Fatalf("DefaultHosts = %v, want %v", DefaultHosts, want)
		}
	}
	// The reason gitlab.com is absent, stated as behaviour.
	if _, err := policy().Check("https://gitlab.com/group/repo"); err == nil {
		t.Fatal("gitlab.com must not be in the default allowlist")
	}
}

// The gateway renders Rule straight into the 400 body, so the two branches that can
// fire before the host is known have to name the right rule, not merely refuse.
// The control characters are not exotic: they are what makes url.Parse itself
// fail, which is the only way to reach that second branch.
func TestPreHostRefusalsAreRuleForm(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"https://github.com/x/\ty",
		"https://github.com/x/\ny",
		"https://github.com/x/\ry",
		"https://github.com/x/\x00y",
		"https://github.com/x/\x7fy",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) || e.Rule != RuleForm {
			t.Fatalf("%q: want a form rejection, got %v", raw, err)
		}
	}
}

// Both binaries build the policy from a split env var, so "github.com, codeberg.org"
// — the natural way to write it — hands NewPolicy a leading space to absorb.
func TestNewPolicyNormalisesHosts(t *testing.T) {
	for _, hosts := range [][]string{
		{"GitHub.com"},
		{" github.com "},
		{"  GitHub.COM  "},
		{"github.com", " codeberg.org"},
	} {
		if _, err := NewPolicy(hosts).Check("https://github.com/o/n"); err != nil {
			t.Fatalf("%q: want accepted, got %v", hosts, err)
		}
	}
}

// A policy with no usable host must refuse everything rather than admit it.
// The hostless URL is the one that matters: an empty entry kept in the map
// would match u.Hostname() == "" and let it through.
func TestPolicyWithoutHostsRefusesEverything(t *testing.T) {
	for _, p := range []Policy{
		NewPolicy([]string{""}),
		NewPolicy([]string{"   "}),
		NewPolicy(nil),
		{},
	} {
		for _, raw := range []string{"https://github.com/o/n", "https:///o/n"} {
			_, err := p.Check(raw)
			var e *Error
			if !errors.As(err, &e) || e.Rule != RuleHost {
				t.Fatalf("%q: want a host rejection, got %v", raw, err)
			}
		}
	}
}

// Host matching is case-insensitive, and a trailing .git or slash is the same
// repository — normalising here means the job dedupe index sees one key.
func TestNormalises(t *testing.T) {
	for _, raw := range []string{
		"https://GitHub.com/Owner/Repo",
		"https://github.com/Owner/Repo.git",
		"https://github.com/Owner/Repo/",
	} {
		got, err := policy().Check(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got.URL != "https://github.com/Owner/Repo" {
			t.Fatalf("%s normalised to %q", raw, got.URL)
		}
	}
}

// The caller has to be able to tell an operator which rule refused them, so
// the message must name the rule and the offending value.
func TestErrorNamesTheRuleAndTheValue(t *testing.T) {
	_, err := policy().Check("https://evil.example/x/y")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"host", "evil.example"} {
		if !contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The measured duplication: github.com/octocat/Spoon-Knife and
// github.com/OctoCat/spoon-knife are one repository on the forge, and were two
// rows in the corpus. Key is what makes them one; URL is what keeps each
// submitter's spelling.
func TestKeyFoldsOwnerAndNameCase(t *testing.T) {
	a, err := policy().Check("https://github.com/octocat/Spoon-Knife")
	if err != nil {
		t.Fatal(err)
	}
	b, err := policy().Check("https://GitHub.com/OctoCat/spoon-knife.git/")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key {
		t.Fatalf("case variants got two keys: %q and %q", a.Key, b.Key)
	}
	if a.Key != "github.com/octocat/spoon-knife" {
		t.Fatalf("key is %q", a.Key)
	}
	if a.URL == b.URL {
		t.Fatalf("both spellings collapsed to one URL %q; display case is lost", a.URL)
	}
	if a.URL != "https://github.com/octocat/Spoon-Knife" {
		t.Fatalf("URL is %q", a.URL)
	}
}

// A forge serves foo, foo.git and foo.git.git as one repository. One trim left
// the third as a second identity.
func TestStripsEveryGitSuffix(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/Owner/Repo.git",
		"https://github.com/Owner/Repo.git.git",
		"https://github.com/Owner/Repo.git.git.git",
	} {
		got, err := policy().Check(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got.Name != "Repo" || got.URL != "https://github.com/Owner/Repo" {
			t.Fatalf("%s normalised to %+v", raw, got)
		}
	}
	// Trimming to nothing is still a refusal, not an empty name.
	if _, err := policy().Check("https://github.com/Owner/.git.git"); err == nil {
		t.Fatal("want a refusal for a name that is only suffixes")
	}
}
