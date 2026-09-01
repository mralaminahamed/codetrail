// Package admit decides whether a submitted repository URL may be cloned.
//
// It runs before anything touches the network or the disk, and it is the only
// SSRF control codetrail has: an exact-host allowlist. It does not defend
// against a hostile allowlisted forge, and it claims no DNS-rebinding
// protection — git is a subprocess and cannot be handed a validating dialer.
package admit

import (
	"fmt"
	"net/url"
	"strings"
)

// Rule names the check that refused a URL, so a 400 can say which one.
type Rule string

const (
	RuleForm   Rule = "form"
	RuleScheme Rule = "scheme"
	RuleHost   Rule = "host"
)

// Error is a refusal. Callers match on Rule to build the response.
type Error struct {
	Rule   Rule
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("admit: %s: %s", e.Rule, e.Detail) }

// Remote is an accepted, normalised repository reference.
//
// URL keeps the case the submitter typed, because a forge preserves the
// display case of an owner and a repository and that is what a citation has
// to show. Key is the identity: forges match owner and name
// case-insensitively, so two spellings are one repository and anything that
// keys on a repository keys on this, not on URL.
type Remote struct {
	URL   string
	Key   string
	Host  string
	Owner string
	Name  string
}

// DefaultHosts is the allowlist a deployment gets if it configures none.
//
// gitlab.com is deliberately absent: GitLab nests namespaces arbitrarily
// (group/subgroup/repo), which the /owner/name path check refuses, so shipping
// it by default would advertise a forge whose typical URL we reject. Adding it
// back means teaching Check about nested namespaces first.
var DefaultHosts = []string{"github.com", "codeberg.org"}

type Policy struct{ hosts map[string]bool }

func NewPolicy(hosts []string) Policy {
	m := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			m[h] = true
		}
	}
	return Policy{hosts: m}
}

// Check validates raw and returns the normalised remote.
func (p Policy) Check(raw string) (Remote, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Remote{}, &Error{RuleForm, "empty URL"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("%q is not a URL", raw)}
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return Remote{}, &Error{RuleScheme, fmt.Sprintf("scheme %q; only https is accepted", u.Scheme)}
	}
	// Userinfo before an authority is how a different host gets smuggled past
	// a careless reader, and a port is not something a forge needs here.
	if u.User != nil {
		return Remote{}, &Error{RuleForm, "credentials in the URL"}
	}
	if u.Port() != "" {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("port %q", u.Port())}
	}
	host := strings.ToLower(u.Hostname())
	if !p.hosts[host] {
		return Remote{}, &Error{RuleHost, fmt.Sprintf("%s is not an allowed host", host)}
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Remote{}, &Error{RuleForm, "path must be /owner/name"}
	}
	// Repeatedly, not once: a forge serves /owner/foo.git and /owner/foo.git.git
	// as the same repository, and one trim would leave "foo.git" as a second
	// identity for it. No forge here allows a name that really ends in ".git".
	owner, name := parts[0], parts[1]
	for strings.HasSuffix(name, ".git") {
		name = strings.TrimSuffix(name, ".git")
	}
	if name == "" {
		return Remote{}, &Error{RuleForm, "path must be /owner/name"}
	}
	if !validSegment(owner) {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("owner %q is not a plain name", owner)}
	}
	if !validSegment(name) {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("name %q is not a plain name", name)}
	}
	return Remote{
		URL:   "https://" + host + "/" + owner + "/" + name,
		Key:   host + "/" + strings.ToLower(owner) + "/" + strings.ToLower(name),
		Host:  host,
		Owner: owner,
		Name:  name,
	}, nil
}

// Owner and Name are exported, so anything downstream may join them into a
// path. Allowlist the charset rather than blacklisting the escapes, and refuse
// the two relative names the charset would otherwise let through.
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
