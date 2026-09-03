// The wire, spelled the way it arrives. Nothing here renames a field: a
// snake_case key that becomes camelCase in one place and not another is a
// second shape to keep in agreement, and the parse layer already owns the four
// decisions worth owning.

// Outcome is every way a request can end **except one**: a refusal is not in
// here. Spec:262 makes a refusal and an error distinct outcomes, and the
// gateway answers a refusal with 200 for exactly that reason (read.go:162-165).
// So a refusal is a *value of the ask endpoint*, carried inside kind "ok", and
// rendering one through the error path requires writing kind "failed" by hand.
//
// unreachable is separate from failed on purpose: a 500 is the gateway saying
// it broke and handing back a request id an operator can grep for
// (handler.go:193-197); a fetch that rejects is a browser, a network or a dead
// process, and it has no id. One member for both would put a retry button on an
// internal error and a request id on dropped Wi-Fi.
export type Outcome<T> =
  | { kind: "ok"; value: T }
  | { kind: "rejected"; rule: string; detail: string }
  | { kind: "missing"; detail: string }
  | { kind: "gone"; detail: string }
  | { kind: "failed"; detail: string; requestId: string | null }
  | { kind: "unreachable"; detail: string };

export type JobStatus = "pending" | "leased" | "done" | "failed";

export type Job = {
  id: string;
  remote: string;
  ref: string;
  status: JobStatus;
  // Empty until the job is done; the key is absent on the wire until then.
  repo_id: string;
};

export type Staleness = {
  state: "unknown" | "superseded";
  forge_checked: boolean;
  indexed_at: string;
  newer_commit: string;
  newer_indexed_at: string;
  // The server's sentence. The console renders it and composes none.
  note: string;
};

export type Citation = {
  repo_id: string;
  remote: string;
  commit: string;
  ref: string;
  path: string;
  start_line: number;
  end_line: number;
  digest: string;
  // "" for a forge whose URL shape codetrail does not know (cite.go:179-186).
  // Never turned into a URL.
  permalink: string;
  staleness: Staleness;
};

export type RepoRow = {
  id: string;
  remote: string;
  ref: string;
  commit: string;
  indexed_at: string;
  last_used_at: string;
};

export type RepoList = { repos: RepoRow[]; count: number };

export type RepoDetail = {
  id: string;
  remote: string;
  ref: string;
  commit: string;
  indexed_at: string;
  files: number;
  spans: number;
  files_with_spans: number;
  symbols: number;
  edges: number;
  edges_resolved: number;
  edges_syntactic: number;
  staleness: Staleness;
};

export type SpanKind = "func" | "type" | "const" | "var" | "file";

export type Span = {
  id: string;
  path: string;
  kind: SpanKind;
  symbol: string;
  start_line: number;
  end_line: number;
  text: string;
};

export type SpanRead = { span: Span; citation: Citation };

export type Hit = {
  span_id: string;
  path: string;
  kind: SpanKind;
  symbol: string;
  start_line: number;
  end_line: number;
  text: string;
  score: number;
  // null when the vector arm did not return this span. 0 there is a real
  // similarity and would read as one (read.go:123-128).
  vector_score: number | null;
  // 0 means that arm did not return this span (rag.go:52).
  vector_rank: number;
  lexical_rank: number;
  citation: Citation;
};

export type SearchResult = {
  repo_id: string;
  mode: string;
  top_score: number | null;
  count: number;
  hits: Hit[];
};

export type Floor = { value: number; calibrated: boolean; applicable: boolean };

export type Cited = {
  marker: number;
  span_id: string;
  kind: SpanKind;
  symbol: string;
  citation: Citation;
};

export type Answered = {
  repo_id: string;
  refused: false;
  answered_by: string;
  answer: string;
  citations: Cited[];
  dropped: number;
  mode: string;
  top_score: number | null;
  floor: Floor;
};

export type RefusalReason = "no_spans" | "below_floor" | "unscored";

export type Refused = {
  repo_id: string;
  refused: true;
  reason: RefusalReason;
  // The server's sentence, one per reason (read.go:386-398). Rendered verbatim.
  detail: string;
  mode: string;
  top_score: number | null;
  floor: Floor;
};

// Discriminated on `refused`, which is the field the server discriminates on —
// never on whether `answer` is non-empty, because an answered response can
// carry answer: "" (answer.go:83-89).
export type AskResult = Answered | Refused;

export type Symbol = {
  id: string;
  name: string;
  pkg: string;
  kind: SpanKind;
  path: string;
  start_line: number;
  end_line: number;
  // "" when the declaration has no span of its own (graph.go:37-39).
  span_id: string;
};

export type SymbolList = {
  repo_id: string;
  count: number;
  matched: string;
  truncated: boolean;
  symbols: Symbol[];
  staleness: Staleness;
};

export type SymbolRead = {
  symbol: Symbol;
  // null when the declaration has no span: the location is in the symbol, what
  // is missing is the digest (graph.go:335-345).
  citation: Citation | null;
  staleness: Staleness;
};

export type CallSite = { path: string; line: number };

export type Provenance = "resolved" | "syntactic";

export type Caller = {
  symbol: Symbol;
  depth: number;
  provenance: Provenance;
  call: CallSite;
  citation: Citation | null;
};

export type ApproximateCaller = {
  symbol: Symbol;
  to_name: string;
  provenance: Provenance;
  call: CallSite;
  citation: Citation | null;
};

// A sibling block with its own count, never merged into callers
// (graph.go:94-97). `failed` is not "empty": one means the query did not run,
// the other means nothing matched.
export type Approximate = {
  matched_on: string;
  count: number;
  truncated: boolean;
  failed: boolean;
  callers: ApproximateCaller[];
};

export type Callers = {
  repo_id: string;
  symbol: Symbol;
  depth: number;
  truncated: boolean;
  callers: Caller[];
  approximate: Approximate;
};
