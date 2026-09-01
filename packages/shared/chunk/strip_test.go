package chunk

import (
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

func stripped(t *testing.T, name string) ([]byte, []byte, []string) {
	t.Helper()
	src, ls := fixture(t, name)
	out, err := StripDocs("docs.go", src)
	if err != nil {
		t.Fatal(err)
	}
	return src, out, ls
}

// The invariant the whole design rests on: line numbers do not move, so a
// window over the stripped source covers the same lines as one over the
// original and the two arms stay comparable.
//
// The fixture's doc comments include a seven-line /* */ block, which is the
// only place a stripper can swallow a newline — a comment's Pos..End stops
// before the newline that ends a // line, so a fixture of line comments alone
// cannot tell blanking from dropping newlines and would prove nothing.
func TestStripDocsPreservesEveryLineNumber(t *testing.T) {
	_, out, ls := stripped(t, "docs.gotxt")
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(ls) {
		t.Fatalf("stripped source has %d lines, original has %d", len(got), len(ls))
	}
	// Blanking empties a line or leaves its indentation; it never rewrites one.
	for i := range got {
		if strings.TrimSpace(got[i]) != "" && got[i] != ls[i] {
			t.Fatalf("line %d changed: %q -> %q", i+1, ls[i], got[i])
		}
	}
	// Pinned by number, because "unchanged content" alone would still hold if
	// every line slid up together. Line 25 is the one directly below the block
	// comment, so it is the first to move if its newlines go.
	for _, tc := range []struct {
		line int
		want string
	}{
		{1, "//go:build linux"},
		{7, "package docs"},
		{16, `var ErrGone = errors.New("gone")`},
		{25, "const ("},
		{41, "func (c *Counter) Add(d int) int {"},
		{48, `func undocumented() error { return fmt.Errorf("no doc") }`},
	} {
		if got[tc.line-1] != tc.want {
			t.Fatalf("line %d holds %q, want %q", tc.line, got[tc.line-1], tc.want)
		}
	}
}

// Prose out, code in. The question is the doc comment; the haystack must be
// code bodies only, or the question is a substring of its own answer.
func TestStripDocsRemovesDocProseAndKeepsCode(t *testing.T) {
	_, out, _ := stripped(t, "docs.gotxt")
	for _, gone := range []string{
		"Package docs is the fixture",     // file doc
		"Errors is the standard one",      // import spec doc
		"ErrGone is returned",             // var decl doc
		"Kinds are the shapes",            // block comment on a const decl
		"KindA is the first shape",        // value spec doc inside the block
		"Counter counts",                  // type decl doc
		"N is the running total",          // struct field doc
		"Add adds d and returns",          // method doc
		"spans more than one line",        // its second paragraph
		"Shape is documented on the spec", // type spec doc inside a type ( … ) group
	} {
		if strings.Contains(string(out), gone) {
			t.Fatalf("doc prose %q survived stripping", gone)
		}
	}
	for _, kept := range []string{
		"package docs",
		`var ErrGone = errors.New("gone")`,
		`KindA = "a"`,
		"N int",
		"func (c *Counter) Add(d int) int {",
		"c.N += d",
		`func undocumented() error { return fmt.Errorf("no doc") }`,
		"Shape struct{ N int }",
	} {
		if !strings.Contains(string(out), kept) {
			t.Fatalf("code %q was removed", kept)
		}
	}
}

// The property the pre-pass exists to buy: the baseline arm's windows land on
// exactly the same lines over the stripped corpus as over the original, so the
// two arms differ in where the cuts fall and in nothing else.
func TestStrippedSourceWindowsToTheSameRanges(t *testing.T) {
	src, out, _ := stripped(t, "docs.gotxt")
	before, _ := Chunks("docs.go", src, winOpts(10, 2))
	after, err := Chunks("docs.go", out, winOpts(10, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("stripping changed the window count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].StartLine != after[i].StartLine || before[i].EndLine != after[i].EndLine {
			t.Fatalf("window %d moved from %d..%d to %d..%d", i,
				before[i].StartLine, before[i].EndLine, after[i].StartLine, after[i].EndLine)
		}
	}
}

// The AST arm chunks the stripped source, so it has to still parse and still
// find the same declarations ending on the same lines.
//
// The start line does move: it was the doc comment's, and a blanked comment is
// no longer a comment, so the parser starts the declaration at its keyword.
// That is the one thing blanking does not preserve, and it is harmless — the
// span still covers all of the code it did. What must not move is the end,
// because everything below a deleted comment would shift.
func TestStrippedSourceStillChunksToTheSameDeclarations(t *testing.T) {
	src, out, _ := stripped(t, "docs.gotxt")
	sl := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	before, _ := Chunks("docs.go", src, astOpts())
	after, err := Chunks("docs.go", out, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("stripping changed the declaration count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Symbol != after[i].Symbol || before[i].Kind != after[i].Kind {
			t.Fatalf("chunk %d: %s/%s -> %s/%s", i,
				before[i].Kind, before[i].Symbol, after[i].Kind, after[i].Symbol)
		}
		if before[i].EndLine != after[i].EndLine {
			t.Fatalf("%s ends at %d, was %d", after[i].Symbol, after[i].EndLine, before[i].EndLine)
		}
		if after[i].StartLine < before[i].StartLine {
			t.Fatalf("%s starts at %d, above its original %d", after[i].Symbol,
				after[i].StartLine, before[i].StartLine)
		}
		if strings.TrimSpace(sl[after[i].StartLine-1]) == "" {
			t.Fatalf("%s starts at line %d, which is blank", after[i].Symbol, after[i].StartLine)
		}
	}
	// Named, because the loop above would pass on a file with no doc comments
	// at all: this is the span the golden set's question is generated from.
	add := byName(t, after, "Counter.Add")
	if strings.Contains(add.Text, "Add adds d and returns") {
		t.Fatalf("Counter.Add still holds its doc prose:\n%s", add.Text)
	}
	if !strings.Contains(add.Text, "c.N += d") {
		t.Fatalf("Counter.Add lost its body:\n%s", add.Text)
	}
	if !strings.Contains(byName(t, before, "Counter.Add").Text, "Add adds d and returns") {
		t.Fatal("the unstripped fixture has no doc prose on Counter.Add, so this test proves nothing")
	}
}

func byName(t *testing.T, cs []Chunk, symbol string) Chunk {
	t.Helper()
	for _, c := range cs {
		if c.Symbol == symbol {
			return c
		}
	}
	t.Fatalf("no chunk for %s", symbol)
	return Chunk{}
}

// Only doc comments. A comment inside a function body is code context a reader
// of the span wants, and no golden question is generated from it.
func TestStripDocsKeepsCommentsInsideBodies(t *testing.T) {
	_, out, _ := stripped(t, "docs.gotxt")
	if !strings.Contains(string(out), "// This comment is inside the body and stays") {
		t.Fatalf("body comment was removed:\n%s", out)
	}
	src := []byte("package p\n\n// Doc prose.\nfunc F() {\n\t// inside the body\n\t_ = 1\n}\n")
	got, err := StripDocs("f.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "Doc prose") {
		t.Fatal("doc comment survived")
	}
	if !strings.Contains(string(got), "// inside the body") {
		t.Fatalf("body comment was removed:\n%s", got)
	}
}

// The package doc is prose too, and it is the largest single block of it in a
// well-documented repository. Nothing else in the walk reaches it: ast.Inspect
// visits a File's Doc as a CommentGroup, which the node switch does not match.
func TestStripDocsRemovesThePackageDoc(t *testing.T) {
	src := []byte("// Package p does a thing.\n//\n// At length.\npackage p\n\nfunc F() {}\n")
	out, err := StripDocs("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "does a thing") || strings.Contains(string(out), "At length") {
		t.Fatalf("package doc survived:\n%s", out)
	}
	if !strings.Contains(string(out), "package p") {
		t.Fatalf("package clause was removed:\n%s", out)
	}
}

// A build constraint is not prose, and removing it changes what the file means.
// //go:generate and //nolint are the same case.
//
// The directive that decides this is //go:noinline, which sits inside a doc
// comment group. A leading //go:build is separated from the package clause by a
// blank line, so it is a floating comment no Doc points at and it survives
// whatever this function does — it cannot tell a working skip from a missing one.
func TestStripDocsKeepsDirectives(t *testing.T) {
	_, out, _ := stripped(t, "docs.gotxt")
	for _, kept := range []string{"//go:build linux", "//go:noinline"} {
		if !strings.Contains(string(out), kept) {
			t.Fatalf("directive %q was stripped:\n%s", kept, out)
		}
	}
	if strings.Contains(string(out), "The second paragraph exists") {
		t.Fatalf("doc prose survived alongside the directive:\n%s", out)
	}
}

// One row per decision isDirective makes. Without them four of its branches
// were decided only by the other half of the rule and could be deleted with
// every test still passing: "// TODO: explain this." is refused twice over —
// by the name loop and by the character after the colon — so it discriminates
// neither. Each row below is refused, or kept, by exactly one check.
func TestStripDocsTellsDirectivesFromProseShapedLikeThem(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		kept bool
	}{
		{"//go:generate stringer -type=K", true},
		{"// TODO: explain this.", false},      // space in the name, and after the colon
		{"// todo:fix this", false},            // the name segment holds a space
		{"//todo: explain this.", false},       // lowercase name, but no value after the colon
		{"//go:", false},                       // a name with no value; the guard refusing it also keeps c[colon+1] in range
		{"//:x", false},                        // and a value with no name
		{"/* Kinds are the shapes. */", false}, // only // comments can be directives
	} {
		src := []byte("package p\n\n" + tc.doc + "\nfunc F() {}\n")
		out, err := StripDocs("p.go", src)
		if err != nil {
			t.Fatalf("%s: %v", tc.doc, err)
		}
		if got := strings.Contains(string(out), tc.doc); got != tc.kept {
			t.Errorf("%q: kept=%v, want %v:\n%s", tc.doc, got, tc.kept, out)
		}
	}
}

// A file that will not parse has no Go doc comments to find. Returning an error
// rather than the source unchanged is the conservative call: the alternative is
// a benchmark corpus that silently holds the prose its questions were generated
// from, and nothing about the run would look wrong.
func TestStripDocsRefusesWhatItCannotParse(t *testing.T) {
	src, _ := fixture(t, "broken.gotxt")
	if _, err := StripDocs("broken.go", src); err == nil {
		t.Fatal("unparseable Go was stripped without error")
	}
}

// Not Go: nothing to strip, and no error. Markdown in the corpus is windowed as
// prose and carries no doc comment a golden question comes from.
func TestStripDocsPassesNonGoThrough(t *testing.T) {
	md := []byte("# Title\n\n// not a doc comment\n")
	out, err := StripDocs("README.md", md)
	if err != nil || string(out) != string(md) {
		t.Fatalf("markdown was altered: %q, %v", out, err)
	}
	// Markdown alone does not test the extension check — it fails to parse, so
	// dropping the check turns this into an error rather than a changed file.
	// What the check decides is a non-Go file whose bytes are valid Go: a
	// .gotxt fixture, a vendored .go.orig. Those must come back untouched.
	src, _ := fixture(t, "docs.gotxt")
	out, err = StripDocs("docs.gotxt", src)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(src) {
		t.Fatal("valid Go under a non-.go name was stripped")
	}
}

// //line directives retarget go/token's positions at the generator's source,
// including past the end of this file, and Task 2 found that hazard live: the
// AST chunker needs PositionFor(pos, false) or a span lands in another file.
//
// It does not reach here. Measured: swapping tf.Offset for fset.Position's
// Offset leaves every test in this file passing, because a directive moves the
// line a position reports and never the byte it sits at. What this test does
// pin is that a directive numbered past the end of the file is prose-free and
// survives, rather than being blanked or panicking on a bad offset.
func TestStripDocsSurvivesLineDirectives(t *testing.T) {
	src := []byte("package p\n\n//line sample.y:3000\n// F does a thing.\nfunc F() {\n\t_ = 1\n}\n")
	out, err := StripDocs("gen.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "F does a thing") {
		t.Fatalf("doc prose survived:\n%s", out)
	}
	if !strings.Contains(string(out), "//line sample.y:3000") {
		t.Fatalf("//line directive was stripped:\n%s", out)
	}
	if n := strings.Count(string(out), "\n"); n != strings.Count(string(src), "\n") {
		t.Fatalf("stripped source has %d newlines, original has %d", n, strings.Count(string(src), "\n"))
	}
}

// Windows line endings, the one shape in this package that produced wrong
// output. ast.Comment.End() is Slash+len(c.Text) and go/scanner strips carriage
// returns out of c.Text, so a CRLF block doc comment's End() lands two bytes
// short and the closing "*/" survives into source that no longer parses.
//
// Nothing here would have said so. StripDocs returned no error, and the AST
// arm's answer to source it cannot parse is windows — so the corpus quietly
// lost the declaration and the run still looked healthy. That is why this test
// chunks the stripped bytes rather than only inspecting them.
func TestStripDocsBlanksDocCommentsInCRLFSource(t *testing.T) {
	src := []byte("package p\r\n\r\n/*\r\nDoc for F.\r\nMore prose.\r\n*/\r\nfunc F() {\r\n\t_ = 1\r\n}\r\n")
	out, err := StripDocs("crlf.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Doc for F") || strings.Contains(string(out), "*/") {
		t.Fatalf("the block doc comment did not go away whole:\n%q", out)
	}
	if got, want := strings.Count(string(out), "\n"), strings.Count(string(src), "\n"); got != want {
		t.Fatalf("stripped source has %d newlines, original has %d", got, want)
	}
	// The terminator is both bytes: keeping only the \n rewrites the line
	// endings of every line inside the comment.
	if strings.Contains(strings.ReplaceAll(string(out), "\r\n", ""), "\n") {
		t.Fatalf("a CRLF line ending became a bare LF:\n%q", out)
	}
	got, err := Chunks("crlf.go", out, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != models.KindFunc || got[0].Symbol != "F" {
		for _, c := range got {
			t.Logf("%s %s %d..%d", c.Kind, c.Symbol, c.StartLine, c.EndLine)
		}
		t.Fatalf("stripped CRLF source no longer reaches the AST arm:\n%q", out)
	}
}

// A bare \r inside a // comment is the same defect one byte at a time: c.Text
// drops it, so End() stops short and the comment's last character is left
// behind as a statement. The remnant is a single letter, which no assertion on
// the prose would notice — only re-chunking does.
func TestStripDocsBlanksDocCommentsHoldingABareCarriageReturn(t *testing.T) {
	src := []byte("package p\n\n// a\rb prose\nfunc G() {\n\t_ = 1\n}\n")
	out, err := StripDocs("cr.go", src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Chunks("cr.go", out, astOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != models.KindFunc || got[0].Symbol != "G" {
		t.Fatalf("stripped source no longer reaches the AST arm: %q", out)
	}
}

// The guard that makes this class of bug loud instead of silent. Nothing in the
// package can produce a broken output any more, so selfCheck is called with one
// directly: a line short, and a file that no longer parses.
func TestStripDocsRefusesOutputThatBrokeAnInvariant(t *testing.T) {
	src := []byte("package p\n\n// Doc.\nfunc F() {}\n")
	if err := selfCheck("p.go", src, []byte("package p\n\nfunc F() {}\n")); err == nil {
		t.Fatal("a stripped source one line short was accepted")
	}
	if err := selfCheck("p.go", src, []byte("package p\n\n*/\nfunc F() {}\n")); err == nil {
		t.Fatal("a stripped source that no longer parses was accepted")
	}
	if err := selfCheck("p.go", src, src); err != nil {
		t.Fatalf("source that broke nothing was refused: %v", err)
	}
}

// The offsets the blanking loop runs over, stated as numbers. End-to-end this
// defect reaches a test as selfCheck's error rather than as an assertion, so
// what the error means is pinned here.
func TestCommentEndCoversTheWholeRawComment(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"block", "/*\nDoc.\n*/\nfunc F() {}\n", 10},
		{"block CRLF", "/*\r\nDoc.\r\n*/\r\nfunc F() {}\r\n", 12},
		{"line", "// Doc.\nfunc F() {}\n", 7},
		{"line CRLF", "// Doc.\r\nfunc F() {}\r\n", 8},
		{"line holding a bare CR", "// a\rb\nfunc F() {}\n", 6},
		{"line at end of file", "// Doc.", 7},
	} {
		if got := commentEnd([]byte(tc.src), 0); got != tc.want {
			t.Errorf("%s: commentEnd is %d, want %d — %q", tc.name, got, tc.want, tc.src[:min(got, len(tc.src))])
		}
	}
}
