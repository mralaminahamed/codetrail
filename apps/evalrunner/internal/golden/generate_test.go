package golden

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// fixture reads a .gotxt fixture and returns it under a .go path, because the
// path is what decides whether Generate parses and strips at all.
func fixture(t *testing.T, name string) (string, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return strings.TrimSuffix(name, "txt"), b
}

func generate(t *testing.T, name string) ([]Case, Stats) {
	t.Helper()
	path, src := fixture(t, name)
	cs, st, err := Generate(path, src)
	if err != nil {
		t.Fatalf("Generate(%s): %v", name, err)
	}
	return cs, st
}

func ids(cs []Case) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func find(t *testing.T, cs []Case, id string) Case {
	t.Helper()
	for _, c := range cs {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no case %q in %v", id, ids(cs))
	return Case{}
}

// declRange is chunk.Decl's answer for the named symbol, over the bytes given.
// The expectation is derived from the chunker rather than written as a literal
// so the two cannot drift; the literals below are asserted as well, because a
// derived expectation alone would move with a chunker bug.
func declRange(t *testing.T, path string, src []byte, symbol string) (int, int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	for _, d := range f.Decls {
		_, sym, start, end, ok := chunk.Decl(fset, d)
		if ok && sym == symbol {
			return start, end
		}
	}
	t.Fatalf("no declaration %q", symbol)
	return 0, 0
}

// The load-bearing test of the phase: the answer's range is the stripped
// source's and the question's is the raw source's, and on a documented
// declaration they are not the same range.
func TestTheAnswerRangeIsTheStrippedOnesAndTheQuestionIsTheRawOnes(t *testing.T) {
	path, src := fixture(t, "simple.gotxt")
	strippedSrc, err := chunk.StripDocs(path, src)
	if err != nil {
		t.Fatalf("StripDocs: %v", err)
	}
	cs, _ := generate(t, "simple.gotxt")

	for _, want := range []struct {
		id                       string
		symbol                   string
		start, end, rStart, rEnd int
	}{
		{"simple.go:Add:decl", "Add", 7, 7, 6, 7},
		{"simple.go:Get:decl", "Get", 11, 11, 9, 11},
		{"simple.go:Legacy:decl", "Legacy", 19, 19, 18, 19},
	} {
		c := find(t, cs, want.id)
		if c.Start != want.start || c.End != want.end {
			t.Errorf("case %q spans %d..%d, want %d..%d (the stripped source's range)",
				c.ID, c.Start, c.End, want.start, want.end)
		}
		if c.RawStart != want.rStart || c.RawEnd != want.rEnd {
			t.Errorf("case %q raw range %d..%d, want %d..%d", c.ID, c.RawStart, c.RawEnd, want.rStart, want.rEnd)
		}
		// Derived, so a literal that drifts from the chunker is caught too.
		if s, e := declRange(t, path, strippedSrc, want.symbol); c.Start != s || c.End != e {
			t.Errorf("case %q spans %d..%d, but chunk.Decl over the stripped source says %d..%d", c.ID, c.Start, c.End, s, e)
		}
		if s, e := declRange(t, path, src, want.symbol); c.RawStart != s || c.RawEnd != e {
			t.Errorf("case %q raw range %d..%d, but chunk.Decl over the raw source says %d..%d", c.ID, c.RawStart, c.RawEnd, s, e)
		}
		// Asserting only that they differ would pass under a mutant that
		// swapped them; asserting only the values would pass on an
		// undocumented fixture where they agree. Both.
		if !c.Moved() {
			t.Errorf("case %q has the same raw and stripped range %d..%d; every documented declaration moves under stripping",
				c.ID, c.Start, c.End)
		}
	}
}

func TestTheQuestionIsTheProseVerbatim(t *testing.T) {
	cs, _ := generate(t, "simple.gotxt")
	for _, want := range []struct{ id, question string }{
		{"simple.go:Add:decl", "Add returns the sum of a and b.\n"},
		{"simple.go:Get:decl", "Get returns the thing named n.\nIt returns an error if n is unknown.\n"},
		{"simple.go:Legacy:decl", "Deprecated.\n"},
	} {
		if got := find(t, cs, want.id).Question; got != want.question {
			t.Errorf("case %q question is %q, want %q", want.id, got, want.question)
		}
	}
}

func TestAnUnexportedMethodOnAnExportedReceiverIsNotACase(t *testing.T) {
	cs, st := generate(t, "exported.gotxt")
	got := ids(cs)
	want := []string{"exported.go:Store:decl", "exported.go:Store.Get:decl"}
	if !equal(got, want) {
		t.Errorf("case list is %v, want %v", got, want)
	}
	// token.IsExported("Store.get") is true and per-segment is false; this is
	// the only shape that separates the two checks.
	if !Exported("Store.Get") || Exported("Store.get") || Exported("cache.Helper") {
		t.Errorf("Exported: Store.Get=%v Store.get=%v cache.Helper=%v; want true false false",
			Exported("Store.Get"), Exported("Store.get"), Exported("cache.Helper"))
	}
	if st.Unexported != 2 {
		t.Errorf("Unexported is %d, want 2 (Store.get and cache.Helper)", st.Unexported)
	}
}

func TestADeclarationWithNoDocCommentIsNotACase(t *testing.T) {
	cs, st := generate(t, "simple.gotxt")
	for _, c := range cs {
		if c.Symbol == "Undocumented" {
			t.Errorf("Undocumented has no doc comment and is a case: %+v", c)
		}
	}
	if st.NoDoc != 1 {
		t.Errorf("NoDoc is %d, want 1", st.NoDoc)
	}

	cs, st = generate(t, "undocumented.gotxt")
	if len(cs) != 0 {
		t.Errorf("a fixture with no doc comments produced %v, want no cases", ids(cs))
	}
	if st.NoDoc != 2 || st.Decls != 3 || st.Imports != 1 {
		t.Errorf("stats are %+v; want Decls 3, Imports 1, NoDoc 2", st)
	}
}

func TestAnImportDeclarationIsNeverACase(t *testing.T) {
	for _, name := range []string{"simple.gotxt", "grouped.gotxt", "undocumented.gotxt"} {
		cs, st := generate(t, name)
		if st.Imports != 1 {
			t.Errorf("%s: Imports is %d, want 1", name, st.Imports)
		}
		for _, c := range cs {
			if c.Kind != models.KindFunc && c.Kind != models.KindType && c.Kind != models.KindConst && c.Kind != models.KindVar {
				t.Errorf("%s: case %q has kind %q, which no declaration Generate accepts can carry", name, c.ID, c.Kind)
			}
		}
	}
}

func TestAGroupedBlockProducesOneCasePerDocumentedExportedName(t *testing.T) {
	cs, st := generate(t, "grouped.gotxt")
	got := ids(cs)
	want := []string{"grouped.go:ErrGone:decl", "grouped.go:ErrGone:spec"}
	if !equal(got, want) {
		t.Errorf("grouped block produced %v, want %v; errHidden is unexported and belongs outside the set", got, want)
	}
	decl := find(t, cs, "grouped.go:ErrGone:decl")
	spec := find(t, cs, "grouped.go:ErrGone:spec")
	if decl.Question != "Errors this package returns.\n" {
		t.Errorf("declaration-level question is %q", decl.Question)
	}
	if spec.Question != "ErrGone is returned when the repo was evicted.\n" {
		t.Errorf("spec-level question is %q", spec.Question)
	}
	// Both point at the one span the block became.
	if decl.Start != spec.Start || decl.End != spec.End || decl.Start != 6 || decl.End != 11 {
		t.Errorf("ranges are decl %d..%d and spec %d..%d, want both 6..11", decl.Start, decl.End, spec.Start, spec.End)
	}
	if !decl.Grouped || !spec.Grouped || st.Grouped != 2 {
		t.Errorf("Grouped flags are %v/%v and the count is %d, want true/true and 2", decl.Grouped, spec.Grouped, st.Grouped)
	}
	if st.SpecDocs != 2 || st.UnexportedSpec != 1 {
		t.Errorf("SpecDocs %d, UnexportedSpec %d; want 2 and 1", st.SpecDocs, st.UnexportedSpec)
	}
}

func TestADeclarationTooLongForASpanStillHasARange(t *testing.T) {
	cs, _ := generate(t, "long.gotxt")
	c := find(t, cs, "long.go:Big:decl")
	if c.Start != 10 || c.End != 33 || c.RawStart != 3 || c.RawEnd != 33 {
		t.Errorf("Big spans %d..%d raw %d..%d, want 10..33 and 3..33", c.Start, c.End, c.RawStart, c.RawEnd)
	}
	// The declaration is longer than the eval fixture's MaxDeclLines, so the
	// AST arm gives it sub-windows and no span carries its symbol. A range is
	// what makes it scoreable at all.
	opt := chunk.Options{Strategy: chunk.StrategyAST, WindowLines: 8, WindowOverlap: 2, MaxDeclLines: 10}
	path, src := fixture(t, "long.gotxt")
	strippedSrc, err := chunk.StripDocs(path, src)
	if err != nil {
		t.Fatalf("StripDocs: %v", err)
	}
	spans, _, err := chunk.Chunks(path, strippedSrc, opt)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	for _, s := range spans {
		if s.Symbol == "Big" {
			t.Fatalf("a span carries the symbol Big: %+v; the fixture is no longer over MaxDeclLines", s)
		}
	}
	if len(spans) != 4 {
		t.Errorf("the AST arm produced %d spans, want 4 sub-windows", len(spans))
	}
}

// The disagreement this refuses cannot be produced by stripping — nothing in
// this repository makes chunk.Decl answer differently for a file and its
// stripped self, and that is recorded as M1.6's survivor. What is testable is
// the guard itself, so it is driven with two parses of two different files.
func TestTheTwoParsesAreMatchedByOrdinalAndDisagreementIsRefused(t *testing.T) {
	parse := func(src string) (*token.FileSet, *ast.File) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		return fset, f
	}
	af, a := parse("package p\n\nfunc A() {}\n")
	bf, b := parse("package p\n\nfunc B() {}\n")
	cf, c := parse("package p\n\nfunc A() {}\nfunc C() {}\n")

	if _, err := pair("p.go", af, a, bf, b); !errors.Is(err, ErrDeclMismatch) {
		t.Errorf("two parses disagreeing on a symbol gave %v, want ErrDeclMismatch", err)
	} else if !strings.Contains(err.Error(), `"A"`) || !strings.Contains(err.Error(), `"B"`) {
		t.Errorf("the refusal is %q; it must name both symbols", err)
	}
	if _, err := pair("p.go", af, a, cf, c); !errors.Is(err, ErrDeclMismatch) {
		t.Errorf("two parses of different lengths gave %v, want ErrDeclMismatch", err)
	}
	if _, err := pair("p.go", af, a, af, a); err != nil {
		t.Errorf("two agreeing parses gave %v, want nil", err)
	}
}

func TestAFileThatWillNotStripProducesNoCasesAndIsCounted(t *testing.T) {
	path, src := fixture(t, "broken.gotxt")
	cs, st, err := Generate(path, src)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(cs) != 0 {
		t.Errorf("a file that does not parse produced %v, want no cases", ids(cs))
	}
	if st.Files != 0 || st.FilesSkipped != 1 || st.SkipReasons[SkipUnparsed] != 1 {
		t.Errorf("stats are %+v, want Files 0, FilesSkipped 1, SkipReasons[%s] 1", st, SkipUnparsed)
	}

	// A non-Go file is a different reason, not the same one: StripDocs passes
	// it through unchanged, so it carries prose into the corpus and generates
	// no case that could ever name it.
	cs, st, err = Generate("NOTES.md", []byte("# Notes\n"))
	if err != nil {
		t.Fatalf("Generate(NOTES.md): %v", err)
	}
	if len(cs) != 0 || st.FilesSkipped != 1 || st.SkipReasons[SkipNotGo] != 1 {
		t.Errorf("NOTES.md gave %v and %+v, want no cases and SkipReasons[%s] 1", ids(cs), st, SkipNotGo)
	}
}

func TestNormaliseMakesAMultiLineDocCommentFindableInSpanText(t *testing.T) {
	path, src := fixture(t, "simple.gotxt")
	cs, _ := generate(t, "simple.gotxt")
	lines := strings.Split(strings.TrimSuffix(string(src), "\n"), "\n")

	// The unstripped span text of Get, which is what an unstripped corpus
	// would hold: its two-line doc comment is inside it, with the "// " that
	// Doc.Text() removed.
	get := find(t, cs, "simple.go:Get:decl")
	unstripped := strings.Join(lines[get.RawStart-1:get.RawEnd], "\n")
	if !strings.Contains(unstripped, "// It returns an error") {
		t.Fatalf("the fixture's unstripped span text lost its second comment line:\n%s", unstripped)
	}
	if !Leaks(get.Question, unstripped) {
		t.Errorf("Leaks reported false for a two-line question whose prose is verbatim in the unstripped span; want true\nquestion: %q\ntext:     %q",
			get.Question, unstripped)
	}
	// Exact containment finds the one-line case either way, so a one-line
	// fixture cannot tell the two spellings apart.
	add := find(t, cs, "simple.go:Add:decl")
	oneLine := strings.Join(lines[add.RawStart-1:add.RawEnd], "\n")
	if strings.Contains(oneLine, strings.TrimSuffix(add.Question, "\n")) != true {
		t.Fatalf("the one-line fixture no longer contains its prose verbatim")
	}
	if !Leaks(add.Question, oneLine) {
		t.Errorf("Leaks reported false for the one-line case; want true")
	}

	// The stripped span text is what the corpus actually holds, and nothing
	// leaks from it.
	strippedSrc, err := chunk.StripDocs(path, src)
	if err != nil {
		t.Fatalf("StripDocs: %v", err)
	}
	slines := strings.Split(strings.TrimSuffix(string(strippedSrc), "\n"), "\n")
	for _, c := range cs {
		text := strings.Join(slines[c.Start-1:c.End], "\n")
		if Leaks(c.Question, text) {
			t.Errorf("case %q leaks into its own stripped span text %q", c.ID, text)
		}
	}
	if got := Normalise("Get returns the thing named n.\nIt returns an error if n is unknown.\n"); got != "get returns the thing named n it returns an error if n is unknown" {
		t.Errorf("Normalise gave %q", got)
	}
	if Leaks("", "anything at all") {
		t.Error("an empty question leaked; the empty needle is inside every haystack")
	}
}

func TestStatsAccountForEveryDeclaration(t *testing.T) {
	for _, name := range []string{"simple.gotxt", "exported.gotxt", "grouped.gotxt", "long.gotxt", "undocumented.gotxt"} {
		cs, st := generate(t, name)
		var fromDecl, fromSpec int
		for _, c := range cs {
			switch c.DocOf {
			case DocOfDecl:
				fromDecl++
			case DocOfSpec:
				fromSpec++
			default:
				t.Errorf("%s: case %q has DocOf %q", name, c.ID, c.DocOf)
			}
		}
		if sum := st.Imports + st.Unchunkable + st.NoDoc + st.Unexported + fromDecl; sum != st.Decls {
			t.Errorf("%s: %d declarations but the buckets hold %d (imports %d + unchunkable %d + nodoc %d + unexported %d + decl cases %d)",
				name, st.Decls, sum, st.Imports, st.Unchunkable, st.NoDoc, st.Unexported, fromDecl)
		}
		if sum := st.UnexportedSpec + fromSpec; sum != st.SpecDocs {
			t.Errorf("%s: %d spec doc comments but the buckets hold %d (unexported %d + spec cases %d)",
				name, st.SpecDocs, sum, st.UnexportedSpec, fromSpec)
		}
		if st.Cases != len(cs) {
			t.Errorf("%s: Cases is %d and the list holds %d", name, st.Cases, len(cs))
		}
		if st.Files+st.FilesSkipped != 1 {
			t.Errorf("%s: Files %d and FilesSkipped %d; a file is one or the other", name, st.Files, st.FilesSkipped)
		}
	}
}

func TestDuplicateQuestionsAreCountedAcrossFilesAndNotSummedPerFile(t *testing.T) {
	src := []byte("package p\n\n// Close releases the handle.\nfunc Close() {}\n")
	a, sa, err := Generate("a.go", src)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, sb, err := Generate("b.go", src)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sa.DuplicateQuestions != 0 || sb.DuplicateQuestions != 0 {
		t.Errorf("per-file duplicate counts are %d and %d, want 0 each", sa.DuplicateQuestions, sb.DuplicateQuestions)
	}
	var merged Stats
	merged.Add(sa)
	merged.Add(sb)
	if merged.DuplicateQuestions != 0 {
		t.Errorf("Add summed DuplicateQuestions to %d; it cannot merge them and must leave them to Duplicates", merged.DuplicateQuestions)
	}
	if got := Duplicates(append(append([]Case{}, a...), b...)); got != 1 {
		t.Errorf("Duplicates over both files is %d, want 1: the same prose in two files is two cases and one duplicate", got)
	}
	if merged.Cases != 2 || merged.Decls != 2 {
		t.Errorf("merged stats are %+v, want 2 cases over 2 declarations", merged)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Measured on sirupsen/logrus, and it stopped a real run: the doc comment on
// `type Level uint32` is the two words "Level type", and a raw normalised
// substring search finds them inside a test file that quotes nothing —
// `logger.SetLevel(logrus.DebugLevel)` followed by `type ctxKey struct{}`
// normalises to "... debuglevel type ctxkey ...", and "level type" sits inside
// "debuglevel type".
//
// One- and two-word doc comments are a deliberate part of this golden set, so
// this is not a rare shape: unfixed, the probe refuses almost any real
// repository and names a file nothing is wrong with.
func TestAShortQuestionDoesNotLeakIntoALongerWord(t *testing.T) {
	const question = "Level type\n"
	const innocent = "func TestHandler(t *testing.T) {\n\tlogger.SetLevel(logrus.DebugLevel)\n\n\ttype ctxKey struct{}\n}"
	if Leaks(question, innocent) {
		t.Errorf("%q leaked into %q; the match runs across a word boundary", question, innocent)
	}
	// The same two words, actually quoted, still leak.
	if !Leaks(question, "// Level type\ntype Level uint32") {
		t.Error("the verbatim prose no longer leaks; the delimiter is too strict")
	}
	// At either end of the text, where the padding is doing the work.
	if !Leaks(question, "Level type") {
		t.Error("a span whose whole text is the prose does not leak")
	}
	if !Leaks("Add returns the sum of a and b.\n", "// Add returns the sum of a and b.\nfunc Add() {}") {
		t.Error("a one-line doc comment no longer leaks")
	}
	// A prefix of a longer word at the start, and a suffix at the end.
	if Leaks("Add returns\n", "// Readd returns nothing") {
		t.Error("the needle matched inside a longer leading word")
	}
	if Leaks("returns the sum\n", "returns the summary") {
		t.Error("the needle matched inside a longer trailing word")
	}
}
