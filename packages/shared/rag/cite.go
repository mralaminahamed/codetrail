package rag

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// Citation is spec §8's tuple — (repo, commit, path, startLine, endLine,
// digest) — plus a permalink over it and what can be said about staleness
// without asking the forge.
//
// The tuple is the claim and the permalink is a convenience: Digest is what
// keeps the claim checkable when the link cannot be rendered, and it is what
// P2 verified 1,303 spans with, byte for byte, against `git show`.
type Citation struct {
	RepoID    string `json:"repo_id"`
	Remote    string `json:"remote"`
	Commit    string `json:"commit"`
	Ref       string `json:"ref"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Digest    string `json:"digest"`
	// Empty for a forge whose URL shape is unknown; see Permalink.
	Permalink string    `json:"permalink"`
	Staleness Staleness `json:"staleness"`
}

// Staleness states what is known about the ref this citation was indexed from.
//
// ForgeChecked is on the wire and always false in P3: it is the field that
// stops Note being read as a freshness guarantee. Asking the forge would put a
// network call to a stranger's host on the public read path — the egress P1
// confined to the indexer — so the corpus is the only evidence here, and the
// corpus cannot see a ref that moved and was never re-indexed.
type Staleness struct {
	State          string    `json:"state"`
	ForgeChecked   bool      `json:"forge_checked"`
	IndexedAt      time.Time `json:"indexed_at"`
	NewerCommit    string    `json:"newer_commit,omitempty"`
	NewerIndexedAt time.Time `json:"newer_indexed_at,omitzero"`
	Note           string    `json:"note"`
}

const (
	// StaleUnknown: the ref may or may not have moved. Nobody looked.
	StaleUnknown = "unknown"
	// StaleSuperseded: this repository's own ref is also indexed here at a
	// different, later commit, so it demonstrably moved. The only positive
	// claim the corpus supports.
	StaleSuperseded = "superseded"
)

// Newer is a later index of the same ref, or the zero value when the corpus
// holds none. It comes from store.NewerCommit.
type Newer struct {
	Commit    string
	IndexedAt time.Time
}

// NewCitation renders one retrieved span as a citation. now is a parameter so
// this package stays free of the clock, like the rest of it.
func NewCitation(r models.Repo, s models.Span, newer Newer, now time.Time) Citation {
	return Citation{
		RepoID:    r.ID,
		Remote:    r.Remote,
		Commit:    r.Commit,
		Ref:       r.Ref,
		Path:      s.Path,
		StartLine: s.StartLine,
		EndLine:   s.EndLine,
		Digest:    s.Digest,
		Permalink: Permalink(r.Remote, r.Commit, s.Path, s.StartLine, s.EndLine),
		Staleness: staleness(r, newer, now),
	}
}

func staleness(r models.Repo, newer Newer, now time.Time) Staleness {
	s := Staleness{
		State: StaleUnknown,
		// Never true in P3. Written as a literal rather than left to the zero
		// value so the claim is visible at the place that makes it.
		ForgeChecked: false,
		IndexedAt:    r.IndexedAt,
	}
	age := ago(now.Sub(r.IndexedAt))
	if newer.Commit == "" {
		s.Note = fmt.Sprintf(
			"Correct at commit %s, indexed %s ago. "+
				"codetrail has not checked whether %s has moved since.",
			short(r.Commit), age, r.Ref)
		return s
	}
	s.State = StaleSuperseded
	s.NewerCommit = newer.Commit
	s.NewerIndexedAt = newer.IndexedAt
	s.Note = fmt.Sprintf(
		"Correct at commit %s, indexed %s ago. "+
			"%s is also indexed here at the later commit %s, so this citation "+
			"is from an older commit. codetrail did not ask the forge, so %s "+
			"may have moved again.",
		short(r.Commit), age, r.Ref, short(newer.Commit), r.Ref)
	return s
}

// ago is coarse because the claim is "this is how old the index is". Minutes
// would imply a freshness check that did not happen.
func ago(d time.Duration) string {
	if days := int(d.Hours() / 24); days >= 1 {
		return plural(days, "day")
	}
	return plural(int(d.Hours()), "hour")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// short is git's own abbreviation, so the note reads like something a person
// can paste into `git show`. The full commit is in the tuple.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// forges maps a host to its permalink shape. Two entries, because P1 settled
// the default allowlist at github.com and codeberg.org and their formats
// differ: Codeberg runs Forgejo, whose blob URL is /src/commit/<sha>/.
//
// Spec §8's example is GitHub's, which is why this is a table and not a format
// string.
var forges = map[string]string{
	"github.com":   "%s/blob/%s/%s#L%d-L%d",
	"codeberg.org": "%s/src/commit/%s/%s#L%d-L%d",
}

// forgePolicy re-admits the remote instead of parsing it again here. The
// string in a repo row was normalised by exactly this check on submission, and
// a second, subtly different parse is a second place for a URL bug to live —
// it is what would put a ".git" suffix or a trailing slash back into a link.
//
// Its host set is the forges table and not the deployment's allowlist: "whose
// URL shape do we know" is a different question from "whose code may we
// clone", and an operator adding a host must not thereby invent a format.
var forgePolicy = admit.NewPolicy(forgeHosts())

func forgeHosts() []string {
	out := make([]string, 0, len(forges))
	for h := range forges {
		out = append(out, h)
	}
	return out
}

// Permalink renders the tuple as a forge URL pinned to the commit, never the
// ref, so the link means the same thing in a year.
//
// It is empty for any remote this cannot re-admit — an unknown host most of
// all. A guessed URL that 404s reads as "the code is gone" rather than "we
// guessed the shape", and the citation still carries the tuple, which is the
// actual claim.
func Permalink(remote, commit, path string, start, end int) string {
	r, err := forgePolicy.Check(remote)
	if err != nil {
		return ""
	}
	// The lookup cannot miss: forgePolicy admits exactly these hosts.
	return fmt.Sprintf(forges[r.Host], r.URL, commit, escapePath(path), start, end)
}

// escapePath escapes each segment and not the separators. A git path may hold
// a space or a '#', and an unescaped '#' truncates the URL at the fragment so
// the link points at the top of the wrong file; escaping the whole path in one
// call would turn the separators — which are structure, not content — into
// %2F, and the link would point at nothing.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
