import { apiUrl, get, post } from "./http";
import {
  parseAsk, parseCallers, parseJob, parseRepo, parseRepoList, parseSearch,
  parseSpanRead, parseSymbolList, parseSymbolRead,
} from "./parse";
import type {
  AskResult, Callers, Job, Outcome, RepoDetail, RepoList, SearchResult,
  SpanRead, SymbolList, SymbolRead,
} from "./types";

// ref is omitted when empty rather than sent as "HEAD": handler.go:156-159
// already substitutes HEAD, and duplicating a default means two places to
// change it, one of them invisible from the server's tests.
export function submitRepo(remote: string, ref: string): Promise<Outcome<Job>> {
  const body: { remote: string; ref?: string } = { remote };
  const trimmed = ref.trim();
  if (trimmed !== "") body.ref = trimmed;
  return post(apiUrl("/repos"), body, parseJob);
}

export function getJob(id: string): Promise<Outcome<Job>> {
  return get(apiUrl(`/jobs/${encodeURIComponent(id)}`), parseJob);
}

export function listRepos(limit?: number): Promise<Outcome<RepoList>> {
  return get(apiUrl("/repos", { limit }), parseRepoList);
}

export function getRepo(repo: string): Promise<Outcome<RepoDetail>> {
  return get(apiUrl(`/repos/${encodeURIComponent(repo)}`), parseRepo);
}

export function getSpan(repo: string, span: string): Promise<Outcome<SpanRead>> {
  return get(apiUrl(`/repos/${encodeURIComponent(repo)}/spans/${encodeURIComponent(span)}`), parseSpanRead);
}

// q travels in the body, never a query string: a question is logged by every
// proxy between the caller and the gateway (handler.go:49-52). `mode` is never
// sent — read.go:452-455 refuses the field outright, every value including the
// configured one, so sending it turns every query into a 400.
//
// `answerer` is NOT in this type, and unlike `mode` that is a decision rather
// than a constraint: /ask honours it (read.go:297-303, 519-537). It is left out
// for two reasons that pull the same way.
//
// 1. It is the operator's setting, not the caller's. ANSWER_DEFAULT is what the
//    deployment decided; a console that put `answerer` on every request would
//    override that from the browser and make its answers differ from every
//    other client's against the same gateway, silently.
// 2. A control offering it would be a 400 generator on the default deployment.
//    The console cannot see whether a model provider is configured — that is
//    boot configuration the browser has no view of, the same reason Submit.tsx
//    refuses to check ALLOWED_HOSTS client-side. Asking for `llm` where there
//    is none is a 400 naming the rule, which is right for an API and wrong for
//    a button that looked available.
//
// Nothing is lost by leaving it out: the one thing the console needs — WHICH
// answerer wrote the prose, and whether one was tried and failed — arrives on
// every response as `answered_by`, `degraded` and `llm`, and is rendered.
type Query = { q: string; limit?: number };

function query(q: string, limit?: number): Query {
  const body: Query = { q };
  if (limit !== undefined) body.limit = limit;
  return body;
}

export function search(repo: string, q: string, limit?: number): Promise<Outcome<SearchResult>> {
  return post(apiUrl(`/repos/${encodeURIComponent(repo)}/search`), query(q, limit), parseSearch);
}

export function ask(repo: string, q: string, limit?: number): Promise<Outcome<AskResult>> {
  return post(apiUrl(`/repos/${encodeURIComponent(repo)}/ask`), query(q, limit), parseAsk);
}

export function listSymbols(
  repo: string,
  opts: { name: string; pkg?: string; suffix?: boolean; limit?: number },
): Promise<Outcome<SymbolList>> {
  return get(
    apiUrl(`/repos/${encodeURIComponent(repo)}/symbols`, {
      name: opts.name,
      pkg: opts.pkg || undefined,
      suffix: opts.suffix ? true : undefined,
      limit: opts.limit,
    }),
    parseSymbolList,
  );
}

export function getSymbol(repo: string, symbol: string): Promise<Outcome<SymbolRead>> {
  return get(
    apiUrl(`/repos/${encodeURIComponent(repo)}/symbols/${encodeURIComponent(symbol)}`),
    parseSymbolRead,
  );
}

export function callersOf(
  repo: string,
  symbol: string,
  opts?: { depth?: number; limit?: number },
): Promise<Outcome<Callers>> {
  return get(
    apiUrl(`/repos/${encodeURIComponent(repo)}/symbols/${encodeURIComponent(symbol)}/callers`, {
      depth: opts?.depth,
      limit: opts?.limit,
    }),
    parseCallers,
  );
}
