package golden

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// Skip reasons, which are Stats.SkipReasons keys and therefore a closed set.
const (
	SkipNotGo        = "not_go"
	SkipUnparsed     = "unparsed"
	SkipUnstrippable = "unstrippable"
)

// Generate derives one file's cases.
//
// It parses twice. The raw parse supplies the question, RawStart and RawEnd;
// the stripped parse supplies Start and End, which are the coordinates the
// corpus is in. The two are matched by position in f.Decls, because stripping
// cannot add, remove or reorder a declaration — matching by symbol would look
// safer and is not, since it fails silently the day two declarations in one
// file share a spelling and hides the disagreement the ordinal check exists to
// surface.
//
// A file that will not parse or will not strip produces no cases and is
// counted with its reason. It produces no spans either — the indexer continues
// past it — so the two agree by construction, but "this repository
// contributed no cases" and "this repository has no documented exported
// symbols" are different facts.
func Generate(path string, src []byte) ([]Case, Stats, error) {
	st := Stats{SkipReasons: map[string]int{}}
	if !strings.HasSuffix(path, ".go") {
		st.FilesSkipped, st.SkipReasons[SkipNotGo] = 1, 1
		return nil, st, nil
	}
	fset := token.NewFileSet()
	raw, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		st.FilesSkipped, st.SkipReasons[SkipUnparsed] = 1, 1
		return nil, st, nil
	}
	strippedSrc, err := chunk.StripDocs(path, src)
	if err != nil {
		st.FilesSkipped, st.SkipReasons[SkipUnstrippable] = 1, 1
		return nil, st, nil
	}
	sset := token.NewFileSet()
	stripped, err := parser.ParseFile(sset, path, strippedSrc, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		// StripDocs re-parses its own output before returning, so this is
		// unreachable through that path; it is here because "the stripped file
		// no longer parses" must never become a case set keyed to nothing.
		return nil, st, fmt.Errorf("golden: %s: stripped source does not parse: %w", path, err)
	}

	st.Files = 1
	paired, err := pair(path, fset, raw, sset, stripped)
	if err != nil {
		return nil, st, err
	}

	var cases []Case
	for _, p := range paired {
		rd := p.raw
		st.Decls++
		st.FieldDocs += fieldDocs(rd)
		if !p.ok {
			if g, ok := rd.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
				st.Imports++
			} else {
				st.Unchunkable++
			}
			continue
		}

		base := Case{
			Path: path, Kind: p.kind,
			Start: p.start, End: p.end, RawStart: p.rawStart, RawEnd: p.rawEnd,
			Grouped: grouped(rd),
		}
		switch doc := declDoc(rd); {
		case doc == nil:
			st.NoDoc++
		case !Exported(p.symbol):
			st.Unexported++
		default:
			cases = append(cases, with(base, p.symbol, doc.Text(), DocOfDecl))
		}

		// A grouped block's specs each carry their own prose and all point at
		// the one span the block became. Ignoring them would drop every
		// documented constant in a const ( … ) block, which on an API-heavy
		// repository is a large and silent subset.
		g, isGen := rd.(*ast.GenDecl)
		if !isGen {
			continue
		}
		for _, s := range g.Specs {
			name, doc := specDoc(s)
			if doc == nil {
				continue
			}
			st.SpecDocs++
			if !Exported(name) {
				st.UnexportedSpec++
				continue
			}
			cases = append(cases, with(base, name, doc.Text(), DocOfSpec))
		}
	}

	st.Cases = len(cases)
	for _, c := range cases {
		if c.Grouped {
			st.Grouped++
		}
	}
	st.DuplicateQuestions = Duplicates(cases)
	return cases, st, nil
}

// matched is one declaration seen from both parses: the raw node the question
// comes from, and the two ranges.
type matched struct {
	raw              ast.Decl
	ok               bool
	kind             models.SpanKind
	symbol           string
	start, end       int
	rawStart, rawEnd int
}

// pair walks two parses in lockstep and refuses a disagreement.
//
// Position, not symbol: stripping cannot add, remove or reorder a declaration,
// while a symbol is not guaranteed unique in a file forever, and a symbol match
// fails silently on the day two declarations share a spelling. The check is
// what converts a future disagreement into a refusal rather than a corpus
// keyed to lines it does not have.
func pair(path string, rf *token.FileSet, raw *ast.File, sf *token.FileSet, stripped *ast.File) ([]matched, error) {
	if len(raw.Decls) != len(stripped.Decls) {
		return nil, fmt.Errorf("%w: %s has %d declarations raw and %d stripped",
			ErrDeclMismatch, path, len(raw.Decls), len(stripped.Decls))
	}
	out := make([]matched, 0, len(raw.Decls))
	for i, rd := range raw.Decls {
		rKind, rSym, rStart, rEnd, rOK := chunk.Decl(rf, rd)
		sKind, sSym, sStart, sEnd, sOK := chunk.Decl(sf, stripped.Decls[i])
		if rOK != sOK || rKind != sKind || rSym != sSym {
			return nil, fmt.Errorf("%w: %s declaration %d is (%v %s %q) raw and (%v %s %q) stripped",
				ErrDeclMismatch, path, i, rOK, rKind, rSym, sOK, sKind, sSym)
		}
		out = append(out, matched{
			raw: rd, ok: rOK, kind: rKind, symbol: rSym,
			start: sStart, end: sEnd, rawStart: rStart, rawEnd: rEnd,
		})
	}
	return out, nil
}

// with fills in the per-case fields the declaration does not decide. The id
// carries docOf so a grouped block's two cases are distinguishable and the
// artefact is diffable across runs.
func with(c Case, symbol, question, docOf string) Case {
	c.Symbol, c.Question, c.DocOf = symbol, question, docOf
	c.ID = c.Path + ":" + symbol + ":" + docOf
	return c
}

// grouped is a parenthesised block, which is the only shape that can carry
// both a declaration doc and per-spec docs. `var X = 1` is one name and one
// comment, and calling it grouped would make the reported share meaningless.
func grouped(d ast.Decl) bool {
	g, ok := d.(*ast.GenDecl)
	return ok && g.Lparen.IsValid()
}

func declDoc(d ast.Decl) *ast.CommentGroup {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return d.Doc
	case *ast.GenDecl:
		return d.Doc
	}
	return nil
}

func specDoc(s ast.Spec) (string, *ast.CommentGroup) {
	switch s := s.(type) {
	case *ast.TypeSpec:
		if s.Name == nil {
			return "", nil
		}
		return s.Name.Name, s.Doc
	case *ast.ValueSpec:
		if len(s.Names) == 0 {
			return "", nil
		}
		return s.Names[0].Name, s.Doc
	}
	return "", nil
}

// fieldDocs counts the struct and interface field doc comments under one
// declaration. Their prose leaves the corpus with everything else StripDocs
// blanks, and no span answers them.
func fieldDocs(d ast.Decl) int {
	n := 0
	ast.Inspect(d, func(node ast.Node) bool {
		if f, ok := node.(*ast.Field); ok && f.Doc != nil {
			n++
		}
		return true
	})
	return n
}

// Kinds the generator can emit, exported so a test can pin the set against
// models.SpanKinds. KindFile is absent: a window is not a declaration.
var Kinds = []models.SpanKind{models.KindFunc, models.KindType, models.KindConst, models.KindVar}
