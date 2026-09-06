import type {
  Answered, ApproximateCaller, AskResult, Caller, Callers, Citation, Cited,
  Degraded, Floor, Hit, Job, LLMTrace, RepoDetail, RepoList, RepoRow,
  SearchResult, Span, SpanRead, Staleness, Symbol, SymbolList, SymbolRead,
  ToolInvocation,
} from "./types";

// A parser is total: it returns null for a body it cannot read, and http.ts
// turns that into a failed outcome. A malformed body from a proxy or a captive
// portal must render as an error, not unmount the tree.
export type Parse<T> = (body: unknown) => T | null;

type Obj = Record<string, unknown>;

function obj(v: unknown): Obj | null {
  return typeof v === "object" && v !== null && !Array.isArray(v) ? (v as Obj) : null;
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function int(v: unknown): number {
  return typeof v === "number" ? v : 0;
}

function bool(v: unknown): boolean {
  return v === true;
}

// NORMALISATION 2 of 4. A score that is not a number stays null and is never
// coerced: read.go:123-128 — "0 there is a real similarity and would read as
// one". A cosine similarity of zero means orthogonal; "we did not compute one"
// means nothing at all.
function num(v: unknown): number | null {
  return typeof v === "number" ? v : null;
}

function arr(v: unknown): unknown[] {
  return Array.isArray(v) ? v : [];
}

function staleness(v: unknown): Staleness {
  const o = obj(v) ?? {};
  return {
    state: o["state"] === "superseded" ? "superseded" : "unknown",
    forge_checked: bool(o["forge_checked"]),
    indexed_at: str(o["indexed_at"]),
    newer_commit: str(o["newer_commit"]),
    newer_indexed_at: str(o["newer_indexed_at"]),
    note: str(o["note"]),
  };
}

function citation(v: unknown): Citation | null {
  const o = obj(v);
  if (!o) return null;
  return {
    repo_id: str(o["repo_id"]),
    remote: str(o["remote"]),
    commit: str(o["commit"]),
    ref: str(o["ref"]),
    path: str(o["path"]),
    start_line: int(o["start_line"]),
    end_line: int(o["end_line"]),
    digest: str(o["digest"]),
    // NORMALISATION 3 of 4. An empty permalink stays empty. It is never
    // composed from remote + commit + path: cite.go keeps a *table* of forge
    // shapes because the two allowlisted forges genuinely differ, and returns
    // "" rather than guessing for anything else (cite.go:172-186).
    permalink: str(o["permalink"]),
    staleness: staleness(o["staleness"]),
  };
}

function span(v: unknown): Span | null {
  const o = obj(v);
  if (!o) return null;
  return {
    id: str(o["id"]),
    path: str(o["path"]),
    kind: str(o["kind"]) as Span["kind"],
    symbol: str(o["symbol"]),
    start_line: int(o["start_line"]),
    end_line: int(o["end_line"]),
    text: str(o["text"]),
  };
}

function symbol(v: unknown): Symbol | null {
  const o = obj(v);
  if (!o) return null;
  return {
    id: str(o["id"]),
    name: str(o["name"]),
    pkg: str(o["pkg"]),
    kind: str(o["kind"]) as Symbol["kind"],
    path: str(o["path"]),
    start_line: int(o["start_line"]),
    end_line: int(o["end_line"]),
    span_id: str(o["span_id"]),
  };
}

function floor(v: unknown): Floor {
  const o = obj(v) ?? {};
  return {
    value: int(o["value"]),
    calibrated: bool(o["calibrated"]),
    applicable: bool(o["applicable"]),
  };
}

// NORMALISATION 5 of 5. An absent block stays null and is never filled in with
// an empty object. `degraded` is on the wire IFF the loop was attempted and did
// not write the answer, and `llm` IFF the loop ran at all (read.go:67-71,
// 161-165); a default {} for either would turn "the model was never asked" into
// "the model degraded for no reason" and "the loop never ran" into "the loop
// ran and stopped at step 0" — which is exactly the distinction those two
// omitempty tags exist to keep.
function degraded(v: unknown): Degraded | null {
  const o = obj(v);
  if (!o) return null;
  return { from: str(o["from"]), reason: str(o["reason"]) };
}

function llm(v: unknown): LLMTrace | null {
  const o = obj(v);
  if (!o) return null;
  const tools: ToolInvocation[] = [];
  // `tools` serialises as null when the loop made no tool call — the trace is
  // built with append from a nil slice, like `citations` above — and a .map on
  // null throws. That is not hypothetical: a provider failure on turn one is
  // the commonest degradation there is, and ask-answered-degraded.json is
  // exactly that shape.
  for (const t of arr(o["tools"])) {
    const row = obj(t);
    if (!row) continue;
    tools.push({ name: str(row["name"]), ms: int(row["ms"]) });
  }
  const u = obj(o["usage"]) ?? {};
  return {
    model: str(o["model"]),
    steps: int(o["steps"]),
    tool_calls: int(o["tool_calls"]),
    stop: str(o["stop"]),
    tools,
    usage: {
      input_tokens: int(u["input_tokens"]),
      output_tokens: int(u["output_tokens"]),
      estimated: bool(u["estimated"]),
    },
    citations_dropped: int(o["citations_dropped"]),
  };
}

export const parseJob: Parse<Job> = (body) => {
  const o = obj(body);
  if (!o) return null;
  return {
    id: str(o["id"]),
    remote: str(o["remote"]),
    ref: str(o["ref"]),
    status: str(o["status"]) as Job["status"],
    repo_id: str(o["repo_id"]),
  };
};

export const parseRepoList: Parse<RepoList> = (body) => {
  const o = obj(body);
  if (!o) return null;
  const repos: RepoRow[] = [];
  // The server's order, kept. ListRepos is most-recently-used first
  // (packages/shared/store/read.go:36) and that order is the API's claim.
  for (const r of arr(o["repos"])) {
    const row = obj(r);
    if (!row) continue;
    repos.push({
      id: str(row["id"]),
      remote: str(row["remote"]),
      ref: str(row["ref"]),
      commit: str(row["commit"]),
      indexed_at: str(row["indexed_at"]),
      last_used_at: str(row["last_used_at"]),
    });
  }
  return { repos, count: int(o["count"]) };
};

export const parseRepo: Parse<RepoDetail> = (body) => {
  const o = obj(body);
  if (!o) return null;
  return {
    id: str(o["id"]),
    remote: str(o["remote"]),
    ref: str(o["ref"]),
    commit: str(o["commit"]),
    indexed_at: str(o["indexed_at"]),
    files: int(o["files"]),
    spans: int(o["spans"]),
    files_with_spans: int(o["files_with_spans"]),
    symbols: int(o["symbols"]),
    edges: int(o["edges"]),
    edges_resolved: int(o["edges_resolved"]),
    edges_syntactic: int(o["edges_syntactic"]),
    staleness: staleness(o["staleness"]),
  };
};

export const parseSpanRead: Parse<SpanRead> = (body) => {
  const o = obj(body);
  if (!o) return null;
  const sp = span(o["span"]);
  const c = citation(o["citation"]);
  if (!sp || !c) return null;
  return { span: sp, citation: c };
};

export const parseSearch: Parse<SearchResult> = (body) => {
  const o = obj(body);
  if (!o) return null;
  const hits: Hit[] = [];
  for (const h of arr(o["hits"])) {
    const row = obj(h);
    if (!row) continue;
    const c = citation(row["citation"]);
    if (!c) continue;
    hits.push({
      span_id: str(row["span_id"]),
      path: str(row["path"]),
      kind: str(row["kind"]) as Hit["kind"],
      symbol: str(row["symbol"]),
      start_line: int(row["start_line"]),
      end_line: int(row["end_line"]),
      text: str(row["text"]),
      score: int(row["score"]),
      vector_score: num(row["vector_score"]),
      vector_rank: int(row["vector_rank"]),
      lexical_rank: int(row["lexical_rank"]),
      citation: c,
    });
  }
  return {
    repo_id: str(o["repo_id"]),
    mode: str(o["mode"]),
    top_score: num(o["top_score"]),
    count: int(o["count"]),
    hits,
  };
};

export const parseAsk: Parse<AskResult> = (body) => {
  const o = obj(body);
  if (!o) return null;
  const common = {
    repo_id: str(o["repo_id"]),
    mode: str(o["mode"]),
    top_score: num(o["top_score"]),
    floor: floor(o["floor"]),
    // On BOTH branches, because read.go puts them on answerResponse and on
    // refusalResponse. answered_by is here rather than on the answer alone for
    // the same reason: P3 put it on one shape and not the other, which left a
    // refusal unable to say which answerer refused.
    answered_by: str(o["answered_by"]),
    degraded: degraded(o["degraded"]),
    llm: llm(o["llm"]),
  };
  // The discriminant is `refused`, not the presence or truthiness of `answer`.
  // An answered response can carry answer: "" — every hit's span failed to
  // travel (answer.go:83-89) — and branching on the answer reads that as a
  // refusal, then renders a refusal with no reason.
  if (o["refused"] === true) {
    return {
      ...common,
      refused: true,
      reason: str(o["reason"]) as Refused["reason"],
      detail: str(o["detail"]),
    };
  }
  const citations: Cited[] = [];
  // NORMALISATION 1 of 4. `citations` is the one array in this API that
  // serialises as null: Assemble builds it with append from a nil slice
  // (answer.go:99-105) while every other list route allocates with make.
  // `arr` is what turns that null into [], and a .map on null throws.
  for (const c of arr(o["citations"])) {
    const row = obj(c);
    if (!row) continue;
    const cit = citation(row["citation"]);
    if (!cit) continue;
    citations.push({
      marker: int(row["marker"]),
      span_id: str(row["span_id"]),
      kind: str(row["kind"]) as Cited["kind"],
      symbol: str(row["symbol"]),
      citation: cit,
    });
  }
  const answered: Answered = {
    ...common,
    refused: false,
    answer: str(o["answer"]),
    citations,
    dropped: int(o["dropped"]),
  };
  return answered;
};

type Refused = Extract<AskResult, { refused: true }>;

export const parseSymbolList: Parse<SymbolList> = (body) => {
  const o = obj(body);
  if (!o) return null;
  const symbols: Symbol[] = [];
  for (const s of arr(o["symbols"])) {
    const sy = symbol(s);
    if (sy) symbols.push(sy);
  }
  return {
    repo_id: str(o["repo_id"]),
    count: int(o["count"]),
    matched: str(o["matched"]),
    truncated: bool(o["truncated"]),
    symbols,
    staleness: staleness(o["staleness"]),
  };
};

export const parseSymbolRead: Parse<SymbolRead> = (body) => {
  const o = obj(body);
  if (!o) return null;
  const sy = symbol(o["symbol"]);
  if (!sy) return null;
  return {
    symbol: sy,
    // NORMALISATION 4 of 4. A null citation stays null and never becomes an
    // empty one: what is missing is the digest, and a citation carrying an
    // empty digest claims text nobody can check against (graph.go:335-345).
    citation: citation(o["citation"]),
    staleness: staleness(o["staleness"]),
  };
};

function callSite(v: unknown) {
  const o = obj(v) ?? {};
  return { path: str(o["path"]), line: int(o["line"]) };
}

export const parseCallers: Parse<Callers> = (body) => {
  const o = obj(body);
  if (!o) return null;
  const sy = symbol(o["symbol"]);
  if (!sy) return null;

  const callers: Caller[] = [];
  for (const c of arr(o["callers"])) {
    const row = obj(c);
    if (!row) continue;
    const s = symbol(row["symbol"]);
    if (!s) continue;
    callers.push({
      symbol: s,
      depth: int(row["depth"]),
      provenance: str(row["provenance"]) as Caller["provenance"],
      call: callSite(row["call"]),
      citation: citation(row["citation"]),
    });
  }

  const ap = obj(o["approximate"]) ?? {};
  const approxCallers: ApproximateCaller[] = [];
  for (const c of arr(ap["callers"])) {
    const row = obj(c);
    if (!row) continue;
    const s = symbol(row["symbol"]);
    if (!s) continue;
    approxCallers.push({
      symbol: s,
      to_name: str(row["to_name"]),
      provenance: str(row["provenance"]) as ApproximateCaller["provenance"],
      call: callSite(row["call"]),
      citation: citation(row["citation"]),
    });
  }

  return {
    repo_id: str(o["repo_id"]),
    symbol: sy,
    depth: int(o["depth"]),
    truncated: bool(o["truncated"]),
    callers,
    // Two lists, never one. graph.go:94-97: "A sibling field with its own
    // count, never merged into callers."
    approximate: {
      matched_on: str(ap["matched_on"]),
      count: int(ap["count"]),
      truncated: bool(ap["truncated"]),
      failed: bool(ap["failed"]),
      callers: approxCallers,
    },
  };
};
