package chunk

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// StripDocs blanks every doc comment in src, keeping its newlines so no line
// number moves.
//
// It exists for the eval (spec §9). The golden set's questions are doc-comment
// prose and its answers are the spans those comments document, so a corpus that
// keeps the comments makes every question a literal substring of its own answer
// and both arms score on string overlap alone. The production index keeps them,
// because there they are the best signal available.
//
// A pre-pass rather than a chunker mode: applied before Chunks, both eval arms
// chunk byte-identical input by construction, and StrategyWindow still never
// parses (spec §5, which asks for both and cannot have them any other way).
//
// Blanked, not deleted. Measured on testdata/docs.gotxt: deleting the lines
// instead moves Counter.Add from 37..46 to 22..28 and turns six windows into
// four, so the two arms would tile different lines and every citation into the
// stripped corpus would name code that is not there.
func StripDocs(filename string, src []byte) ([]byte, error) {
	if !strings.HasSuffix(filename, ".go") {
		return src, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		// Refused rather than passed through: the alternative is an eval corpus
		// that silently holds the prose its questions came from, and nothing
		// about the run would look wrong.
		return nil, fmt.Errorf("chunk: strip %s: %w", filename, err)
	}
	tf := fset.File(f.Pos())

	// drop[i] is true for a byte of doc prose. A mask rather than a list of
	// ranges so the traversal below can visit them in any order.
	drop := make([]bool, len(src))
	add := func(g *ast.CommentGroup) {
		if g == nil {
			return
		}
		for _, c := range g.List {
			if isDirective(c.Text) {
				continue
			}
			for i := tf.Offset(c.Pos()); i < tf.Offset(c.End()); i++ {
				drop[i] = true
			}
		}
	}
	// f.Doc is not reached by Inspect's switch below, and it is the largest
	// single block of prose in a well-documented file.
	add(f.Doc)
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			add(d.Doc)
		case *ast.GenDecl:
			add(d.Doc)
		case *ast.TypeSpec:
			add(d.Doc)
		case *ast.ValueSpec:
			add(d.Doc)
		case *ast.ImportSpec:
			add(d.Doc)
		case *ast.Field:
			add(d.Doc)
		}
		return true
	})

	out := make([]byte, 0, len(src))
	for i, b := range src {
		// The newline is what is kept: it is the whole of "blanked, not
		// deleted", and only a /* */ doc comment has any inside it.
		if drop[i] && b != '\n' {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// isDirective reports whether a comment is a directive rather than prose.
// Blanking one changes what the file means: //go:build decides whether it
// compiles at all, and //line decides what the rest of it claims to be.
//
// Hand-rolled because go/ast keeps its own copy unexported: at Go 1.27.0,
// `go doc go/ast.IsDirective` answers "no symbol IsDirective in package go/ast".
// The rule is go/ast's — the three grandfathered words, then //name:value with
// name lowercase alphanumeric, which is what excludes prose like "// TODO: x".
func isDirective(text string) bool {
	c, ok := strings.CutPrefix(text, "//")
	if !ok {
		return false
	}
	for _, word := range []string{"line ", "extern ", "export "} {
		if strings.HasPrefix(c, word) {
			return true
		}
	}
	colon := strings.Index(c, ":")
	if colon <= 0 || colon+1 >= len(c) {
		return false
	}
	for i := 0; i < colon; i++ {
		if !isLowerAlnum(c[i]) {
			return false
		}
	}
	return isLowerAlnum(c[colon+1])
}

func isLowerAlnum(b byte) bool {
	return 'a' <= b && b <= 'z' || '0' <= b && b <= '9'
}
