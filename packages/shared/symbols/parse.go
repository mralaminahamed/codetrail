package symbols

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
)

// Parse reads path's definitions and call sites out of src. Nothing is read
// from disk; path is what the parser names in an error and what every Def and
// Call carries.
//
// It returns an error only when the file does not parse, and then an empty
// File: files that do not parse are normal in a stranger's repository, chunk
// windows them rather than failing, and a graph pass stricter than the chunker
// would fail a job over one bad file. The partial AST go/parser recovers goes
// with it — its ranges are the parser's guesses about code that is not there.
//
// SkipObjectResolution, as chunk uses: ast.Object links are what would tempt a
// reader into believing this pass can bind a name to a declaration. It cannot.
func Parse(path string, src []byte) (File, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return File{}, fmt.Errorf("symbols: parse %s: %w", path, err)
	}
	return walk(fset, f, path), nil
}

func walk(fset *token.FileSet, f *ast.File, path string) File {
	// Pkg is the package clause, not the import path: the import path needs a
	// module-aware load, which is the thing spec §6 allows to fail.
	out := File{Pkg: f.Name.Name}
	for _, d := range f.Decls {
		kind, name, start, end, ok := chunk.Decl(fset, d)
		if !ok {
			continue
		}
		out.Defs = append(out.Defs, Def{
			Kind: kind, Name: name, Pkg: out.Pkg, Path: path,
			StartLine: start, EndLine: end,
		})
		// Per declaration rather than file-wide: an edge's tail is the
		// definition the call is in, and a wrong tail is a wrong answer that
		// looks exactly like a right one. Nothing is orphaned — there is no
		// code outside a declaration in Go.
		ast.Inspect(d, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id := calleeIdent(c.Fun)
			if id == nil {
				out.Unnameable++
				return true
			}
			// PositionFor(…, false): //line directives retarget positions at
			// a generator's source, and Task 3 keys resolutions by an offset
			// into the file that was indexed.
			pos := fset.PositionFor(id.Pos(), false)
			out.Calls = append(out.Calls, Call{
				Path: path, Name: id.Name,
				Offset: pos.Offset, Line: pos.Line, FromStart: start,
			})
			return true
		})
	}
	return out
}

// calleeIdent is the identifier a call names, or nil when it names none.
//
// An IndexExpr is deliberately not unwrapped: without type information
// Map[int]() and fns[i]() are the same syntax, so naming the generic
// instantiation would also name a call through a slice element after the
// variable that holds it. Both are counted instead.
func calleeIdent(e ast.Expr) *ast.Ident {
	switch e := e.(type) {
	case *ast.Ident:
		return e
	case *ast.SelectorExpr:
		return e.Sel
	}
	return nil
}
