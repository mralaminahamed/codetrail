//go:build live

package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// callGraph writes a graph over the indexed fixture whose call sites are the
// ones actually written in the committed files.
//
// The offsets and lines are found in the bytes rather than typed in, for the
// reason the test below exists: a hand-typed line number would make the "the
// cited line holds this call" assertion a comparison between two copies of the
// same guess. Hand-built rather than run through the indexer's graph stage
// because apps/indexer is the other side of the deployment boundary this suite
// does not cross; what it needs from that path is what decides a row's
// identity, and SymbolID and EdgeID are shared.
func callGraph(t *testing.T, st *store.Store, repo models.Repo) (push models.Symbol) {
	t.Helper()
	ctx := context.Background()

	spanOf := func(path string, start, end int) string {
		t.Helper()
		var id string
		if err := st.Pool().QueryRow(ctx, `
			SELECT id FROM spans
			WHERE repo_id = $1 AND path = $2 AND start_line = $3 AND end_line = $4`,
			repo.ID, path, start, end).Scan(&id); err != nil {
			t.Fatalf("no span for %s:%d-%d: %v", path, start, end, err)
		}
		return id
	}
	sym := func(path, name string, start, end int) models.Symbol {
		return models.Symbol{
			ID:     store.SymbolID(repo.ID, path, start, string(models.KindFunc), name),
			RepoID: repo.ID, FileID: store.FileID(repo.ID, path), Path: path,
			Name: name, Pkg: "calc", Kind: models.KindFunc,
			StartLine: start, EndLine: end, SpanID: spanOf(path, start, end),
		}
	}
	// The ranges are the ones apps/indexer/cmd/index_live_test.go's
	// wantASTSpans pins by hand against these same files.
	push = sym("calc/calc.go", "Machine.Push", 19, 23)
	total := sym("calc/use.go", "Total", 3, 10)

	// m.Push(n) in Total's body: the offset of the callee's identifier, which
	// is what EdgeID is keyed on, and the 1-based line it falls on.
	off, line := callSiteIn(t, "calc/use.go", "Push")
	if err := st.PutGraph(ctx, repo.ID,
		[]models.Symbol{push, total},
		[]models.Edge{{
			ID: store.EdgeID(repo.ID, total.ID, total.Path, off, "Push"), RepoID: repo.ID,
			FromSymbolID: total.ID, ToSymbolID: push.ID, ToName: "Push",
			Kind: models.EdgeCalls, Provenance: models.ProvenanceResolved,
			Path: total.Path, Line: line,
		}}); err != nil {
		t.Fatal(err)
	}
	return push
}

// callSiteIn finds where name is called in the committed file: the byte offset
// of the identifier and the line it is on, both read out of the bytes a
// citation would be checked against.
func callSiteIn(t *testing.T, path, name string) (offset, line int) {
	t.Helper()
	body := fixtureBytes(t, path)
	at := bytes.Index(body, []byte(name+"("))
	if at < 0 {
		t.Fatalf("%s holds no call to %s", path, name)
	}
	return at, bytes.Count(body[:at], []byte{'\n'}) + 1
}

// "Who calls this", over the API, with a citation a reader can check.
//
// Two claims, and the second is the one a graph can quietly get wrong: the
// answer names a caller, and the file:line it cites *is where that call is
// written*. A graph that cites a line nothing calls from is worse than no
// graph, and nothing inside the pipeline can catch it — both halves would be
// derived from the same wrong number. So the check is against the committed
// bytes: the cited line holds the call, and the caller's citation digest is
// the hash of exactly the lines it names.
func TestWhoCallsThisAnswersOverTheApiWithACheckableCitationLive(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "whocalls", chunk.StrategyAST)
	h := liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor())
	push := callGraph(t, st, repo)

	defs := body(t, get(t, h, "/api/repos/"+repo.ID+"/symbols?name=Machine.Push"))
	if defs["count"] != float64(1) || defs["matched"] != "exact" {
		t.Fatalf("definitions of Machine.Push: %v", defs)
	}

	rec := get(t, h, "/api/repos/"+repo.ID+"/symbols/"+push.ID+"/callers")
	if rec.Code != http.StatusOK {
		t.Fatalf("callers: %d %s", rec.Code, rec.Body)
	}
	var out callersPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Symbol.Name != "Machine.Push" || out.Depth != 1 || out.RepoID != repo.ID {
		t.Fatalf("answered for %+v at depth %d in %s", out.Symbol, out.Depth, out.RepoID)
	}
	if len(out.Callers) != 1 {
		t.Fatalf("%d callers, want 1: %+v", len(out.Callers), out.Callers)
	}
	c := out.Callers[0]
	if c.Symbol.Name != "Total" || c.Depth != 1 || c.Provenance != string(models.ProvenanceResolved) {
		t.Errorf("caller %+v at depth %d is %s", c.Symbol, c.Depth, c.Provenance)
	}

	// The cited line, in the file at that commit, holds the call this hop is
	// reporting. Read out of the committed bytes and not out of anything the
	// indexing path produced.
	src := strings.Split(string(fixtureBytes(t, c.Call.Path)), "\n")
	if c.Call.Line < 1 || c.Call.Line > len(src) {
		t.Fatalf("the hop cites %s:%d, which the file does not have", c.Call.Path, c.Call.Line)
	}
	if got := src[c.Call.Line-1]; !strings.Contains(got, "Push(") {
		t.Errorf("%s:%d is %q, which is not a call to Push", c.Call.Path, c.Call.Line, got)
	}

	// And the caller's own citation is checkable the way P2 checked every
	// span: the digest is the hash of exactly the bytes of the lines it names.
	if c.Citation == nil {
		t.Fatal("the caller cites nothing, so nothing about it can be checked")
	}
	var text, digest string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT text, digest FROM spans WHERE repo_id = $1 AND id = $2`,
		repo.ID, c.Symbol.SpanID).Scan(&text, &digest); err != nil {
		t.Fatal(err)
	}
	if c.Citation.Digest != digest || c.Citation.Path != c.Symbol.Path {
		t.Errorf("citation %s %s, want the span's %s %s",
			c.Citation.Path, c.Citation.Digest, c.Symbol.Path, digest)
	}
	sum := sha256.Sum256([]byte(text))
	if got := hex.EncodeToString(sum[:]); got != c.Citation.Digest {
		t.Errorf("the cited text hashes to %s, not to the digest served, %s", got, c.Citation.Digest)
	}
	if !bracketsExactly(fixtureBytes(t, c.Citation.Path), text, c.Citation.StartLine, c.Citation.EndLine) {
		t.Errorf("lines %d-%d of %s are not the bytes this citation stands for:\n%s",
			c.Citation.StartLine, c.Citation.EndLine, c.Citation.Path, text)
	}
	if c.Citation.Commit != repo.Commit || c.Citation.Permalink == "" {
		t.Errorf("citation %+v names no checkable commit", c.Citation)
	}
}

// Eviction takes the graph with it and the graph routes then answer 410 —
// which is one claim in two halves, and each half is passable alone.
//
// A cascade that removed the rows while the routes answered 404 would forget
// the repository was ever here; a tombstone answering 410 over rows that
// survived would be a lie in the other direction. Both are read here, and the
// symbols are counted straight out of Postgres rather than inferred from a
// status code.
func TestAnEvictedRepoTakesItsGraphAndAnswersFourTenLive(t *testing.T) {
	ctx := context.Background()
	st := liveStore(t)
	repo := indexFixture(t, st, "graphevict", chunk.StrategyAST)
	h := liveHandler(t, st, rag.ModeHybrid, rag.DefaultFloor())
	push := callGraph(t, st, repo)

	syms, edges, err := st.CountGraph(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if syms == 0 || edges == 0 {
		t.Fatalf("no graph to evict: %d symbols, %d edges", syms, edges)
	}
	routes := []string{
		"/api/repos/" + repo.ID + "/symbols?name=Machine.Push",
		"/api/repos/" + repo.ID + "/symbols/" + push.ID,
		"/api/repos/" + repo.ID + "/symbols/" + push.ID + "/callers",
	}
	for _, r := range routes {
		if rec := get(t, h, r); rec.Code != http.StatusOK {
			t.Fatalf("GET %s before eviction: %d %s", r, rec.Code, rec.Body)
		}
	}

	if _, err := st.Evict(ctx, 0, 100); err != nil {
		t.Fatal(err)
	}

	syms, edges, err = st.CountGraph(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if syms != 0 || edges != 0 {
		t.Errorf("%d symbols and %d edges outlived their repository", syms, edges)
	}
	for _, r := range routes {
		rec := get(t, h, r)
		if rec.Code == http.StatusNotFound {
			t.Errorf("GET %s: an evicted repository answered 404, which forgets it was ever here", r)
		}
		if rec.Code != http.StatusGone {
			t.Errorf("GET %s after eviction: %d %s, want 410", r, rec.Code, rec.Body)
		}
	}
}
