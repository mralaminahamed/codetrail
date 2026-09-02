package symbols

import (
	"os"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	src, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// The fixtures are .gotxt and the filename handed to both packages is .go:
// chunk windows anything else, and a .go file under testdata would have to
// compile with the module.
const path = "sample.go"

func parse(t *testing.T, src []byte) File {
	t.Helper()
	f, err := Parse(path, src)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// chunks returns what chunk makes of src under opt.
func chunks(t *testing.T, src []byte, opt chunk.Options) []chunk.Chunk {
	t.Helper()
	cs, _, err := chunk.Chunks(path, src, opt)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// wholeDecls is chunk's configuration with sub-windowing out of reach, for the
// tests that are about the spelling and the range rather than about the span
// hole.
func wholeDecls() chunk.Options {
	o := chunk.Defaults()
	o.MaxDeclLines = 10000
	return o
}

// The test this task exists for: it asserts against chunk's output rather than
// against a literal, so one symbol cannot come to have two spellings without a
// failure here. A literal-vs-literal pair agrees on the day it is written.
func TestEveryDeclarationsSymbolIsSpelledAsItsChunkSpellsIt(t *testing.T) {
	for _, name := range []string{"calls.gotxt", "twoget.gotxt", "docs.gotxt", "long.gotxt"} {
		t.Run(name, func(t *testing.T) {
			src := fixture(t, name)
			cs := chunks(t, src, wholeDecls())
			defs := parse(t, src).Defs
			if len(defs) != len(cs) {
				for _, d := range defs {
					t.Logf("def  %s %s %d..%d", d.Kind, d.Name, d.StartLine, d.EndLine)
				}
				for _, c := range cs {
					t.Logf("span %s %s %d..%d", c.Kind, c.Symbol, c.StartLine, c.EndLine)
				}
				t.Fatalf("%d definitions, %d spans", len(defs), len(cs))
			}
			for i, d := range defs {
				c := cs[i]
				if d.Kind != c.Kind || d.Name != c.Symbol || d.StartLine != c.StartLine || d.EndLine != c.EndLine {
					t.Fatalf("definition %d is %s/%q %d..%d, its span is %s/%q %d..%d",
						i, d.Kind, d.Name, d.StartLine, d.EndLine,
						c.Kind, c.Symbol, c.StartLine, c.EndLine)
				}
			}
		})
	}
}

// A method is Recv.Method, with the pointer and any type parameters dropped —
// the string spans.symbol already holds and P3's lexical arm already indexes
// through replace(symbol,'.',' '). Three types share the name Get: a fixture
// whose names were unique would still find "a definition named Get" with the
// receiver thrown away.
func TestAMethodIsNamedByItsReceiverBase(t *testing.T) {
	got := parse(t, fixture(t, "twoget.gotxt"))
	if got.Pkg != "twoget" {
		t.Fatalf("package is %q, want the package clause %q", got.Pkg, "twoget")
	}
	want := []Def{
		{models.KindType, "Store", "twoget", path, 3, 5},
		{models.KindFunc, "Store.Get", "twoget", path, 7, 8},
		{models.KindType, "Cache", "twoget", path, 10, 10},
		{models.KindFunc, "Cache.Get", "twoget", path, 12, 13},
		{models.KindType, "Pair", "twoget", path, 15, 16},
		{models.KindFunc, "Pair.Get", "twoget", path, 18, 19},
	}
	if len(got.Defs) != len(want) {
		for _, d := range got.Defs {
			t.Logf("%s %s %d..%d", d.Kind, d.Name, d.StartLine, d.EndLine)
		}
		t.Fatalf("got %d definitions, want %d", len(got.Defs), len(want))
	}
	for i, w := range want {
		if got.Defs[i] != w {
			t.Fatalf("definition %d is %s/%q %s %s %d..%d, want %s/%q %s %s %d..%d",
				i, got.Defs[i].Kind, got.Defs[i].Name, got.Defs[i].Pkg, got.Defs[i].Path,
				got.Defs[i].StartLine, got.Defs[i].EndLine,
				w.Kind, w.Name, w.Pkg, w.Path, w.StartLine, w.EndLine)
		}
	}
}

// A span starts at the doc comment, so a definition must too: one range from
// one function, and the two cannot disagree. Asserted against the chunk and
// against the fixture's own lines, so a mutant that moved both would still
// have to move the fixture.
func TestADefinitionsRangeIncludesItsDocComment(t *testing.T) {
	src := fixture(t, "docs.gotxt")
	ls := strings.Split(strings.TrimSuffix(string(src), "\n"), "\n")
	defs := parse(t, src).Defs
	cs := chunks(t, src, wholeDecls())
	if len(defs) != 1 || len(cs) != 1 {
		t.Fatalf("got %d definitions and %d spans, want 1 of each", len(defs), len(cs))
	}
	d, c := defs[0], cs[0]
	if d.StartLine != c.StartLine || d.EndLine != c.EndLine {
		t.Fatalf("definition %s spans %d..%d, its chunk spans %d..%d",
			d.Name, d.StartLine, d.EndLine, c.StartLine, c.EndLine)
	}
	if !strings.HasPrefix(ls[d.StartLine-1], "// Parse turns bytes") {
		t.Fatalf("%s starts at line %d, which holds %q, want its doc comment",
			d.Name, d.StartLine, ls[d.StartLine-1])
	}
}

// The hole this package exists to fill: over MaxDeclLines the declaration is
// sub-windowed and no span has its range, so a symbols row that borrowed the
// span's identity would lose exactly the largest declarations. Task 2 links
// them by containment instead.
func TestADeclarationTooLongForASpanStillHasADefinition(t *testing.T) {
	src := fixture(t, "long.gotxt")
	defs := parse(t, src).Defs
	want := Def{models.KindFunc, "Big", "long", path, 3, 29}
	if len(defs) != 1 || defs[0] != want {
		t.Fatalf("got %+v, want one %+v", defs, want)
	}
	// Scaled down from the shipped defaults (200-line declarations, 40-line
	// windows): what makes the hole is the ratio, and a fixture of 200 lines
	// would be 200 lines of n++.
	o := chunk.Defaults()
	o.MaxDeclLines, o.WindowLines, o.WindowOverlap = 10, 10, 2
	cs := chunks(t, src, o)
	if len(cs) < 2 {
		t.Fatalf("a %d-line declaration produced %d spans, so it was not sub-windowed", want.EndLine-want.StartLine+1, len(cs))
	}
	for _, c := range cs {
		if c.Kind != models.KindFile || c.Symbol != "" {
			t.Fatalf("sub-window is %s/%q, want file/empty", c.Kind, c.Symbol)
		}
		if c.StartLine == want.StartLine && c.EndLine == want.EndLine {
			t.Fatalf("a span spans %d..%d, so this fixture does not have the hole it is for", c.StartLine, c.EndLine)
		}
	}
}

// doc.go and tools.go produce zero spans because f.Doc is not a Decl and an
// import is refused. Definitions have the same hole, and it is the harmless
// one: zero declarations is zero definitions, consistent with zero spans.
func TestAFileWithNoDeclarationsHasNoDefinitions(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"doc.go", "// Package p explains the package.\n//\n// At length.\npackage p\n"},
		{"tools.go", "//go:build tools\n\npackage tools\n\nimport (\n\t_ \"example.com/x/tool\"\n)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parse(t, []byte(tc.src))
			if len(got.Defs) != 0 || len(got.Calls) != 0 {
				t.Fatalf("got %d definitions and %d calls, want none", len(got.Defs), len(got.Calls))
			}
			// The consistency claim: chunk sees nothing here either.
			if cs := chunks(t, []byte(tc.src), wholeDecls()); len(cs) != 0 {
				t.Fatalf("chunk produced %d spans, so the two passes disagree", len(cs))
			}
		})
	}

	// A file that does not parse is the same outcome plus an error the caller
	// counts: chunk falls back to windows rather than failing, and this pass
	// must not be stricter. The fixture is chunk's, so the two packages agree
	// on what "does not parse" means.
	t.Run("does not parse", func(t *testing.T) {
		src, err := os.ReadFile("../chunk/testdata/broken.gotxt")
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(path, src)
		if err == nil {
			t.Fatal("Parse accepted a file that does not parse")
		}
		if len(got.Defs) != 0 || len(got.Calls) != 0 || got.Pkg != "" || got.Unnameable != 0 {
			t.Fatalf("Parse returned %d definitions, %d calls, pkg %q and %d unnameable with error %q, want an empty File",
				len(got.Defs), len(got.Calls), got.Pkg, got.Unnameable, err)
		}
	})
}

// An import block is not a definition, matching the span chunk refuses to make
// of it.
func TestImportsAreNotDefinitions(t *testing.T) {
	got := parse(t, fixture(t, "docs.gotxt"))
	if len(got.Defs) != 1 {
		for _, d := range got.Defs {
			t.Logf("%s %s %d..%d", d.Kind, d.Name, d.StartLine, d.EndLine)
		}
		t.Fatalf("got %d definitions, want only Parse", len(got.Defs))
	}
	if got.Defs[0].Name != "Parse" {
		t.Fatalf("the one definition is %q, want Parse", got.Defs[0].Name)
	}
}

// a(b(), b()) is two calls, and Task 2 hashes the key into the edge id — so
// what matters is not that two Calls come back but that their keys differ. A
// line key yields two calls that become one row.
func TestTwoCallsOnOneLineAreTwoCalls(t *testing.T) {
	got := parse(t, fixture(t, "calls.gotxt"))
	var bs []Call
	for _, c := range got.Calls {
		if c.Name == "b" && c.FromStart == 9 {
			bs = append(bs, c)
		}
	}
	if len(bs) != 2 {
		t.Fatalf("got %d calls to b in twoOnALine, want 2", len(bs))
	}
	if bs[0].Line != 12 || bs[1].Line != 12 {
		t.Fatalf("the two calls to b are on lines %d and %d, want both on 12 — this fixture is not the one call per line a line key survives",
			bs[0].Line, bs[1].Line)
	}
	if bs[0].Offset == bs[1].Offset {
		t.Fatalf("two calls to b on line 12 share the key %d, so they are one edge", bs[0].Offset)
	}
	if bs[0].Path != path {
		t.Fatalf("call carries path %q, want %q", bs[0].Path, path)
	}
}

// spec:84 — a syntactic edge "knows it calls something named Close". The
// x.y.Z row is the one that separates the last segment from the last two.
func TestTheCalleeNameIsItsLastSegment(t *testing.T) {
	src := []byte("package p\n\nfunc caller() {\n\tpkg.Fn()\n\ts.Get()\n\tx.y.Z()\n\tClose()\n}\n")
	got := parse(t, src)
	want := []string{"Fn", "Get", "Z", "Close"}
	if len(got.Calls) != len(want) {
		for _, c := range got.Calls {
			t.Logf("%q at %d", c.Name, c.Offset)
		}
		t.Fatalf("got %d calls, want %d", len(got.Calls), len(want))
	}
	for i, w := range want {
		if got.Calls[i].Name != w {
			t.Fatalf("callee %d named %q, want %q", i, got.Calls[i].Name, w)
		}
	}
}

// f()(), fns[i]() and func(){}() name nothing. edges.to_name is NOT NULL, so
// the choice is between counting them and writing an edge that says it calls
// something called nothing.
func TestACalleeWithNoNameIsCountedRatherThanGuessed(t *testing.T) {
	got := parse(t, fixture(t, "calls.gotxt"))
	if len(got.Calls) != 6 || got.Unnameable != 3 {
		for _, c := range got.Calls {
			t.Logf("%q line %d from %d", c.Name, c.Line, c.FromStart)
		}
		t.Fatalf("got %d calls and %d unnameable, want 6 and 3", len(got.Calls), got.Unnameable)
	}
	for i, c := range got.Calls {
		if c.Name == "" {
			t.Fatalf("call %d at line %d has no name", i, c.Line)
		}
	}
	// The nameable ones inside the same declaration, so the count above is
	// not satisfied by a pass that dropped a real call and gained a blank.
	var inUnnameable []string
	for _, c := range got.Calls {
		if c.FromStart == 15 {
			inUnnameable = append(inUnnameable, c.Name)
		}
	}
	if strings.Join(inUnnameable, ",") != "f,b" {
		t.Fatalf("unnameable's nameable callees are %v, want [f b]", inUnnameable)
	}
}

// The tail of an edge is the definition the call is in. A call inside a func
// literal inside a var declaration belongs to the var: it is the only
// definition there is.
func TestACallInsideAFuncLiteralBelongsToTheEnclosingDeclaration(t *testing.T) {
	got := parse(t, fixture(t, "calls.gotxt"))
	var g []Call
	for _, c := range got.Calls {
		if c.Name == "g" {
			g = append(g, c)
		}
	}
	if len(g) != 1 {
		t.Fatalf("got %d calls to g, want 1", len(g))
	}
	// 22 is the var declaration's doc comment, which is where its definition
	// starts; 3 is the first declaration in the file.
	if g[0].FromStart != 22 {
		t.Fatalf("call to g is in the declaration starting at line %d, want 22", g[0].FromStart)
	}
}

// Measured here rather than assumed, because Task 3 keys resolutions into
// bytes go/packages reads from disk while the chunker may have read stripped
// bytes.
//
// The measurement contradicts the plan: StripDocs does not blank doc bytes in
// place, it *deletes* them and keeps only their line terminators. So line
// numbers are invariant and byte offsets are not — every offset below a doc
// comment moves by the prose removed above it (125 bytes in this fixture).
// The consequence is Task 4's: the graph pass must be handed the same bytes
// go/packages reads, which is the indexer's `body`, never its stripped `src`.
func TestLineNumbersSurviveDocCommentStrippingAndOffsetsDoNot(t *testing.T) {
	src := fixture(t, "docs.gotxt")
	stripped, err := chunk.StripDocs(path, src)
	if err != nil {
		t.Fatal(err)
	}
	if string(stripped) == string(src) {
		t.Fatal("StripDocs changed nothing, so this fixture proves nothing")
	}
	raw, blank := parse(t, src), parse(t, stripped)
	if len(raw.Calls) != len(blank.Calls) || len(raw.Calls) == 0 {
		t.Fatalf("%d calls before stripping, %d after", len(raw.Calls), len(blank.Calls))
	}
	for i, c := range raw.Calls {
		b := blank.Calls[i]
		if c.Name != b.Name || c.Line != b.Line {
			t.Fatalf("call %d is %q on line %d, and %q on line %d once stripped — a line moved, so the two streams share no coordinate at all",
				i, c.Name, c.Line, b.Name, b.Line)
		}
	}
	// Pinned as values, so the day stripping starts preserving length this
	// test says so instead of quietly agreeing.
	type key struct{ raw, stripped int }
	want := []key{{194, 69}, {226, 101}}
	for i, w := range want {
		if got := (key{raw.Calls[i].Offset, blank.Calls[i].Offset}); got != w {
			t.Fatalf("call %d %q is at offset %d raw and %d stripped, want %d and %d",
				i, raw.Calls[i].Name, got.raw, got.stripped, w.raw, w.stripped)
		}
	}
	// The range moves too, by exactly the doc comment: 5..14 with its
	// three-line doc, 8..14 once the doc is not a comment any more.
	if len(raw.Defs) != 1 || len(blank.Defs) != 1 {
		t.Fatalf("%d definitions before stripping, %d after", len(raw.Defs), len(blank.Defs))
	}
	if raw.Defs[0].StartLine != 5 || blank.Defs[0].StartLine != 8 ||
		raw.Defs[0].EndLine != 14 || blank.Defs[0].EndLine != 14 {
		t.Fatalf("Parse is %d..%d raw and %d..%d stripped, want 5..14 and 8..14",
			raw.Defs[0].StartLine, raw.Defs[0].EndLine, blank.Defs[0].StartLine, blank.Defs[0].EndLine)
	}
}
