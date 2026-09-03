import { describe, expect, test } from "vitest";
import { ask, callersOf, listSymbols, search, submitRepo } from "./client";
import { stub } from "../test/msw";
import askAnswered from "./fixtures/ask-answered.json";
import askRefusedNoSpans from "./fixtures/ask-refused-no-spans.json";
import askRefusedBelowFloor from "./fixtures/ask-refused-below-floor.json";
import askRefusedUnscored from "./fixtures/ask-refused-unscored.json";
import searchHybrid from "./fixtures/search-hybrid.json";
import jobPending from "./fixtures/job-pending.json";
import callers from "./fixtures/callers.json";

function keys(body: unknown): string[] {
  return Object.keys(body as object).sort();
}

describe("the outcome taxonomy", () => {
  test("a refusal parses to ok with refused true, never to a failed outcome", async () => {
    // The status is 200. There is no such thing as a non-200 refusal, which is
    // precisely why this mutation is invisible to any status-code assertion.
    stub("post", "/api/repos/:repo/ask", 200, askRefusedNoSpans);
    const out = await ask("r", "why");
    expect(out.kind).toBe("ok");
    if (out.kind !== "ok") throw new Error("unreachable");
    expect(out.value.refused).toBe(true);
    if (!out.value.refused) throw new Error("unreachable");
    expect(out.value.reason).toBe("no_spans");
    expect(out.value.detail).toBe("Nothing in this repository's index matched the question.");
  });

  test("all three refusal reasons parse to ok, each with its own sentence", async () => {
    const cases = [
      [askRefusedNoSpans, "no_spans", "Nothing in this repository's index matched the question."],
      [askRefusedBelowFloor, "below_floor", "The best match scored under the configured floor of 0.99. That floor is not calibrated; its value is measured in P6."],
      [askRefusedUnscored, "unscored", "The best match has no usable similarity score, so there is nothing to judge it by."],
    ] as const;
    for (const [fixture, reason, detail] of cases) {
      stub("post", "/api/repos/:repo/ask", 200, fixture);
      const out = await ask("r", "why");
      expect(out.kind).toBe("ok");
      if (out.kind !== "ok" || !out.value.refused) throw new Error("unreachable");
      expect(out.value.reason).toBe(reason);
      expect(out.value.detail).toBe(detail);
    }
  });

  test("an answered response parses to ok with refused false", async () => {
    stub("post", "/api/repos/:repo/ask", 200, askAnswered);
    const out = await ask("r", "why");
    expect(out.kind).toBe("ok");
    if (out.kind !== "ok") throw new Error("unreachable");
    expect(out.value.refused).toBe(false);
    if (out.value.refused) throw new Error("unreachable");
    expect(out.value.answered_by).toBe("extractive");
    expect(out.value.citations.length).toBeGreaterThan(0);
  });
});

describe("what the client sends", () => {
  test("the ask request body has exactly the keys q and limit", async () => {
    const rec = stub("post", "/api/repos/:repo/ask", 200, askAnswered);
    await ask("r", "how does Push work", 3);
    expect(rec.calls).toBe(1);
    expect(keys(rec.bodies[0])).toEqual(["limit", "q"]);
    expect(rec.bodies[0]).toEqual({ q: "how does Push work", limit: 3 });
  });

  test("search and ask never send a mode field", async () => {
    // read.go:452-455 refuses `mode` outright — every value, including the
    // configured one — so a client that sends it turns every query into a 400.
    const s = stub("post", "/api/repos/:repo/search", 200, searchHybrid);
    await search("r", "Push");
    expect(keys(s.bodies[0])).toEqual(["q"]);
    expect(s.bodies[0]).not.toHaveProperty("mode");

    const a = stub("post", "/api/repos/:repo/ask", 200, askAnswered);
    await ask("r", "Push");
    expect(keys(a.bodies[0])).toEqual(["q"]);
    expect(a.bodies[0]).not.toHaveProperty("mode");
  });

  test("an empty ref is sent as absent, not as HEAD", async () => {
    // handler.go:156-159 already substitutes HEAD. Deep equality on the parsed
    // body, because HEAD and absent produce the same job and a response
    // assertion cannot tell them apart.
    const rec = stub("post", "/api/repos", 202, jobPending);
    await submitRepo("https://github.com/rs/zerolog", "   ");
    expect(rec.bodies[0]).toEqual({ remote: "https://github.com/rs/zerolog" });
    expect(rec.bodies[0]).not.toHaveProperty("ref");
  });

  test("a ref the user typed is sent as given", async () => {
    const rec = stub("post", "/api/repos", 202, jobPending);
    await submitRepo("https://github.com/rs/zerolog", " v1.2.3 ");
    expect(rec.bodies[0]).toEqual({ remote: "https://github.com/rs/zerolog", ref: "v1.2.3" });
  });

  test("suffix is sent only when it is asked for, and depth only when it is set", async () => {
    const s = stub("get", "/api/repos/:repo/symbols", 200, { repo_id: "r", count: 0, matched: "exact", truncated: false, symbols: [], staleness: {} });
    await listSymbols("r", { name: "Push" });
    expect(s.urls[0]).toBe("/api/repos/r/symbols?name=Push");
    await listSymbols("r", { name: "Push", suffix: true, limit: 5 });
    expect(s.urls[1]).toBe("/api/repos/r/symbols?name=Push&suffix=true&limit=5");

    const c = stub("get", "/api/repos/:repo/symbols/:symbol/callers", 200, callers);
    await callersOf("r", "s");
    expect(c.urls[0]).toBe("/api/repos/r/symbols/s/callers");
    await callersOf("r", "s", { depth: 2 });
    expect(c.urls[1]).toBe("/api/repos/r/symbols/s/callers?depth=2");
  });
});
