import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "jest-axe";
import { MemoryRouter, Route, Routes } from "react-router";
import Ask from "./Ask";
import { stub } from "../test/msw";
import answered from "../api/fixtures/ask-answered.json";
import answeredEmpty from "../api/fixtures/ask-answered-empty.json";
import noSpans from "../api/fixtures/ask-refused-no-spans.json";
import noSpansLexical from "../api/fixtures/ask-refused-no-spans-lexical.json";
import belowFloor from "../api/fixtures/ask-refused-below-floor.json";
import unscored from "../api/fixtures/ask-refused-unscored.json";
import searchHybrid from "../api/fixtures/search-hybrid.json";
import searchLexical from "../api/fixtures/search-lexical.json";
import error500 from "../api/fixtures/error-500.json";
import repoDetail from "../api/fixtures/repo.json";
import error410 from "../api/fixtures/error-410-repo.json";
import error404 from "../api/fixtures/error-404-repo.json";
import { stubUnreachable } from "../test/msw";

const ASK = "/api/repos/:repo/ask";
const SEARCH = "/api/repos/:repo/search";

// The three sentences detail() writes (read.go:386-398). Every refusal test
// asserts its own present and the other two absent: a mutant that renders a
// single "no answer" string passes a one-reason fixture.
const REASONS = [noSpans.detail, belowFloor.detail, unscored.detail];

function renderAsk() {
  // The repo page reads its own detail on mount. Wired in every render,
  // because MSW is set to error on an unhandled request and 17 of them were
  // being swallowed as `unreachable` while the tests still passed — an error
  // branch behind a fake nobody had wired.
  stub("get", "/api/repos/:repo", 200, repoDetail);
  return render(
    <MemoryRouter initialEntries={["/repos/r1"]}>
      <Routes>
        <Route path="/repos/:repo" element={<Ask />} />
        <Route path="/repos/:repo/spans/:span" element={<h1>Span</h1>} />
      </Routes>
    </MemoryRouter>,
  );
}

async function askQuestion(op: "Ask" | "Search" = "Ask") {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText("Question"), "how does Push work");
  await user.click(screen.getByRole("button", { name: op }));
}

function assertOnlyReason(container: HTMLElement, want: string) {
  expect(container.textContent).toContain(want);
  for (const other of REASONS) {
    if (other === want) continue;
    expect(container.textContent).not.toContain(other);
  }
}

describe("asking", () => {
  test("an answered response renders the answer, its markers and its citations", async () => {
    stub("post", ASK, 200, answered);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    expect(container.textContent).toContain(answered.answer.slice(0, 40));
    const rows = screen.getAllByRole("listitem");
    expect(rows.length).toBe(answered.citations.length);
    // The marker ORDER, not just membership: the markers point into the answer
    // text, so a reordered citation list makes every marker cite the wrong
    // span. Sorting by span id reorders this fixture, which is why the order
    // is asserted as a sequence rather than as a set.
    expect(rows.map((li) => li.textContent?.match(/\[(\d+)\]/)?.[1])).toEqual(
      answered.citations.map((c) => String(c.marker)),
    );
    expect(rows.map((li) => (li.textContent?.includes(answered.citations[0]!.citation.digest) ? 0 : 1))[0]).toBe(0);
    for (const c of answered.citations) {
      expect(container.textContent).toContain(c.citation.digest);
    }
  });

  test("an answered response names what answered it", async () => {
    stub("post", ASK, 200, answered);
    renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    // Rendered even though there is one value until P7: read.go:66-70 says the
    // field exists so a client can notice a silent downgrade.
    expect(screen.getByText("extractive")).toBeInTheDocument();
  });

  test("an answer that dropped spans says how many", async () => {
    stub("post", ASK, 200, answeredEmpty);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    expect(container.textContent).toContain(
      `${answeredEmpty.dropped} more ranked spans did not fit the answer's budget.`,
    );
  });

  test("an answered response with no citations renders the answer and says there were none", async () => {
    // refused is false and answer is "". A console that branched on the answer
    // renders a refusal here, with reason undefined.
    stub("post", ASK, 200, answeredEmpty);
    const { container } = renderAsk();
    await askQuestion();
    expect(await screen.findByText("codetrail assembled no citations for this answer.")).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "Answer outcome" })).toBeNull();
    expect(container.textContent).not.toContain("No answer — and no error.");
  });

  test("a no_spans refusal renders its own sentence and neither of the other two", async () => {
    stub("post", ASK, 200, noSpans);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("status", { name: "Answer outcome" });
    assertOnlyReason(container, noSpans.detail);
    expect(screen.getByText("no_spans")).toBeInTheDocument();
  });

  test("a below_floor refusal renders its own sentence and neither of the other two", async () => {
    stub("post", ASK, 200, belowFloor);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("status", { name: "Answer outcome" });
    assertOnlyReason(container, belowFloor.detail);
  });

  test("an unscored refusal renders its own sentence and neither of the other two", async () => {
    stub("post", ASK, 200, unscored);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("status", { name: "Answer outcome" });
    assertOnlyReason(container, unscored.detail);
  });

  test("a refusal is a status, not an alert, and carries no request id", async () => {
    stub("post", ASK, 200, noSpans);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("status", { name: "Answer outcome" });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(container.textContent).not.toContain(error500.request_id);
    expect(container.textContent).not.toContain("codetrail could not answer.");
    // The floor block is the refusal's own evidence, and ErrorPanel has none.
    expect(container.textContent).toContain("has never been calibrated");
  });

  test("a lexical-only refusal says no floor was applied", async () => {
    stub("post", ASK, 200, noSpansLexical);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("status", { name: "Answer outcome" });
    expect(container.textContent).toContain(
      "This deployment retrieves in lexical mode, which produces no similarity score, so no floor was applied.",
    );
    expect(container.textContent).not.toContain("has never been calibrated");
  });

  test("a failed ask is an alert, carries the request id, and shows no floor", async () => {
    stub("post", ASK, 500, error500);
    const { container } = renderAsk();
    await askQuestion();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("codetrail could not answer.");
    expect(container.textContent).toContain(error500.request_id);
    // Neither the refusal's role nor its evidence.
    expect(screen.queryByRole("status", { name: "Answer outcome" })).toBeNull();
    expect(container.textContent).not.toContain("has never been calibrated");
    for (const r of REASONS) expect(container.textContent).not.toContain(r);
  });
});

describe("the outcomes a question can end in", () => {
  test("an evicted repository is an error that says it was evicted, not a refusal", async () => {
    stub("post", ASK, 410, error410);
    const { container } = renderAsk();
    await askQuestion();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("this repository was indexed and has since been evicted");
    // Not a refusal: no status region, no floor, no reason.
    expect(screen.queryByRole("status", { name: "Answer outcome" })).toBeNull();
    expect(container.textContent).not.toContain("has never been calibrated");
    for (const r of REASONS) expect(container.textContent).not.toContain(r);
  });

  test("an unknown repository is an error that says so, and is not a refusal", async () => {
    stub("post", ASK, 404, error404);
    const { container } = renderAsk();
    await askQuestion();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("no such repository");
    expect(screen.queryByRole("status", { name: "Answer outcome" })).toBeNull();
    expect(container.textContent).not.toContain("has never been calibrated");
  });

  test("an unreachable gateway is an error with no request id, and is not a refusal", async () => {
    stubUnreachable("post", ASK);
    const { container } = renderAsk();
    await askQuestion();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("codetrail could not be reached.");
    expect(alert.textContent).toContain("codetrail did not return a request id for this failure.");
    expect(screen.queryByRole("status", { name: "Answer outcome" })).toBeNull();
    expect(container.textContent).not.toContain("has never been calibrated");
  });

  // What the region CONTAINED at the instant it received focus.
  //
  // The test this replaces asserted document.activeElement afterwards, and
  // passed under both the broken and the fixed version: focus() was called on
  // the line after setResult, React 19 batched that update into a promise
  // continuation, so focus landed on an EMPTY div and the answer was inserted
  // silently afterwards — and the element focused is the same element either
  // way. Only the content at the moment of focus tells the two apart.
  function contentAtFocus(): { read: () => string | null; stop: () => void } {
    let seen: string | null = null;
    const on = (e: FocusEvent) => {
      const el = e.target as HTMLElement;
      // The result region, not the button the click focused: only the region
      // carries tabindex="-1".
      if (seen === null && el.getAttribute?.("tabindex") === "-1") seen = el.textContent ?? "";
    };
    document.addEventListener("focusin", on);
    return { read: () => seen, stop: () => document.removeEventListener("focusin", on) };
  }

  test("an answered result is IN the region before focus reaches it, and the region has a name", async () => {
    stub("post", ASK, 200, answered);
    renderAsk();
    const watch = contentAtFocus();
    try {
      await askQuestion();
      await screen.findByRole("heading", { name: "Answer" });
      const region = screen.getByRole("region", { name: "Result" });
      expect(document.activeElement).toBe(region);
      // Under the broken version this is "". An unnamed, empty div taking
      // focus announces nothing, and the answered path had no role="status"
      // and no role="alert" to announce it afterwards either — so "it worked"
      // was the one outcome a screen-reader user was never told about.
      expect(watch.read()).not.toBe("");
      expect(watch.read()).toContain(answered.answer.slice(0, 30));
    } finally {
      watch.stop();
    }
  });

  test("a refused result is also in the region before focus reaches it", async () => {
    stub("post", ASK, 200, noSpans);
    renderAsk();
    const watch = contentAtFocus();
    try {
      await askQuestion();
      await screen.findByRole("status", { name: "Answer outcome" });
      expect(document.activeElement).toBe(screen.getByRole("region", { name: "Result" }));
      expect(watch.read()).toContain(noSpans.detail);
    } finally {
      watch.stop();
    }
  });
});

describe("the repository's own facts", () => {
  test("the graph counts are four numbers and a sentence, never a percentage badge", async () => {
    stub("post", ASK, 200, answered);
    const { container } = renderAsk();
    await screen.findByText(
      `${repoDetail.symbols} definitions, ${repoDetail.edges} call edges: ${repoDetail.edges_resolved} resolved and ${repoDetail.edges_syntactic} syntactic.`,
    );
    expect(container.textContent).toContain(
      `${repoDetail.edges_resolved} of ${repoDetail.edges} call edges name a definition in this repository; the rest name something codetrail could not resolve.`,
    );
    // P4 Open question 13: the aggregate is a per-repo fact and provenance is
    // a per-row label, so no ratio, no percentage and no badge.
    expect(container.textContent).not.toMatch(/\d+%/);
    expect(container.textContent).not.toMatch(/\d+\s*\/\s*\d+/);
  });

  test("the repository's staleness sentence is the server's", async () => {
    stub("post", ASK, 200, answered);
    const { container } = renderAsk();
    await screen.findByText(repoDetail.staleness.note);
    expect(container.textContent).not.toMatch(
      /may be out of date|possibly stale|might have changed|last updated/i,
    );
  });
});

describe("searching", () => {
  test("a null top_score renders as no similarity score, never as zero", async () => {
    stub("post", SEARCH, 200, searchLexical);
    const { container } = renderAsk();
    await askQuestion("Search");
    await screen.findByRole("heading", { name: "Ranked spans" });
    expect(container.textContent).toContain("Best cosine similarity: no similarity score.");
    expect(container.textContent).not.toContain("Best cosine similarity: 0");
  });

  test("a hit whose vector rank is zero shows an em dash for that arm", async () => {
    stub("post", SEARCH, 200, searchHybrid);
    const { container } = renderAsk();
    await askQuestion("Search");
    await screen.findByRole("heading", { name: "Ranked spans" });

    const rows = screen.getAllByRole("listitem");
    const scored = searchHybrid.hits.findIndex((h) => h.vector_rank > 0);
    const unscoredIdx = searchHybrid.hits.findIndex((h) => h.vector_rank === 0);
    expect(scored).toBeGreaterThanOrEqual(0);
    expect(unscoredIdx).toBeGreaterThanOrEqual(0);

    // The row the vector arm missed: an em dash for the rank and the words for
    // the score, never a 0 for either.
    expect(rows[unscoredIdx]!.textContent).toContain("Vector rank —");
    expect(rows[unscoredIdx]!.textContent).toContain("Cosine similarity: no similarity score.");
    expect(rows[unscoredIdx]!.textContent).not.toContain("Vector rank 0");
    // And the row it returned still shows its real number, so a mutant that
    // dashes everything fails too.
    expect(rows[scored]!.textContent).toContain(`Vector rank ${searchHybrid.hits[scored]!.vector_rank}`);
    expect(container.textContent).toContain("lexical rank —");
  });

  test("search renders hits in the order the server sent them", async () => {
    stub("post", SEARCH, 200, searchHybrid);
    renderAsk();
    await askQuestion("Search");
    await screen.findByRole("heading", { name: "Ranked spans" });
    // Only the in-app span links: each row also renders its citation, whose
    // permalink is an absolute forge URL, and a bare getAllByRole("link")
    // interleaves the two.
    const links = screen
      .getAllByRole("link")
      .filter((a) => a.getAttribute("href")?.startsWith("/repos/"))
      .map((a) => a.textContent);
    const want = searchHybrid.hits.map((h) => `${h.path}:${h.start_line}-${h.end_line}`);
    expect(links).toEqual(want);
    // Server order is fused rank order and is not alphabetical, so a console
    // that sorts is visible here.
    expect([...want].sort()).not.toEqual(want);
  });

  test("search never renders a floor", async () => {
    stub("post", SEARCH, 200, searchHybrid);
    const { container } = renderAsk();
    await askQuestion("Search");
    await screen.findByRole("heading", { name: "Ranked spans" });
    // read.go:313-314: reporting a floor here would imply a filter that did
    // not run.
    expect(container.textContent).not.toMatch(/floor/i);
  });

  test("ask has no axe violations in answered, refused and failed states", async () => {
    for (const [status, fixture] of [
      [200, answered],
      [200, noSpans],
      [500, error500],
    ] as const) {
      stub("post", ASK, status, fixture);
      const { container, unmount } = renderAsk();
      await askQuestion();
      await screen.findByRole(status === 500 ? "alert" : "heading", status === 500 ? {} : { level: 2 });
      expect(await axe(container)).toHaveNoViolations();
      unmount();
    }
  });
});
