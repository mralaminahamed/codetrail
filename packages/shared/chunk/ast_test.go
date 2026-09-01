package chunk

import (
	"os"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

func fixture(t *testing.T, name string) ([]byte, []string) {
	t.Helper()
	src, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return src, strings.Split(strings.TrimSuffix(string(src), "\n"), "\n")
}

func astOpts() Options { o := Defaults(); o.Strategy = StrategyAST; return o }

// One span per top-level declaration, imports excluded, with the kinds and
// symbols the schema stores. The filename is not a path that exists: the
// parser is handed src, so nothing is read from disk.
func TestASTChunksOneSpanPerDeclaration(t *testing.T) {
	src, _ := fixture(t, "decls.gotxt")
	got, err := Chunks("sample.go", src, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		kind   models.SpanKind
		symbol string
	}{
		{models.KindVar, "ErrGone"},
		{models.KindConst, "KindA"},
		{models.KindType, "Counter"},
		{models.KindFunc, "Counter.Add"},
		{models.KindFunc, "undocumented"},
	}
	if len(got) != len(want) {
		for _, c := range got {
			t.Logf("%s %s %d..%d", c.Kind, c.Symbol, c.StartLine, c.EndLine)
		}
		t.Fatalf("got %d chunks, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Symbol != w.symbol {
			t.Fatalf("chunk %d is %s/%s, want %s/%s", i, got[i].Kind, got[i].Symbol, w.kind, w.symbol)
		}
	}
}

// The invariant models.Span calls not-cosmetic. Anchored on what the fixture's
// own lines say, so a uniform shift of both ends is caught: a text-equals-slice
// check would move with the shift and pass.
func TestASTLineRangesAreOneBasedInclusive(t *testing.T) {
	src, ls := fixture(t, "decls.gotxt")
	got, err := Chunks("sample.go", src, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Chunk{}
	for _, c := range got {
		byName[c.Symbol] = c
	}
	for _, tc := range []struct{ symbol, first, last string }{
		{"ErrGone", "// ErrGone is returned", "var ErrGone = errors.New"},
		{"KindA", "// Kinds are the shapes", ")"},
		{"Counter", "// Counter counts.", "}"},
		{"Counter.Add", "// Add adds d and returns", "}"},
		{"undocumented", "func undocumented()", "func undocumented()"},
	} {
		c, ok := byName[tc.symbol]
		if !ok {
			t.Fatalf("no chunk for %s", tc.symbol)
		}
		if c.StartLine < 1 || c.EndLine > len(ls) || c.EndLine < c.StartLine {
			t.Fatalf("%s: range %d..%d is outside 1..%d", tc.symbol, c.StartLine, c.EndLine, len(ls))
		}
		if !strings.HasPrefix(ls[c.StartLine-1], tc.first) {
			t.Fatalf("%s starts at line %d, which holds %q, want a line starting %q",
				tc.symbol, c.StartLine, ls[c.StartLine-1], tc.first)
		}
		if !strings.HasPrefix(strings.TrimSpace(ls[c.EndLine-1]), tc.last) {
			t.Fatalf("%s ends at line %d, which holds %q, want a line starting %q",
				tc.symbol, c.EndLine, ls[c.EndLine-1], tc.last)
		}
		if c.Text != strings.Join(ls[c.StartLine-1:c.EndLine], "\n") {
			t.Fatalf("%s: text is not the lines %d..%d", tc.symbol, c.StartLine, c.EndLine)
		}
	}
}

// Spec §5: the span carries its doc comment. This is also what makes the
// stripped corpus of Task 3 a different corpus rather than the same one.
func TestSpanTextCarriesTheDocComment(t *testing.T) {
	src, _ := fixture(t, "decls.gotxt")
	got, _ := Chunks("sample.go", src, astOpts())
	for _, c := range got {
		if c.Symbol == "Counter.Add" {
			if !strings.Contains(c.Text, "// Add adds d and returns the new total.") ||
				!strings.Contains(c.Text, "spans more than one line") {
				t.Fatalf("Counter.Add lost its doc comment:\n%s", c.Text)
			}
			return
		}
	}
	t.Fatal("no chunk for Counter.Add")
}

// Nothing spans two declarations, and no two spans overlap. An overlap would
// mean one line answers as two documents.
func TestASTChunksDoNotOverlap(t *testing.T) {
	src, _ := fixture(t, "decls.gotxt")
	got, _ := Chunks("sample.go", src, astOpts())
	for i := 1; i < len(got); i++ {
		if got[i].StartLine <= got[i-1].EndLine {
			t.Fatalf("%s (%d..%d) overlaps %s (%d..%d)",
				got[i].Symbol, got[i].StartLine, got[i].EndLine,
				got[i-1].Symbol, got[i-1].StartLine, got[i-1].EndLine)
		}
	}
}

// A method is searched for as Recv.Method, with the pointer and any type
// parameters dropped: a generic receiver names the same type as a plain one.
func TestMethodSymbolsDropPointerAndTypeParameters(t *testing.T) {
	src := []byte("package p\n\n" +
		"func (s *Stack[T]) Push(x T) {}\n\n" +
		"func (m Pair[K, V]) Get(k K) V { var v V; return v }\n\n" +
		"func (c Counter) N() int { return 0 }\n")
	got, err := Chunks("g.go", src, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Stack.Push", "Pair.Get", "Counter.N"}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Symbol != w {
			t.Fatalf("chunk %d is %q, want %q", i, got[i].Symbol, w)
		}
	}
}

// //line directives retarget go/token's positions at the generator's source.
// A span is cited against the file that was indexed, so the range has to be
// this file's: under the adjusted position F is one line long and starts where
// the generator's line 1 lands. (With a directive numbered past the end of the
// file, which is the common case in generated Go, it is a slice out of range.)
func TestLineDirectivesDoNotMoveRanges(t *testing.T) {
	src := []byte("package p\n\n//line sample.y:1\nfunc F() {\n\t_ = 1\n}\n")
	got, err := Chunks("gen.go", src, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	c := got[0]
	// The directive is the declaration's doc comment, so the span starts on
	// it: 3..6, the file's own lines, not the generator's.
	if c.StartLine != 3 || c.EndLine != 6 {
		t.Fatalf("F is %d..%d, want 3..6 — this file has 6 lines", c.StartLine, c.EndLine)
	}
	if c.Text != "//line sample.y:1\nfunc F() {\n\t_ = 1\n}" {
		t.Fatalf("F's text is not the lines %d..%d: %q", c.StartLine, c.EndLine, c.Text)
	}
}

// A file that will not parse falls back to windows under StrategyAST, and does
// not fail. This is the fallback half of the strategy.
func TestUnparseableGoFallsBackToWindows(t *testing.T) {
	src, ls := fixture(t, "broken.gotxt")
	o := astOpts()
	got, err := Chunks("broken.go", src, o)
	if err != nil {
		t.Fatalf("a file that will not parse is a fallback, not an error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no chunks at all")
	}
	// The fixture is longer than one window on purpose: a fallback that
	// emitted one chunk per file would satisfy every other assertion here.
	if len(got) < 2 {
		t.Fatalf("%d lines at %d per window produced %d chunks", len(ls), o.WindowLines, len(got))
	}
	if got[len(got)-1].EndLine != len(ls) {
		t.Fatalf("windows stop at line %d, want %d", got[len(got)-1].EndLine, len(ls))
	}
	for _, c := range got {
		if c.Kind != models.KindFile || c.Symbol != "" {
			t.Fatalf("fallback produced %s/%s, want file/empty", c.Kind, c.Symbol)
		}
	}
}

// Not-Go is windowed without the parser being asked. Markdown that happens to
// contain "func (" must not decide anything.
func TestNonGoFileIsWindowed(t *testing.T) {
	md := []byte("# Title\n\nfunc (c *Counter) Add(d int) int {\n\nprose\n")
	got, err := Chunks("README.md", md, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Not only "every chunk is a window" — markdown does not parse as Go, so
	// a chunker that dropped the extension check would produce *no* chunks
	// and this loop would vacuously pass.
	if len(got) == 0 {
		t.Fatal("markdown produced no chunks")
	}
	for _, c := range got {
		if c.Kind != models.KindFile {
			t.Fatalf("markdown produced %s", c.Kind)
		}
	}
	// Markdown alone does not test the extension check: it fails to parse, so
	// the parse fallback windows it either way. What the check decides is a
	// non-Go file whose bytes *are* valid Go — a .gotxt fixture, a vendored
	// .go.orig — which parses cleanly and must still be windowed.
	src, _ := fixture(t, "decls.gotxt")
	got, err = Chunks("decls.gotxt", src, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("decls.gotxt produced no chunks")
	}
	for _, c := range got {
		if c.Kind != models.KindFile || c.Symbol != "" {
			t.Fatalf("valid Go named decls.gotxt produced %s/%s, want file/empty", c.Kind, c.Symbol)
		}
	}
}

// The window strategy never parses — not even Go that parses cleanly. Task 1
// could not test that against a stub; with a real AST path behind the seam,
// this is what keeps the baseline arm from quietly becoming the AST arm.
func TestWindowStrategyIgnoresParseableGo(t *testing.T) {
	src, ls := fixture(t, "decls.gotxt")
	got, err := Chunks("sample.go", src, winOpts(5, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("parseable Go produced no windows")
	}
	for _, c := range got {
		if c.Kind != models.KindFile || c.Symbol != "" {
			t.Fatalf("window strategy produced %s/%s, want file/empty", c.Kind, c.Symbol)
		}
	}
	if got[0].StartLine != 1 || got[0].EndLine != 5 || got[len(got)-1].EndLine != len(ls) {
		t.Fatalf("windows are %d..%d … %d, not the 5-line tiling of %d lines",
			got[0].StartLine, got[0].EndLine, got[len(got)-1].EndLine, len(ls))
	}
}

// Spec §5: one generated 3,000-line file must not become a single useless
// span. Over the threshold the declaration is sub-windowed.
func TestOverlongDeclarationIsSubWindowed(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\n// Big is generated.\nfunc Big() {\n")
	for i := 0; i < 300; i++ {
		b.WriteString("\t_ = 1\n")
	}
	b.WriteString("}\n")
	o := astOpts()
	o.MaxDeclLines = 50
	got, err := Chunks("big.go", []byte(b.String()), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("a %d-line declaration produced %d chunks", 305, len(got))
	}
	for _, c := range got {
		if c.Kind != models.KindFile || c.Symbol != "" {
			t.Fatalf("sub-window is %s/%s, want file/empty", c.Kind, c.Symbol)
		}
		if n := c.EndLine - c.StartLine + 1; n > o.MaxDeclLines {
			t.Fatalf("sub-window is %d lines, over the %d threshold", n, o.MaxDeclLines)
		}
	}
	if got[0].StartLine != 3 {
		t.Fatalf("sub-windows start at %d, want 3 — the declaration's doc comment", got[0].StartLine)
	}
}

// Exactly at the threshold is one span, not sub-windows. An off-by-one here
// would shred ordinary declarations into windows and quietly turn the AST arm
// into the baseline arm.
func TestDeclarationAtTheThresholdIsOneSpan(t *testing.T) {
	o := astOpts()
	o.MaxDeclLines = 10
	var b strings.Builder
	b.WriteString("package p\n\nfunc Exactly() {\n") // decl starts at line 3
	for i := 0; i < 8; i++ {
		b.WriteString("\t_ = 1\n")
	}
	b.WriteString("}\n") // decl is lines 3..12 inclusive = 10 lines
	got, err := Chunks("x.go", []byte(b.String()), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != models.KindFunc || got[0].Symbol != "Exactly" {
		for _, c := range got {
			t.Logf("%s %s %d..%d", c.Kind, c.Symbol, c.StartLine, c.EndLine)
		}
		t.Fatalf("a declaration of exactly %d lines was not one span", o.MaxDeclLines)
	}
}
