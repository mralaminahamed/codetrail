package rag

import (
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// Two forges, not one. Spec §8's example is GitHub's, and a GitHub-only
// fixture — the shape that example invites — cannot see a hard-coded URL
// shape at all. The lines differ from each other so a swapped or collapsed
// range is visible, and the comparison is of the whole string, because
// strings.Contains survives a mangled anchor.
func TestPermalinkPerForge(t *testing.T) {
	for _, c := range []struct{ remote, want string }{
		{"https://github.com/rs/zerolog",
			"https://github.com/rs/zerolog/blob/dfd11cca/log.go#L10-L20"},
		{"https://codeberg.org/forgejo/forgejo",
			"https://codeberg.org/forgejo/forgejo/src/commit/dfd11cca/log.go#L10-L20"},
		// Rendering through admit is what keeps the link normalised the same
		// way the row was: a second parser here would put ".git" in the URL.
		{"https://github.com/rs/zerolog.git/",
			"https://github.com/rs/zerolog/blob/dfd11cca/log.go#L10-L20"},
		// A host an operator added. No format is known, so no link is
		// rendered: a guessed URL that 404s reads as "the code is gone".
		{"https://gitlab.com/group/repo", ""},
	} {
		if got := Permalink(c.remote, "dfd11cca", "log.go", 10, 20); got != c.want {
			t.Errorf("%s rendered %q, want %q", c.remote, got, c.want)
		}
	}
}

// A git path may hold a space or a '#'. An unescaped '#' truncates the URL at
// the fragment and the link points at the top of the wrong file; an escaped
// '/' points at nothing. Two segments, because a single-segment path cannot
// tell per-segment escaping from whole-path escaping.
func TestPermalinkEscapesEachSegmentButNotTheSeparators(t *testing.T) {
	got := Permalink("https://github.com/o/n", "abc", "docs/a b#c.md", 1, 2)
	want := "https://github.com/o/n/blob/abc/docs/a%20b%23c.md#L1-L2"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Lines are 1-based and inclusive (spec §3), so a one-line span is L1-L1 and
// not L1-L2 or L0-L1.
func TestPermalinkKeepsOneBasedInclusiveLines(t *testing.T) {
	got := Permalink("https://github.com/o/n", "abc", "x.go", 1, 1)
	want := "https://github.com/o/n/blob/abc/x.go#L1-L1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The ref and the commit differ, and neither is the path: a fixture whose ref
// is its commit cannot tell a staleness bug from a correct answer.
var (
	fixedNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	oldRepo = models.Repo{
		ID: "r1", Remote: "https://github.com/rs/zerolog", Ref: "main",
		Commit:    "dfd11cca9d1f4b7d0f2b0a8e3c5d6e7f8a9b0c1d",
		IndexedAt: fixedNow.AddDate(0, 0, -92),
	}
	logSpan = models.Span{
		ID: "s1", RepoID: "r1", Path: "log.go", Kind: models.KindFunc,
		StartLine: 10, EndLine: 20, Digest: "0badc0de0badc0de",
	}
)

// The default claim names what was not checked. A citation that implied
// freshness nobody verified is the same failure as an ungrounded answer, one
// level up — so the disclaimer is in the words, not only in a flag a renderer
// may drop.
func TestUncheckedStalenessSaysSoInWords(t *testing.T) {
	s := NewCitation(oldRepo, logSpan, Newer{}, fixedNow).Staleness
	if s.State != StaleUnknown {
		t.Fatalf("state %q, want %q", s.State, StaleUnknown)
	}
	if s.ForgeChecked {
		t.Fatal("ForgeChecked is true, and P3 never asks the forge")
	}
	want := "Correct at commit dfd11cc, indexed 92 days ago. " +
		"codetrail has not checked whether main has moved since."
	if s.Note != want {
		t.Fatalf("note\n got %q\nwant %q", s.Note, want)
	}
	if !s.IndexedAt.Equal(oldRepo.IndexedAt) {
		t.Fatalf("IndexedAt %v, want %v", s.IndexedAt, oldRepo.IndexedAt)
	}
	if s.NewerCommit != "" {
		t.Fatalf("NewerCommit %q, want empty", s.NewerCommit)
	}
}

// Superseded is the one positive claim the corpus supports, and it still is
// not a forge check: the ref may have moved again since.
func TestSupersededNamesTheNewerCommit(t *testing.T) {
	newer := Newer{
		Commit:    "ab97b0e5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9",
		IndexedAt: fixedNow.AddDate(0, 0, -1),
	}
	s := NewCitation(oldRepo, logSpan, newer, fixedNow).Staleness
	if s.State != StaleSuperseded {
		t.Fatalf("state %q, want %q", s.State, StaleSuperseded)
	}
	if s.ForgeChecked {
		t.Fatal("ForgeChecked is true, and the corpus is not the forge")
	}
	if s.NewerCommit != newer.Commit || !s.NewerIndexedAt.Equal(newer.IndexedAt) {
		t.Fatalf("newer %q at %v, want %q at %v",
			s.NewerCommit, s.NewerIndexedAt, newer.Commit, newer.IndexedAt)
	}
	want := "Correct at commit dfd11cc, indexed 92 days ago. " +
		"main is also indexed here at the later commit ab97b0e, so this " +
		"citation is from an older commit. codetrail did not ask the forge, " +
		"so main may have moved again."
	if s.Note != want {
		t.Fatalf("note\n got %q\nwant %q", s.Note, want)
	}
}

// Coarse on purpose: a minute's precision would imply a freshness check that
// did not happen. The singular case is here because "1 days ago" is the bug
// this arithmetic writes by default.
func TestStalenessAgeIsCoarseAndReads(t *testing.T) {
	for _, c := range []struct {
		since time.Duration
		want  string
	}{
		{92 * 24 * time.Hour, "92 days"},
		{25 * time.Hour, "1 day"},
		{5 * time.Hour, "5 hours"},
		{90 * time.Minute, "1 hour"},
	} {
		r := oldRepo
		r.IndexedAt = fixedNow.Add(-c.since)
		s := NewCitation(r, logSpan, Newer{}, fixedNow).Staleness
		want := "Correct at commit dfd11cc, indexed " + c.want + " ago. " +
			"codetrail has not checked whether main has moved since."
		if s.Note != want {
			t.Errorf("after %v the note is %q, want %q", c.since, s.Note, want)
		}
	}
}

// The tuple is the claim; the permalink is a convenience over it. The digest
// is the half that survives a forge whose URL shape is unknown, and it is what
// P2's byte-for-byte check against `git show` verified — a citation that
// dropped it would be quotable and not checkable, which is the prose citation
// this product exists to replace.
func TestCitationCarriesTheTupleThatIsTheClaim(t *testing.T) {
	c := NewCitation(oldRepo, logSpan, Newer{}, fixedNow)
	if c.RepoID != oldRepo.ID || c.Remote != oldRepo.Remote ||
		c.Commit != oldRepo.Commit || c.Ref != oldRepo.Ref {
		t.Fatalf("repo half of the tuple: %+v", c)
	}
	if c.Path != logSpan.Path || c.StartLine != 10 || c.EndLine != 20 {
		t.Fatalf("span half of the tuple: %+v", c)
	}
	if c.Digest != logSpan.Digest {
		t.Fatalf("digest %q, want %q", c.Digest, logSpan.Digest)
	}
	want := "https://github.com/rs/zerolog/blob/" + oldRepo.Commit + "/log.go#L10-L20"
	if c.Permalink != want {
		t.Fatalf("permalink %q, want %q", c.Permalink, want)
	}

	// A forge with no known format loses the link and nothing else.
	r := oldRepo
	r.Remote = "https://gitlab.com/group/repo"
	unlinked := NewCitation(r, logSpan, Newer{}, fixedNow)
	if unlinked.Permalink != "" {
		t.Fatalf("an unknown forge rendered %q", unlinked.Permalink)
	}
	if unlinked.Digest != logSpan.Digest || unlinked.EndLine != 20 {
		t.Fatalf("the tuple did not survive the missing link: %+v", unlinked)
	}
}
