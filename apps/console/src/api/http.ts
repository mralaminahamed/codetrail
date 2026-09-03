import type { Parse } from "./parse";
import type { Outcome } from "./types";

// A relative prefix, and there is no VITE_API_URL. A base URL compiled into the
// bundle is a bundle per environment and a cross-origin request the gateway has
// no header for; Vite's dev and preview proxies are what make the console
// same-origin, in every environment, without CORS on a public API that clones a
// stranger's URL.
export const API_BASE = "/api";

export function apiUrl(path: string, query?: Record<string, string | number | boolean | undefined>): string {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v !== undefined) qs.set(k, String(v));
  }
  const tail = qs.toString();
  return API_BASE + path + (tail ? `?${tail}` : "");
}

async function bodyOf(res: Response): Promise<unknown> {
  // An error page is text/html far more often than anyone expects: a proxy, a
  // captive portal, a load balancer with no route. It must render as an error
  // and never as a thrown parse.
  try {
    return await res.json();
  } catch {
    return null;
  }
}

function field(body: unknown, key: string): string | null {
  if (typeof body !== "object" || body === null) return null;
  const v = (body as Record<string, unknown>)[key];
  return typeof v === "string" ? v : null;
}

function outcomeFor<T>(res: Response, body: unknown): Outcome<T> | null {
  if (res.ok) return null;
  const detail = field(body, "error");
  switch (res.status) {
    case 400:
      // rule is a category and error is the message: several details share one
      // rule (handler.go:152-165), and outside POST /api/repos badRequest
      // hard-codes "form" (read.go:497-500). The console renders detail.
      return { kind: "rejected", rule: field(body, "rule") ?? "", detail: detail ?? "The request was refused." };
    case 404:
      return { kind: "missing", detail: detail ?? "Not found." };
    case 410:
      return { kind: "gone", detail: detail ?? "Gone." };
    default:
      return {
        kind: "failed",
        detail: detail ?? "codetrail failed and did not say why.",
        requestId: field(body, "request_id"),
      };
  }
}

async function send<T>(path: string, init: RequestInit, parse: Parse<T>): Promise<Outcome<T>> {
  let res: Response;
  try {
    res = await fetch(path, init);
  } catch (e) {
    // A dropped connection has no request id and a different next action from a
    // 500, which is why it is its own member of Outcome.
    return { kind: "unreachable", detail: e instanceof Error ? e.message : "codetrail could not be reached." };
  }
  const body = await bodyOf(res);
  const bad = outcomeFor<T>(res, body);
  if (bad) return bad;
  const value = parse(body);
  if (value === null) {
    return { kind: "failed", detail: "codetrail sent a response this console could not read.", requestId: null };
  }
  return { kind: "ok", value };
}

export function get<T>(path: string, parse: Parse<T>): Promise<Outcome<T>> {
  return send(path, { method: "GET", headers: { accept: "application/json" } }, parse);
}

export function post<T>(path: string, body: unknown, parse: Parse<T>): Promise<Outcome<T>> {
  return send(
    path,
    {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify(body),
    },
    parse,
  );
}
