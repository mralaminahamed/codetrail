import { describe, expect, test } from "vitest";
import { parseAsk, parseCallers, parseSearch, parseSpanRead, parseSymbolRead } from "./parse";
import askAnswered from "./fixtures/ask-answered.json";
import askAnsweredEmpty from "./fixtures/ask-answered-empty.json";
import askAnsweredLLM from "./fixtures/ask-answered-llm.json";
import askAnsweredDegraded from "./fixtures/ask-answered-degraded.json";
import askRefusedDegraded from "./fixtures/ask-refused-degraded.json";
import askRefusedNoSpans from "./fixtures/ask-refused-no-spans.json";
import searchHybrid from "./fixtures/search-hybrid.json";
import searchLexical from "./fixtures/search-lexical.json";
import citationNoLink from "./fixtures/citation-nolink.json";
import citationCodeberg from "./fixtures/citation-codeberg.json";
import symbolNoSpan from "./fixtures/symbol-nospan.json";
import callers from "./fixtures/callers.json";

describe("the four normalisations", () => {
  test("an answered response with citations null parses to an empty array", () => {
    // ask-answered.json cannot separate this mutant: it has citations, so
    // `?? []` never runs. This fixture's citations really are JSON null.
    expect(askAnsweredEmpty.citations).toBeNull();

    const out = parseAsk(askAnsweredEmpty);
    expect(out).not.toBeNull();
    if (out === null || out.refused) throw new Error("unreachable");
    // Deep equality on the value, at the parser. Not through a render: a .map
    // on null inside render() throws, and a thrown render is an incident, not
    // a kill.
    expect(out.citations).toEqual([]);
    expect(Array.isArray(out.citations)).toBe(true);
  });

  test("a null top_score stays null and is never coerced to zero", () => {
    expect(searchLexical.top_score).toBeNull();
    const lexical = parseSearch(searchLexical);
    expect(lexical?.top_score).toBeNull();

    // And the other side: a real one survives unchanged, so a mutant that
    // nulls everything fails too.
    const hybrid = parseSearch(searchHybrid);
    expect(hybrid?.top_score).toBe(searchHybrid.top_score);
    expect(typeof hybrid?.top_score).toBe("number");
  });

  test("a null vector_score on a hit the vector arm did not return stays null", () => {
    // search-hybrid.json is the only fixture that can separate this: it holds
    // a hit the vector arm returned, with a real cosine similarity, BESIDE a
    // hit only the lexical arm returned. In search-lexical.json every score is
    // null, so a blanket `?? 0` produces a uniformly wrong column that a
    // "some are null" assertion never reaches.
    const out = parseSearch(searchHybrid);
    expect(out).not.toBeNull();
    const hits = out!.hits;

    const scored = hits.find((h) => h.vector_rank > 0);
    const unscored = hits.find((h) => h.vector_rank === 0);
    expect(scored).toBeDefined();
    expect(unscored).toBeDefined();

    expect(unscored!.vector_score).toBeNull();
    expect(scored!.vector_score).toBe(0.559502899646759);
  });

  test("every vector_score is null in a lexical-only result, and top_score is too", () => {
    const out = parseSearch(searchLexical);
    expect(out).not.toBeNull();
    expect(out!.top_score).toBeNull();
    expect(out!.hits.length).toBeGreaterThan(1);
    for (const h of out!.hits) {
      expect(h.vector_score).toBeNull();
      expect(h.vector_rank).toBe(0);
    }
  });

  test("an empty permalink stays an empty string and is not made into a URL", () => {
    const out = parseSpanRead(citationNoLink);
    expect(out).not.toBeNull();
    // Exactly "", not a guessed URL and not undefined.
    expect(out!.citation.permalink).toBe("");
    // The tuple is still whole: the claim survives the missing convenience.
    expect(out!.citation.digest).not.toBe("");
    expect(out!.citation.commit).not.toBe("");
    expect(out!.citation.path).toBe("calc/calc.go");
  });

  test("a permalink is carried verbatim and never composed", () => {
    const out = parseSpanRead(citationCodeberg);
    expect(out).not.toBeNull();
    // Codeberg runs Forgejo: /src/commit/, not /blob/ (cite.go:151). A client
    // that composed the URL would produce the GitHub shape here.
    expect(out!.citation.permalink).toBe(citationCodeberg.citation.permalink);
    expect(out!.citation.permalink).toContain("/src/commit/");
    expect(out!.citation.permalink).not.toContain("/blob/");
  });

  test("a caller row with a null citation parses to null, not to an empty citation", () => {
    const out = parseCallers(callers);
    expect(out).not.toBeNull();
    const rows = out!.callers;
    expect(rows.length).toBe(2);

    const withDigest = rows.find((c) => c.citation !== null);
    const without = rows.find((c) => c.citation === null);
    expect(withDigest).toBeDefined();
    expect(without).toBeDefined();

    // Named by its own path and line range, not by a count: a count survives a
    // mutant that drops the row entirely.
    expect(without!.symbol.name).toBe("Table");
    expect(without!.symbol.path).toBe("big.go");
    expect(without!.symbol.span_id).toBe("");
    expect(without!.citation).toBeNull();
    expect(withDigest!.citation?.digest).not.toBe("");
  });

  test("a definition with no span parses to a null citation and keeps its location", () => {
    const out = parseSymbolRead(symbolNoSpan);
    expect(out).not.toBeNull();
    expect(out!.citation).toBeNull();
    expect(out!.symbol.path).toBe("big.go");
    expect(out!.symbol.start_line).toBe(2);
    expect(out!.symbol.end_line).toBe(208);
    // The staleness claim survives the missing citation: the repository's ref
    // may still have moved (graph.go:199-201).
    expect(out!.staleness.note).toContain("codetrail has not checked whether main has moved since.");
  });

  test("refused is the discriminant, not the presence of an answer", () => {
    const refused = parseAsk(askRefusedNoSpans);
    expect(refused?.refused).toBe(true);

    const answered = parseAsk(askAnswered);
    expect(answered?.refused).toBe(false);

    // The trap: this response is answered and its answer is "".
    const emptyAnswer = parseAsk(askAnsweredEmpty);
    expect(emptyAnswer?.refused).toBe(false);
    if (emptyAnswer === null || emptyAnswer.refused) throw new Error("unreachable");
    expect(emptyAnswer.answer).toBe("");
  });

  test("a body that is not an object parses to null rather than throwing", () => {
    expect(parseAsk(null)).toBeNull();
    expect(parseAsk("<!doctype html>")).toBeNull();
    expect(parseSearch([])).toBeNull();
    expect(parseCallers(42)).toBeNull();
  });

  test("degraded and llm are absent as null, never as an empty block", () => {
    // The distinction the two omitempty tags exist to keep. A default {} for
    // either turns "the model was never asked" into "the model degraded for no
    // reason", and "the loop never ran" into "the loop ran and stopped at step
    // 0" — both of which read as facts and are not.
    const plain = parseAsk(askAnswered);
    if (plain === null || plain.refused) throw new Error("unreachable");
    expect(plain.degraded).toBeNull();
    expect(plain.llm).toBeNull();

    const degraded = parseAsk(askAnsweredDegraded);
    if (degraded === null || degraded.refused) throw new Error("unreachable");
    expect(degraded.degraded).toEqual({ from: "llm", reason: "rate_limited" });
    expect(degraded.llm?.stop).toBe("rate_limited");
    // tools serialises as null when the loop made no tool call. arr() is what
    // turns that into [], and a .map on null throws.
    expect(askAnsweredDegraded.llm.tools).toBeNull();
    expect(degraded.llm?.tools).toEqual([]);
    expect(degraded.llm?.usage.estimated).toBe(false);
  });

  test("an llm answer parses its trace, tool names and estimated usage", () => {
    const out = parseAsk(askAnsweredLLM);
    if (out === null || out.refused) throw new Error("unreachable");
    expect(out.answered_by).toBe("llm");
    expect(out.degraded).toBeNull();
    expect(out.llm?.model).toBe("fake-scripted");
    expect(out.llm?.tools).toEqual([{ name: "read_span", ms: 0 }]);
    // estimated is a real distinction: a token count the client sized itself is
    // not a token count the provider reported.
    expect(out.llm?.usage.estimated).toBe(true);
    expect(out.llm?.usage.input_tokens).toBeGreaterThan(0);
  });

  test("a refusal carries answered_by, degraded and llm too", () => {
    // P3 put answered_by on the answer and not on the refusal, which left a
    // refusal unable to say which answerer refused. read.go puts all three on
    // refusalResponse, and this is the payload that proves the parser reads
    // them off both branches rather than only the one.
    const out = parseAsk(askRefusedDegraded);
    if (out === null || !out.refused) throw new Error("unreachable");
    expect(out.answered_by).toBe("extractive");
    expect(out.reason).toBe("below_floor");
    expect(out.degraded).toEqual({ from: "llm", reason: "provider_unavailable" });
    expect(out.llm?.stop).toBe("provider_unavailable");

    // And a refusal from an unconfigured deployment carries neither.
    const plain = parseAsk(askRefusedNoSpans);
    if (plain === null || !plain.refused) throw new Error("unreachable");
    expect(plain.degraded).toBeNull();
    expect(plain.llm).toBeNull();
  });
});
