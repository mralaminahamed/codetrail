// Package golden derives the eval's questions from doc-comment prose, and the
// coordinates their answers live in.
//
// The question is the prose verbatim and the answer is the declaration's line
// range — but not the same file's. Spec:244 says the set is "generated, not
// hand-picked", and spec:177 says the corpus is indexed with doc comments
// stripped, so the question comes out of the raw source and the range has to
// come out of the stripped one. chunk.Decl starts a declaration at its doc
// comment; a blanked comment is no longer one, and P2 measured 506 of
// rs/zerolog's 1,303 AST spans starting later in the stripped corpus.
package golden

import (
	"errors"
	"strings"
	"unicode"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// ErrDeclMismatch is the raw and stripped parses disagreeing at an ordinal.
//
// A refusal rather than a repair: stripping cannot add, remove or reorder a
// declaration, so a disagreement means the two parses are not of the same file
// and every range this package would emit names lines the corpus does not
// have. There is no fixture in this repository that produces it — recorded as
// such rather than claimed to be tested.
var ErrDeclMismatch = errors.New("golden: the raw and stripped parses disagree")

// DocOf says which comment a case's question came from: the declaration's own,
// or one spec inside a parenthesised block.
const (
	DocOfDecl = "decl"
	DocOfSpec = "spec"
)

// Case is one generated question and the answer's coordinates.
//
// Start/End are the *stripped* source's range, which is the corpus's. RawStart
// and RawEnd are the unstripped source's, recorded beside them because the
// difference is a figure this phase has to report: how many cases moved, and
// by how much.
type Case struct {
	ID       string          `json:"id"`
	Path     string          `json:"path"`
	Symbol   string          `json:"symbol"`
	Question string          `json:"question"`
	Kind     models.SpanKind `json:"kind"`
	Start    int             `json:"start"`
	End      int             `json:"end"`
	RawStart int             `json:"raw_start"`
	RawEnd   int             `json:"raw_end"`
	Grouped  bool            `json:"grouped"`
	DocOf    string          `json:"doc_of"`
}

// Moved reports whether stripping moved this case's range.
func (c Case) Moved() bool { return c.Start != c.RawStart || c.End != c.RawEnd }

// Stats is the ledger. Every declaration the parser saw lands in exactly one
// of Imports, Unchunkable, NoDoc, Unexported or a decl-level case, and every
// spec-level doc comment lands in UnexportedSpec or a spec-level case. Without
// the identity a mutant that drops a case reduces Cases and nothing notices.
type Stats struct {
	Files        int `json:"files"`
	FilesSkipped int `json:"files_skipped"`
	Decls        int `json:"decls"`
	// Imports are refused by chunk.Decl: an import block is not a retrievable
	// unit and there is no span a question about one could name.
	Imports        int `json:"imports"`
	Unchunkable    int `json:"unchunkable"`
	NoDoc          int `json:"no_doc"`
	Unexported     int `json:"unexported"`
	SpecDocs       int `json:"spec_docs"`
	UnexportedSpec int `json:"unexported_spec"`
	// FieldDocs counts struct and interface field doc comments. StripDocs
	// blanks them, so the prose leaves the corpus, but a field is not a
	// top-level declaration and no span answers it. Counted rather than
	// ignored, because "this prose produced no case" is a fact about the set.
	FieldDocs          int            `json:"field_docs"`
	Cases              int            `json:"cases"`
	Grouped            int            `json:"grouped"`
	DuplicateQuestions int            `json:"duplicate_questions"`
	SkipReasons        map[string]int `json:"skip_reasons,omitempty"`
}

// Add merges another file's ledger in.
//
// DuplicateQuestions is deliberately not summed: two files can carry the same
// prose, and adding per-file counts would report fewer duplicates than the
// corpus has. The caller recomputes it with Duplicates over the whole set.
func (s *Stats) Add(o Stats) {
	s.Files += o.Files
	s.FilesSkipped += o.FilesSkipped
	s.Decls += o.Decls
	s.Imports += o.Imports
	s.Unchunkable += o.Unchunkable
	s.NoDoc += o.NoDoc
	s.Unexported += o.Unexported
	s.SpecDocs += o.SpecDocs
	s.UnexportedSpec += o.UnexportedSpec
	s.FieldDocs += o.FieldDocs
	s.Cases += o.Cases
	s.Grouped += o.Grouped
	for k, v := range o.SkipReasons {
		if s.SkipReasons == nil {
			s.SkipReasons = map[string]int{}
		}
		s.SkipReasons[k] += v
	}
}

// Duplicates counts the cases whose question is not the first of its wording.
// Duplicate prose is kept — deduplicating is hand-picking — so the count is
// what a reader gets instead.
func Duplicates(cases []Case) int {
	seen := make(map[string]bool, len(cases))
	n := 0
	for _, c := range cases {
		if seen[c.Question] {
			n++
			continue
		}
		seen[c.Question] = true
	}
	return n
}

// Exported reports whether every dot-separated segment of a symbol is
// exported.
//
// Measured: token.IsExported("Store.get") is true, because it reads the first
// rune. A method a caller outside the package cannot reach is not an exported
// symbol, and admitting it puts a case in the set whose correct answer nobody
// can ask for.
func Exported(symbol string) bool {
	if symbol == "" {
		return false
	}
	for _, seg := range strings.Split(symbol, ".") {
		if seg == "" {
			return false
		}
		r := []rune(seg)[0]
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// Normalise is the form the leakage probe compares in: lower case, with every
// run of non-alphanumeric characters collapsed to one space.
//
// Exact containment would find a one-line doc comment inside unstripped span
// text — the prose survives verbatim inside "// …" — and miss a multi-line
// one, because Doc.Text() drops the "// " from the second line and the span
// text keeps it. A probe that misses the multi-line case passes on the corpus
// it exists to refuse.
func Normalise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		space = true
	}
	return b.String()
}

// Leaks reports whether a question's prose is findable in a span's text.
func Leaks(question, text string) bool {
	return Contains(Normalise(text), Normalise(question))
}

// Contains reports whether an already-normalised question appears in
// already-normalised text as a whole phrase.
//
// Delimited, not a raw substring. Measured on sirupsen/logrus: the doc comment
// on `type Level uint32` is the two words "Level type", and a raw substring
// search finds them inside `logger.SetLevel(logrus.DebugLevel)\n\ttype ctxKey`,
// which normalises to "... debuglevel type ctxkey ...". The probe refused a
// correctly stripped corpus and named a test file that quotes nothing. A short
// question is not a rare shape — one- and two-word doc comments are a
// deliberate part of this golden set — so the false positive would have
// stopped a run on almost any real repository.
//
// An empty question never leaks: the empty needle is inside every haystack.
func Contains(text, question string) bool {
	if question == "" {
		return false
	}
	// Normalise collapses to single spaces, so padding both ends makes a
	// space the only word delimiter there is.
	return strings.Contains(" "+text+" ", " "+question+" ")
}
