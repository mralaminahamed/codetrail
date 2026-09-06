//go:build live

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/agent"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/llm"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// liveCorpus is the same Reader and Retriever the read routes use, handed to
// the four tools. The tools call the STORE, so this is the composition the
// shipped gateway builds — no HTTP anywhere inside the loop.
type liveCorpus struct {
	*store.Store
	rag *rag.Retriever
}

func (c liveCorpus) Search(ctx context.Context, repoID, q string, limit int) (rag.Result, error) {
	return c.rag.Search(ctx, repoID, q, limit)
}

// liveLoopHandler is liveHandler plus a scripted model.
//
// NO REAL PROVIDER. There is no API key in this environment and none of these
// numbers came from one: the model is llm.Fake, the corpus and the citations
// are real, and the README says which half is which.
func liveLoopHandler(t *testing.T, st *store.Store, turns ...llm.Turn) (*Handler, *llm.Fake) {
	t.Helper()
	h := liveHandler(t, st, rag.ModeVector, rag.DefaultFloor())
	f := llm.NewFake(turns...)
	h.LLM = NewLoop(f, liveCorpus{Store: st, rag: h.Rag.(*rag.Retriever)},
		agent.DefaultBounds(), agent.DefaultToolLimits(), false, 2, 1_000_000, time.Now)
	h.AnswerDefault = answererLLM
	return h, f
}

func askLive(t *testing.T, h *Handler, repoID, q string) map[string]any {
	t.Helper()
	rec := post(t, h, "/api/repos/"+repoID+"/ask", `{"q":`+strconv.Quote(q)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return out
}

// The strongest regression assertion this phase can make, and it is cheap.
//
// With LLM_PROVIDER=none the /ask payload must differ from P3's by EXACTLY ONE
// ADDED KEY — answered_by on the refusal shape — and nothing else: not a
// reordered citation, not a changed top_score, not a degraded key, not an llm
// key. Task 6 touched the fusion arithmetic, and this is where a float that
// shifted shows up as a re-ranked answer on a real corpus rather than as a
// passing unit test on a fixture.
func TestAnAskWithNoProviderIsByteIdenticalToP3sAnswerLive(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "p7-noprovider", chunk.StrategyAST)

	// No loop at all: this is the shipped default.
	h := liveHandler(t, st, rag.ModeVector, rag.DefaultFloor())
	if h.LLM != nil {
		t.Fatal("liveHandler built a loop; this test is about the deployment that has none")
	}
	answered := askLive(t, h, repo.ID, "how does the sampler drop an event")
	for _, absent := range []string{"degraded", "llm"} {
		if v, ok := answered[absent]; ok {
			t.Errorf("the answer carries %q = %v with no provider configured", absent, v)
		}
	}
	if answered["answered_by"] != answererExtractive {
		t.Errorf("answered_by %v", answered["answered_by"])
	}
	// P3's keys, exactly, plus answered_by.
	wantKeys := []string{
		"answer", "answered_by", "citations", "dropped", "floor", "mode",
		"refused", "repo_id", "top_score",
	}
	if got := sortedKeys(answered); !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("answer keys %v, want %v", got, wantKeys)
	}

	// And the refusal shape, which is where the one added key lives.
	refused := askLive(t, h, repo.ID, "zzzznothingmatchesthisquery")
	if refused["refused"] == true {
		wantRefusal := []string{
			"answered_by", "detail", "floor", "mode", "reason", "refused",
			"repo_id", "top_score",
		}
		if got := sortedKeys(refused); !reflect.DeepEqual(got, wantRefusal) {
			t.Errorf("refusal keys %v, want %v", got, wantRefusal)
		}
		if refused["answered_by"] != answererExtractive {
			t.Errorf("refusal answered_by %v", refused["answered_by"])
		}
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// The citation gate's end-to-end proof: the loop opens a real span in a real
// corpus and the answer's citation carries a digest that hashes the text a
// reader can fetch back out of the store.
//
// A loop that cited a span it did not read, or a digest that does not match, is
// worse than no loop.
func TestTheLoopAnswersOverTheApiWithACheckableCitationLive(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "p7-loop", chunk.StrategyAST)

	// Pick a real span out of the corpus and script the model to read and cite
	// exactly it.
	spanID, want := firstSpan(t, st, repo.ID)
	h, f := liveLoopHandler(t, st,
		llm.Turn{Calls: []llm.ToolCall{{ID: "s", Name: "search_code",
			Args: json.RawMessage(`{"q":"sampler"}`)}}},
		llm.Turn{Calls: []llm.ToolCall{{ID: "r", Name: "read_span",
			Args: json.RawMessage(`{"span_id":"` + spanID + `"}`)}}},
		llm.Turn{Text: "The sampler decides whether an event is written [1]."},
	)
	out := askLive(t, h, repo.ID, "how does the sampler drop an event")

	if out["answered_by"] != answererLLM {
		t.Fatalf("answered_by %v, want llm: %v", out["answered_by"], out)
	}
	tr, _ := out["llm"].(map[string]any)
	if tr == nil {
		t.Fatalf("no llm trace: %v", out)
	}
	if tr["stop"] != string(agent.StopFinal) {
		t.Errorf("stop %v, want final", tr["stop"])
	}
	// The ordered tool list, element by element, so "it used tools" is not the
	// claim being made.
	tools, _ := tr["tools"].([]any)
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if !reflect.DeepEqual(names, []string{"search_code", "read_span"}) {
		t.Errorf("tools %v, want [search_code read_span]", names)
	}
	if tr["steps"] != float64(3) || tr["tool_calls"] != float64(2) {
		t.Errorf("steps %v tool_calls %v, want 3 and 2", tr["steps"], tr["tool_calls"])
	}

	cites, _ := out["citations"].([]any)
	if len(cites) != 1 {
		t.Fatalf("citations %v, want one", out["citations"])
	}
	c := cites[0].(map[string]any)
	if c["span_id"] != spanID {
		t.Errorf("the answer cites %v, want the span the loop opened (%s)", c["span_id"], spanID)
	}
	cit, _ := c["citation"].(map[string]any)
	digest, _ := cit["digest"].(string)
	// THE CHECK: the digest hashes the text the store holds for that span. This
	// is the same claim `git show <sha>:<path> | sed -n '<a>,<b>p' | sha256sum`
	// makes against a real forge, made here against the corpus the citation
	// points at.
	if digest != store.Digest(want.Text) {
		t.Errorf("citation digest %s, but the span's text hashes to %s", digest, store.Digest(want.Text))
	}
	if digest != want.Digest {
		t.Errorf("citation digest %s, but the row's digest is %s", digest, want.Digest)
	}
	if int(cit["start_line"].(float64)) != want.StartLine || int(cit["end_line"].(float64)) != want.EndLine {
		t.Errorf("citation range %v-%v, want %d-%d",
			cit["start_line"], cit["end_line"], want.StartLine, want.EndLine)
	}
	t.Logf("live loop: steps=%v tool_calls=%v stop=%v tools=%v usage=%v citations=%d dropped=%v",
		tr["steps"], tr["tool_calls"], tr["stop"], names, tr["usage"], len(cites), tr["citations_dropped"])
	if n := len(f.Requests()); n != 3 {
		t.Errorf("the model was called %d times, want 3", n)
	}
}

func firstSpan(t *testing.T, st *store.Store, repoID string) (string, models.Span) {
	t.Helper()
	var id string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT id FROM spans WHERE repo_id = $1 ORDER BY path COLLATE "C", start_line LIMIT 1`,
		repoID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	sp, err := st.GetSpan(context.Background(), repoID, id)
	if err != nil {
		t.Fatal(err)
	}
	return id, sp
}

// The drift guard Task 3's decision created: the tools call the store and the
// endpoints call the store, and nothing else makes the two agree.
func TestAToolAndItsEndpointReturnTheSameRowsForOneRepositoryLive(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "p7-drift", chunk.StrategyAST)
	h := liveHandler(t, st, rag.ModeVector, rag.DefaultFloor())
	ts := agent.NewTools(liveCorpus{Store: st, rag: h.Rag.(*rag.Retriever)}, repo.ID, agent.DefaultToolLimits())

	spanID, want := firstSpan(t, st, repo.ID)

	// read_span against GET /api/repos/:repo/spans/:span.
	res, err := ts.Call(context.Background(), llm.ToolCall{
		ID: "r", Name: "read_span", Args: json.RawMessage(`{"span_id":"` + spanID + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	tool := unwrapLive(t, res.Content)
	rec := get(t, h, "/api/repos/"+repo.ID+"/spans/"+spanID)
	if rec.Code != http.StatusOK {
		t.Fatalf("endpoint: %d %s", rec.Code, rec.Body)
	}
	var endpoint struct {
		Span struct {
			ID, Path, Text string
			StartLine      int `json:"start_line"`
			EndLine        int `json:"end_line"`
		} `json:"span"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &endpoint); err != nil {
		t.Fatal(err)
	}
	if tool["span_id"] != endpoint.Span.ID || tool["text"] != endpoint.Span.Text {
		t.Errorf("the tool and the endpoint disagree about span %s", spanID)
	}
	if int(tool["start_line"].(float64)) != endpoint.Span.StartLine ||
		int(tool["end_line"].(float64)) != endpoint.Span.EndLine {
		t.Errorf("the tool and the endpoint disagree about the range of %s", spanID)
	}
	if tool["text"] != want.Text {
		t.Errorf("neither agrees with the row")
	}

	// search_code against POST /api/repos/:repo/search, over the same query.
	sres, err := ts.Call(context.Background(), llm.ToolCall{
		ID: "s", Name: "search_code", Args: json.RawMessage(`{"q":"sampler"}`)})
	if err != nil {
		t.Fatal(err)
	}
	sv := unwrapLive(t, sres.Content)
	hits, _ := sv["hits"].([]any)
	srec := post(t, h, "/api/repos/"+repo.ID+"/search",
		`{"q":"sampler","limit":`+strconv.Itoa(agent.DefaultToolLimits().MaxHits)+`}`)
	if srec.Code != http.StatusOK {
		t.Fatalf("search endpoint: %d %s", srec.Code, srec.Body)
	}
	var searched struct {
		Hits []struct {
			SpanID string `json:"span_id"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &searched); err != nil {
		t.Fatal(err)
	}
	if len(hits) != len(searched.Hits) {
		t.Fatalf("the tool ranked %d spans and the endpoint %d", len(hits), len(searched.Hits))
	}
	for i, h := range hits {
		if got := h.(map[string]any)["span_id"]; got != searched.Hits[i].SpanID {
			t.Errorf("rank %d: the tool says %v and the endpoint says %s", i+1, got, searched.Hits[i].SpanID)
		}
	}
}

func unwrapLive(t *testing.T, framed string) map[string]any {
	t.Helper()
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(framed), "</tool_result>"))
	if i := strings.LastIndex(body, "\n"); i >= 0 {
		body = body[i+1:]
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("tool body is not JSON: %v\n%s", err, framed)
	}
	return v
}

// Every degradation still returns a CITED extractive answer over a real corpus.
func TestEveryDegradationStillReturnsACitedExtractiveAnswerLive(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "p7-degrade", chunk.StrategyAST)

	for _, k := range llm.Kinds {
		t.Run(string(k), func(t *testing.T) {
			h, _ := liveLoopHandler(t, st, llm.Turn{Err: &llm.Failure{Kind: k, Status: 500}})
			out := askLive(t, h, repo.ID, "how does the sampler drop an event")
			if out["answered_by"] != answererExtractive {
				t.Errorf("answered_by %v", out["answered_by"])
			}
			deg, _ := out["degraded"].(map[string]any)
			if deg == nil || deg["reason"] != string(k) {
				t.Errorf("degraded %v, want reason %q", out["degraded"], k)
			}
			if s, _ := out["answer"].(string); s == "" {
				t.Errorf("no extractive answer")
			}
			cs, _ := out["citations"].([]any)
			if len(cs) == 0 {
				t.Fatalf("no citations on the degraded answer")
			}
			// Cited, and the citation is checkable: this is what makes the
			// fallback worth having rather than merely present.
			c := cs[0].(map[string]any)
			cit := c["citation"].(map[string]any)
			sp, err := st.GetSpan(context.Background(), repo.ID, c["span_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if cit["digest"] != store.Digest(sp.Text) {
				t.Errorf("the degraded answer's citation digest does not hash its span's text")
			}
		})
	}
}

// An evicted repository takes the loop with it: 410 before any model call.
func TestAnEvictedRepoTakesTheLoopWithItAndAnswersFourTenLive(t *testing.T) {
	st := liveStore(t)
	repo := indexFixture(t, st, "p7-evicted", chunk.StrategyAST)
	h, f := liveLoopHandler(t, st, llm.Turn{Text: "never reached [1]"})

	if _, err := st.Pool().Exec(context.Background(),
		`INSERT INTO evicted_repos (id, remote, ref, commit_sha, indexed_at) VALUES ($1, $2, $3, $4, now())`,
		repo.ID, repo.Remote, repo.Ref, repo.Commit); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(context.Background(), `DELETE FROM repos WHERE id = $1`, repo.ID); err != nil {
		t.Fatal(err)
	}

	rec := post(t, h, "/api/repos/"+repo.ID+"/ask", `{"q":"sampler"}`)
	if rec.Code != http.StatusGone {
		t.Fatalf("want 410, got %d: %s", rec.Code, rec.Body)
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("the model was called %d times for an evicted repository, want 0", n)
	}
}
