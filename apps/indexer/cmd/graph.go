package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
	"github.com/mralaminahamed/codetrail/packages/shared/symbols"
)

// graphJob is what the graph stage needs from the rest of the job.
type graphJob struct {
	repoID string
	// root is the checkout the type-checker reads. home is where the go
	// command's caches go: beside the checkout rather than inside it, because
	// ./... would otherwise walk them and a fetched module carries a go.mod of
	// its own.
	root, home string
	parsed     []symbols.File
	files      []models.File
	spans      []store.EmbeddedSpan
}

// graphRows is one repository's graph plus what making it cost.
type graphRows struct {
	symbols    []models.Symbol
	edges      []models.Edge
	resolved   int
	syntactic  int
	unnameable int
	stats      symbols.Stats
}

// runGraph builds this repository's graph, says what it produced, and writes
// it.
//
// Two contexts, and the split is the whole of spec §6 in one signature. The
// type-check runs on jobCtx, the budget the clone and the chunker have already
// been spending — one deadline for the whole job, so a slow clone cannot buy
// itself extra time by failing into the type-check. The write runs on
// writeCtx, which is derived from the process, because a write on the budget
// the type-check just spent is the type-check's failure failing the job, and
// spec:196 makes that failure an outcome instead. What can still fail a job
// here is a database that is not answering.
func (ix *indexer) runGraph(jobCtx, writeCtx context.Context, l zerolog.Logger, j graphJob) error {
	g := ix.buildGraph(jobCtx, j)

	// One line per job. warn only when something was lost, so an operator
	// grepping for the downgrade finds it: reason is the only place the
	// difference between "this code has no in-repo calls" and "this worker has
	// no toolchain" is written down.
	ev := l.Info()
	if g.stats.Reason != symbols.ReasonOK {
		ev = l.Warn()
	}
	ev.Int("symbols", len(g.symbols)).Int("edges", len(g.edges)).
		Int("resolved", g.resolved).Int("syntactic", g.syntactic).
		Int("external", g.stats.External).Int("unnameable", g.unnameable).
		// The per-package split, which reason cannot carry: load_error is one
		// word for "none of them loaded" and for "one of nine did not", and
		// those are different repositories to an operator deciding whether to
		// open the proxy.
		Int("packages", g.stats.Packages).Int("loaded", g.stats.Loaded).
		Int("failed", g.stats.Failed).
		Str("reason", g.stats.Reason).Msg("symbol graph")

	for _, e := range g.edges {
		metrics.CountGraph(string(e.Provenance))
	}
	metrics.CountTypecheck(g.stats.Reason)

	return ix.putGraph(writeCtx, j.repoID, g.symbols, g.edges)
}

// buildGraph makes every call site an edge, then lets type information upgrade
// the rows it can name.
//
// The edge set is the AST's and resolution never changes its size (spec §6):
// a call the type-checker cannot name keeps the null target that makes the
// syntactic label mean something (spec:84).
func (ix *indexer) buildGraph(ctx context.Context, j graphJob) graphRows {
	var g graphRows

	fileIDs := make(map[string]string, len(j.files))
	for _, f := range j.files {
		fileIDs[f.Path] = f.ID
	}
	spans := make(map[string][]container, len(j.files))
	for _, sp := range j.spans {
		spans[sp.Path] = append(spans[sp.Path], container{sp.ID, sp.StartLine, sp.EndLine})
	}

	defs := make(map[string][]container)
	// from is the tail of every edge: the declaration a call is inside, which
	// Parse records as the declaration's first line. First one wins, because
	// `type A int; type B int` on one line is legal and one of the two has to
	// own a call written inside it.
	from := make(map[string]map[int]string)
	for _, f := range j.parsed {
		for _, d := range f.Defs {
			fileID, ok := fileIDs[d.Path]
			if !ok {
				// index appends a row for every file it reads, so this is a
				// guard and not a case: a definition whose file has no row
				// fails the foreign key and takes the whole job with it.
				continue
			}
			id := store.SymbolID(j.repoID, d.Path, d.StartLine, string(d.Kind), d.Name)
			spanID, _ := spanFor(spans[d.Path], d.StartLine, d.EndLine)
			g.symbols = append(g.symbols, models.Symbol{
				ID: id, RepoID: j.repoID, FileID: fileID, Path: d.Path,
				Name: d.Name, Pkg: d.Pkg, Kind: d.Kind,
				StartLine: d.StartLine, EndLine: d.EndLine, SpanID: spanID,
			})
			defs[d.Path] = append(defs[d.Path], container{id, d.StartLine, d.EndLine})
			if from[d.Path] == nil {
				from[d.Path] = make(map[int]string)
			}
			if _, seen := from[d.Path][d.StartLine]; !seen {
				from[d.Path][d.StartLine] = id
			}
		}
		g.unnameable += f.Unnameable
	}

	resolved, st := ix.resolveCalls(ctx, j)
	g.stats = st

	for _, f := range j.parsed {
		for _, c := range f.Calls {
			fromID, ok := from[c.Path][c.FromStart]
			if !ok {
				continue
			}
			e := models.Edge{
				ID:     store.EdgeID(j.repoID, fromID, c.Path, c.Offset, c.Name),
				RepoID: j.repoID, FromSymbolID: fromID, ToName: c.Name,
				Kind: models.EdgeCalls, Provenance: models.ProvenanceSyntactic,
				Path: c.Path, Line: c.Line,
			}
			// One lookup per call site, and nothing here reads Stats. Spec:190
			// makes provenance a property of the row; a branch on whether a
			// package loaded is exactly how it stops being one, and it is
			// invisible in any repository with a single package.
			if t, ok := resolved[symbols.Key{Path: c.Path, Offset: c.Offset}]; ok {
				// By containment again, for a different reason: a Target is
				// the declaration's own line and a Def's range starts at its
				// doc comment, so the two are equal only for an undocumented
				// declaration.
				if id, ok := mostSpecific(defs[t.Path], t.Line); ok {
					e.ToSymbolID, e.Provenance = id, models.ProvenanceResolved
				}
			}
			if e.Provenance == models.ProvenanceResolved {
				g.resolved++
			} else {
				g.syntactic++
			}
			g.edges = append(g.edges, e)
		}
	}
	return g
}

// resolveCalls runs the type-checker, or says why it did not.
//
// The kill switch is checked here rather than around the whole stage: a
// repository still gets its definitions and its syntactic edges with
// TYPECHECK=false, which is what makes the switch an honest answer to an
// operator who cannot ship a Go toolchain rather than a way to turn the graph
// off.
func (ix *indexer) resolveCalls(ctx context.Context, j graphJob) (map[symbols.Key]symbols.Target, symbols.Stats) {
	if !ix.lim.typecheck {
		return nil, symbols.Stats{Reason: symbols.ReasonDisabled}
	}
	start := time.Now()
	out, st := ix.graph(ctx, symbols.Policy{
		Root: j.root, Home: j.home, GoBin: ix.lim.goBin, Proxy: ix.lim.goProxy,
	})
	metrics.ObserveTypecheck(time.Since(start))
	return out, st
}

// container is a range something can be inside: a span, or a definition.
type container struct {
	id         string
	start, end int
}

// spanFor is the span a definition is cited by: the one holding its first
// line, or failing that the first one its range overlaps.
//
// Containment first, and never an exact range match: the chunker sub-windows a
// declaration longer than MaxDeclLines, so the biggest declarations have no
// span with their range and an exact match would leave exactly them uncitable.
//
// The overlap is not a widening for its own sake — it is what makes the link
// survive STRIP_DOC_COMMENTS. index parses the RAW body and chunks the
// STRIPPED one (main.go), deliberately, because go/packages keys its offsets
// against the file on disk. StripDocs blanks a doc comment into empty lines,
// an empty line is not an *ast.CommentGroup, so chunk.Decl no longer sees d.Doc
// and starts the span at the `func` keyword — while the definition's own range
// still starts at the comment, one or more lines ABOVE every span in the file.
// Start-line containment then misses, and the miss is silent: the symbol is
// written with a NULL span_id and the answering loop hands the model a
// span_id it cannot read (agent/tools.go).
//
// Measured over this repository's own 156 Go files, 2,092 definitions:
//
//	strategy=ast     span_id NULL: unstripped 0 (0.0%)   stripped 1339 (64.0%)
//	strategy=window  span_id NULL: unstripped 0 (0.0%)   stripped 0    (0.0%)
//
// Production keeps its doc comments, so this is not a production number — but
// BOTH eval corpora are built stripped (the Makefile's eval-corpus target), and
// the asymmetry above falls on exactly the two arms spec §9 exists to compare.
//
// The start line is still tried first, so no link that already resolved moves:
// under CHUNK_STRATEGY=window a definition's first line is inside two windows
// and mostSpecific's tie-break is what picks between them.
func spanFor(cs []container, start, end int) (string, bool) {
	if id, ok := mostSpecific(cs, start); ok {
		return id, true
	}
	return firstOverlapping(cs, start, end)
}

// firstOverlapping is the container intersecting [start,end] that begins
// earliest — the one a start-line lookup would have found had the definition's
// first line not moved out from under the spans.
//
// Earliest and not most specific, because a declaration over MaxDeclLines is
// several windows and the head window is the one its first line would have
// picked. Top-level declarations do not overlap each other, so the only spans
// that can intersect a definition's range are its own. Ties are broken the way
// mostSpecific breaks them, on the end and then on the id, so the answer does
// not depend on the order rows came back in.
func firstOverlapping(cs []container, start, end int) (string, bool) {
	var best container
	found := false
	for _, c := range cs {
		if c.end < start || c.start > end {
			continue
		}
		switch {
		case !found, c.start < best.start,
			c.start == best.start && c.end < best.end,
			c.start == best.start && c.end == best.end && c.id < best.id:
			best, found = c, true
		}
	}
	return best.id, found
}

// mostSpecific is the container holding line — the greatest start, then the
// smallest end, then the smallest id.
//
// The tie-break is not decoration. Under CHUNK_STRATEGY=window the spans
// overlap by WindowOverlap lines, so a definition's first line is inside two
// of them, and "the first one found" would make a symbol's span depend on the
// order rows came back in. Under the AST strategy the declaration's own span
// is the only container and the rule is a no-op, which is why only a
// window-shaped fixture can see it at all.
func mostSpecific(cs []container, line int) (string, bool) {
	var best container
	found := false
	for _, c := range cs {
		if line < c.start || line > c.end {
			continue
		}
		switch {
		case !found, c.start > best.start,
			c.start == best.start && c.end < best.end,
			c.start == best.start && c.end == best.end && c.id < best.id:
			best, found = c, true
		}
	}
	return best.id, found
}

// logToolchain says once, at boot, whether this worker can type-check.
//
// It is a log line and not a refusal to boot. Every other boot check guards
// something without which the indexer cannot do its job; this one guards a
// stage whose absence downgrades a label, and an operator with no toolchain
// indexing with syntactic edges is a better product than one who cannot index
// at all (open question 9). The line exists because the failure it announces
// is otherwise the silent one: a repository of syntactic edges looks exactly
// like a repository with no in-repo calls.
func (ix *indexer) logToolchain() {
	switch {
	case !ix.lim.typecheck:
		ix.log.Info().Msg("TYPECHECK is off: every edge this worker writes will be syntactic")
	case ix.lim.goBin == "":
		ix.log.Warn().Msg("no go binary on PATH: every edge this worker writes will be syntactic")
	default:
		ix.log.Info().Str("go", ix.lim.goBin).Str("goproxy", ix.lim.goProxy).Msg("type-checking with the go binary on PATH")
	}
}

// goToolchain is the go binary the loader will actually run: go/packages forks
// exec.Command("go", …), which resolves against this process's PATH, so
// anything else would be a policy about a binary nobody runs. Empty when there
// is none, which Resolve reports as no_toolchain rather than as an error.
func goToolchain() string {
	p, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	return p
}

// typecheckEnabled reads the kill switch, and parses it rather than comparing
// against "true" for stripDocs' reason: TYPECHECK=no would otherwise read as
// enabled, and a knob an operator believes is in force and is not is the shape
// of bug this project has already shipped.
func typecheckEnabled() (bool, error) {
	v := config.Get("TYPECHECK", "true")
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("TYPECHECK must be a boolean, got %q", v)
	}
	return b, nil
}

// typecheckProxy reads the module proxy the type-check may use, and refuses a
// bad one at boot.
//
// At boot rather than per job, and this is the line between the two failures:
// a missing toolchain is the operator's environment and downgrades a label,
// while a GOPROXY naming direct is a setting that would turn a stranger's
// go.mod into an outbound connection of their choosing. The first must not
// stop a worker; the second must not start one.
func typecheckProxy() (string, error) {
	p := config.Get("TYPECHECK_GOPROXY", symbols.ProxyOff)
	if err := symbols.ValidateProxy(p); err != nil {
		return "", err
	}
	return p, nil
}

// removeScratch removes a scratch tree, including one the go command left
// read-only.
//
// Measured in Task 3: with a proxy configured, the module cache's directories
// are written 0555 and os.RemoveAll then fails with permission denied —
// leaking not the cache but the whole tree it is in, one job at a time.
// Removing a file needs write permission on its parent, so restoring the
// directories' is enough, and the retry is only reached when the first pass
// failed.
func removeScratch(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
