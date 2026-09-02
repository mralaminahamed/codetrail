package symbols

import (
	"context"
	"errors"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"

	"golang.org/x/tools/go/packages"
)

// Why a resolution stopped where it did. Spec:196 makes a type-check failure
// an outcome rather than an error, so this is the whole of what Resolve
// reports about going wrong, and it is a closed set a counter can be keyed on.
const (
	ReasonOK          = "ok"
	ReasonDisabled    = "disabled" // set by the caller's kill switch, never here
	ReasonNoToolchain = "no_toolchain"
	ReasonNoModule    = "no_module"
	ReasonLoadError   = "load_error"
	ReasonDeadline    = "deadline"
	ReasonPolicy      = "policy"
)

// Key is one call site, spelled exactly as Parse spells it: a repo-relative
// path and the byte offset of the callee's identifier.
//
// Both passes must therefore read the same bytes. chunk.StripDocs removes the
// prose and keeps only its line terminators, so a caller that hands Parse
// stripped source and lets go/packages read the file from disk is keying two
// different coordinate systems against each other, and every lookup misses
// with no error anywhere.
type Key struct {
	Path   string
	Offset int
}

// Target is where a resolved call's definition is, repo-relative.
//
// Line is the declaration's own line, not the first line of its doc comment —
// a Def's StartLine begins at the comment — so a caller matching this to a
// definition must match by containment, not by equality with StartLine.
type Target struct {
	Path string
	Line int
}

// Stats is what one Resolve managed. Resolved+External+Unresolved is every
// call site in the packages that loaded; the calls in a package that did not
// load are not counted here at all, because the loader never saw them.
type Stats struct {
	Packages   int
	Loaded     int
	Failed     int
	Resolved   int
	External   int
	Unresolved int
	Reason     string
}

// NeedDeps is present, and it is the opposite of what it looks like: without
// it, usesExportData is true and go/packages runs "go list -export=true",
// which *compiles* every dependency. Measured on the std fixture: 2.66s and
// 31.8MB of build cache without NeedDeps, 0.38s and 0.69MB with it; on a
// package importing net/http, 5.9s and 92.4MB against 0.69s and 1.0MB. With
// the cache scoped to one job, none of that is ever reused.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
	packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps

// Resolve type-checks the checkout under p and reports which call sites it
// could name.
//
// It has no error return on purpose. Spec:196 makes a failed type-check an
// expected outcome that downgrades a repository's edges to syntactic; an error
// in this signature would be an invitation to propagate it into the job's
// failure path, and the compiler would not object.
//
// The context is the job's own. Spec:194 asks for one deadline for the whole
// job, so nothing here derives a second budget from it — not even a smaller
// one, which is defensible and still a second budget.
func Resolve(ctx context.Context, p Policy) (map[Key]Target, Stats) {
	if err := p.Validate(); err != nil {
		if errors.Is(err, ErrNoToolchain) {
			return nil, Stats{Reason: ReasonNoToolchain}
		}
		return nil, Stats{Reason: ReasonPolicy}
	}
	if ctx.Err() != nil {
		return nil, Stats{Reason: ReasonDeadline}
	}
	// A repository with no go.mod at its root is the shape most single-file
	// repositories have, and the go command's answer to it is an error about a
	// pattern rather than about the repository. Answering it here keeps a
	// subprocess from starting at all.
	if _, err := os.Stat(filepath.Join(p.Root, "go.mod")); err != nil {
		return nil, Stats{Reason: ReasonNoModule}
	}
	for _, d := range p.dirs() {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, Stats{Reason: ReasonPolicy}
		}
	}

	cfg := &packages.Config{
		Mode: loadMode,
		Dir:  p.Root,
		// Non-nil Env is what makes go/packages run the child with this
		// environment *instead of* the parent's rather than appended to it.
		Env:     p.Env(),
		Context: ctx,
		Tests:   false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		if ctx.Err() != nil {
			return nil, Stats{Reason: ReasonDeadline}
		}
		return nil, Stats{Reason: ReasonLoadError}
	}

	out, st := collect(p.Root, pkgs)
	switch {
	case ctx.Err() != nil:
		st.Reason = ReasonDeadline
	case st.Packages == 0 || st.Failed > 0:
		st.Reason = ReasonLoadError
	default:
		st.Reason = ReasonOK
	}
	return out, st
}

// collect walks every call site the loader could see. There is deliberately no
// branch on whether a package loaded: a package that type-checks with errors
// still resolves most of its identifiers, and the ones types.Info.Uses has no
// object for stay unresolved inside a package that loaded (spec:190).
func collect(root string, pkgs []*packages.Package) (map[Key]Target, Stats) {
	out := make(map[Key]Target)
	var st Stats
	for _, pkg := range pkgs {
		st.Packages++
		if pkg.TypesInfo == nil || len(pkg.Errors) > 0 {
			st.Failed++
		} else {
			st.Loaded++
		}
		if pkg.TypesInfo == nil {
			continue
		}
		for _, f := range pkg.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// calleeIdent is Parse's, not a second copy: the key this
				// returns has to be the key Parse wrote.
				id := calleeIdent(c.Fun)
				if id == nil {
					return true
				}
				pos := pkg.Fset.PositionFor(id.Pos(), false)
				key, ok := rel(root, pos.Filename)
				if !ok {
					return true
				}
				t, where := lookup(root, pkg, id)
				switch where {
				case inside:
					out[Key{Path: key, Offset: pos.Offset}] = t
					st.Resolved++
				case outside:
					st.External++
				default:
					st.Unresolved++
				}
				return true
			})
		}
	}
	return out, st
}

type where int

const (
	unknown where = iota
	inside
	outside
)

// lookup asks types.Info.Uses what the callee names, and refuses two answers
// that would be worse than none.
//
// Anything that is not a package-scoped func or a method is unknown: Uses
// resolves fn() where fn is a local variable holding a closure to the
// *variable*, whose position is inside the enclosing function, which would
// record an edge from a function to itself that is not a self-call.
//
// A func outside the checkout is outside, not a target: fmt.Println names a
// real object and has no symbols row, and §3 says a resolved edge points at
// one while spec:84 says a null target is what makes the label mean anything.
// Both cannot hold, so the invariant wins and the fact is kept in a counter.
func lookup(root string, pkg *packages.Package, id *ast.Ident) (Target, where) {
	fn, ok := pkg.TypesInfo.Uses[id].(*types.Func)
	if !ok {
		return Target{}, unknown
	}
	if fn.Signature().Recv() == nil && (fn.Pkg() == nil || fn.Parent() != fn.Pkg().Scope()) {
		return Target{}, unknown
	}
	pos := pkg.Fset.PositionFor(fn.Pos(), false)
	if !pos.IsValid() {
		return Target{}, outside
	}
	path, ok := rel(root, pos.Filename)
	if !ok {
		return Target{}, outside
	}
	return Target{Path: path, Line: pos.Line}, inside
}

// rel spells a path the way spans.path does. No symlink resolution: the go
// command reports positions under the working directory it was given, which
// go/packages pins with PWD, so a symlinked checkout root comes back under the
// symlink.
func rel(root, name string) (string, bool) {
	r, err := filepath.Rel(root, name)
	if err != nil || !filepath.IsLocal(r) {
		return "", false
	}
	return filepath.ToSlash(r), true
}
