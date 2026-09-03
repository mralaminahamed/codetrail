import { describe, expect, test } from "vitest";
import { apiUrl, get } from "./http";
import { parseJob, parseRepo } from "./parse";
import { stub, stubHtml, stubUnreachable } from "../test/msw";
import error400 from "./fixtures/error-400-scheme.json";
import error404repo from "./fixtures/error-404-repo.json";
import error404job from "./fixtures/error-404-job.json";
import error410 from "./fixtures/error-410-repo.json";
import error500 from "./fixtures/error-500.json";

describe("the transport", () => {
  test("the request path is relative, so the browser stays same-origin", () => {
    // Strict equality on the whole composed URL, not a toContain on the path:
    // an absolute base would still contain "/api/repos".
    expect(apiUrl("/repos")).toBe("/api/repos");
    expect(apiUrl("/repos", { limit: 3 })).toBe("/api/repos?limit=3");
    expect(apiUrl("/jobs/abc")).toBe("/api/jobs/abc");
  });

  test("a 400 becomes rejected and carries the server's detail and rule", async () => {
    stub("get", "/api/repos/:repo", 400, error400);
    const out = await get("/api/repos/x", parseRepo);
    expect(out.kind).toBe("rejected");
    if (out.kind !== "rejected") throw new Error("unreachable");
    expect(out.detail).toBe('scheme "http"; only https is accepted');
    expect(out.rule).toBe("scheme");
  });

  test("a 404 becomes missing, a 410 becomes gone, and the two detail strings differ", async () => {
    stub("get", "/api/repos/:repo", 404, error404repo);
    const missing = await get("/api/repos/x", parseRepo);
    expect(missing.kind).toBe("missing");
    if (missing.kind !== "missing") throw new Error("unreachable");
    expect(missing.detail).toBe("no such repository");

    stub("get", "/api/repos/:repo", 410, error410);
    const gone = await get("/api/repos/x", parseRepo);
    expect(gone.kind).toBe("gone");
    if (gone.kind !== "gone") throw new Error("unreachable");
    expect(gone.detail).toBe("this repository was indexed and has since been evicted");

    // 404 and 410 are different facts, and the words are how a reader tells
    // "never here" from "was here and was evicted" (read.go:424-432).
    expect(gone.detail).not.toBe(missing.detail);
  });

  test("a 404 on a job says the job is unknown, not that the repository is", async () => {
    stub("get", "/api/jobs/:id", 404, error404job);
    const out = await get("/api/jobs/x", parseJob);
    expect(out.kind).toBe("missing");
    if (out.kind !== "missing") throw new Error("unreachable");
    expect(out.detail).toBe("no such job");
  });

  test("a 500 becomes failed and carries the request id from the body", async () => {
    stub("get", "/api/repos/:repo", 500, error500);
    const out = await get("/api/repos/x", parseRepo);
    expect(out.kind).toBe("failed");
    if (out.kind !== "failed") throw new Error("unreachable");
    expect(out.detail).toBe("internal error");
    // The id is the whole point of the 500 body: it is what ties a caller's
    // report to the log line (handler.go:189-197).
    expect(out.requestId).toBe("01JQ0FIXTURE0000000000REQ");
  });

  test("a fetch rejection becomes unreachable, not failed", async () => {
    stubUnreachable("get", "/api/repos/:repo");
    const out = await get("/api/repos/x", parseRepo);
    expect(out.kind).toBe("unreachable");
  });

  test("an HTML error page becomes failed with a null request id, and does not throw", async () => {
    stubHtml("get", "/api/repos/:repo", 502);
    const out = await get("/api/repos/x", parseRepo);
    expect(out.kind).toBe("failed");
    if (out.kind !== "failed") throw new Error("unreachable");
    // No id to quote, and saying so is different from inventing one.
    expect(out.requestId).toBeNull();
  });

  test("a 200 whose body cannot be read is failed, not ok", async () => {
    stubHtml("get", "/api/repos/:repo", 200);
    const out = await get("/api/repos/x", parseRepo);
    expect(out.kind).toBe("failed");
    if (out.kind !== "failed") throw new Error("unreachable");
    expect(out.requestId).toBeNull();
  });
});
