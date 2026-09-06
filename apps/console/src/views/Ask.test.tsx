import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "jest-axe";
import { MemoryRouter, Route, Routes } from "react-router";
import Ask from "./Ask";
import { stub, stubSlow } from "../test/msw";
import answered from "../api/fixtures/ask-answered.json";
import answeredEmpty from "../api/fixtures/ask-answered-empty.json";
import answeredLLM from "../api/fixtures/ask-answered-llm.json";
import answeredDegraded from "../api/fixtures/ask-answered-degraded.json";
import refusedDegraded from "../api/fixtures/ask-refused-degraded.json";
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
      // The SPAN links specifically. The repository header now also links to
      // the symbol graph, which is an in-app /repos/ link and not a hit.
      .filter((a) => a.getAttribute("href")?.includes("/spans/"))
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

describe("what the page shows while it is still working, and what it stops showing", () => {
  test("pressing Ask produces an observable change before any answer arrives", async () => {
    // The audit's sentence: inFlight only disabled two buttons that had no
    // visible affordance to lose, so an embedding and a vector search ran with
    // nothing on screen saying so.
    stubSlow("post", ASK, 200, answered);
    renderAsk();
    await askQuestion();
    const pending = screen.getByRole("status", { name: "Request progress" });
    expect(pending.textContent).toContain("embedding the question");
    // Announced rather than merely drawn: the region is not aria-busy, because
    // aria-busy would tell a screen reader to DEFER exactly this sentence.
    expect(screen.getByRole("region", { name: "Result" })).not.toHaveAttribute("aria-busy", "true");
    await screen.findByRole("heading", { name: "Answer" });
    expect(screen.queryByRole("status", { name: "Request progress" })).toBeNull();
  });

  test("a second question never renders the first question's citations beside it", async () => {
    const user = userEvent.setup();
    stub("post", ASK, 200, answered);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    const firstDigest = answered.citations[0]!.citation.digest;
    expect(container.textContent).toContain(firstDigest);

    // setResult ran AFTER the await, so the first question's answer — and its
    // citations — stayed on screen beside the second question for as long as
    // the second embedding and vector search took. For a product whose claim is
    // a checkable citation, showing one that answers a different question is
    // the worst available stale state.
    stubSlow("post", ASK, 200, noSpans);
    await user.clear(screen.getByLabelText("Question"));
    await user.type(screen.getByLabelText("Question"), "an entirely different question");
    await user.click(screen.getByRole("button", { name: "Ask" }));

    expect(screen.getByRole("status", { name: "Request progress" })).toBeInTheDocument();
    expect(container.textContent).not.toContain(firstDigest);
    expect(screen.queryByRole("heading", { name: "Answer" })).toBeNull();

    await screen.findByRole("status", { name: "Answer outcome" });
  });

  test("a repository the console cannot read says so, instead of the header silently vanishing", async () => {
    // getRepo's outcome was thrown away unless it was ok, so a 410 made the
    // whole header disappear and the server's eviction sentence — which the
    // console had already fetched and parsed — went on the floor.
    stub("get", "/api/repos/:repo", 410, error410);
    render(
      <MemoryRouter initialEntries={["/repos/r1"]}>
        <Routes>
          <Route path="/repos/:repo" element={<Ask />} />
        </Routes>
      </MemoryRouter>,
    );
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("this repository was indexed and has since been evicted");
    // And the question form is still there: the repository facts failing does
    // not mean the ask endpoint will.
    expect(screen.getByLabelText("Question")).toBeInTheDocument();
  });

  test("the repository's facts offer a way into the symbol graph", async () => {
    // routes.tsx has registered /repos/:repo/symbols since P4 and nothing in
    // the tree linked to it: seven Links, none reaching the graph.
    stub("post", ASK, 200, answered);
    renderAsk();
    const link = await screen.findByRole("link", { name: "Find a definition and who calls it" });
    expect(link).toHaveAttribute("href", `/repos/${repoDetail.id}/symbols`);
  });

  test("the browser tab names the repository once its facts are read", async () => {
    stub("post", ASK, 200, answered);
    renderAsk();
    await screen.findByText(repoDetail.staleness.note);
    // PageTitle has taken a `title` prop since P5 and no caller passed one, so
    // every tab read the same string for the life of the view.
    expect(document.title).toBe(`${repoDetail.remote.replace("https://", "")} — codetrail`);
  });
});

describe("the two fields the console ignored for a phase", () => {
  // The pair the whole defect rests on. Measured, not assumed: strip `degraded`
  // and `llm` from the degraded fixture and it is BYTE-IDENTICAL to the plain
  // extractive one — same answered_by, same prose, same three citations, same
  // top_score. read.go stamps answered_by "extractive" on a degraded answer, so
  // those two fields are the only difference there is.
  test("the degraded fixture differs from the plain one in exactly those two keys", () => {
    const strip = (o: Record<string, unknown>) => {
      const c = { ...o };
      delete c["degraded"];
      delete c["llm"];
      return JSON.stringify(c);
    };
    expect(strip(answeredDegraded)).toBe(strip(answered));
    expect(answeredDegraded.answered_by).toBe("extractive");
    expect(answered).not.toHaveProperty("degraded");
  });

  test("a degraded answer does not render identically to a plain extractive one", async () => {
    stub("post", ASK, 200, answered);
    const plain = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    const plainText = plain.container.textContent;
    expect(plainText).not.toMatch(/fallback/i);
    plain.unmount();

    stub("post", ASK, 200, answeredDegraded);
    const degraded = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });

    // The claim: two payloads that differ only in `degraded` and `llm` must not
    // produce the same page. Before this, they did.
    expect(degraded.container.textContent).not.toBe(plainText);
    const panel = screen.getByRole("status", { name: "Answer provenance" });
    expect(panel.textContent).toContain("This answer is the fallback, not the model's.");
    // The reason as the server spelled it, never a gloss of this console's own:
    // the stop set is closed server-side and a lookup table here would render a
    // tenth reason as nothing.
    expect(panel.textContent).toContain(answeredDegraded.degraded.reason);
    expect(panel.textContent).toContain(answeredDegraded.degraded.from);
    // And the answer itself is still rendered: a degradation is a fact about
    // how, not about whether.
    expect(degraded.container.textContent).toContain(answered.answer.slice(0, 40));
  });

  test("an answer the model wrote says so and carries its trace, with no degradation", async () => {
    stub("post", ASK, 200, answeredLLM);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    expect(screen.getByText("llm")).toBeInTheDocument();
    // `degraded` is present IFF the loop was attempted and did NOT write the
    // answer, so a panel here would be the console inventing a downgrade.
    expect(screen.queryByRole("status", { name: "Answer provenance" })).toBeNull();
    expect(container.textContent).not.toMatch(/fallback/i);
    // The trace still renders, because the loop ran.
    expect(container.textContent).toContain(answeredLLM.llm.model);
    expect(container.textContent).toContain(answeredLLM.llm.tools[0]!.name);
  });

  test("a refusal that degraded first renders both the degradation and the floor", async () => {
    // read.go:443-448 — the degraded loop does not inherit the floor's
    // permission to be ignored. Both facts are on the same payload and the
    // console renders both, in two panels: the degradation is not smuggled
    // into Refusal, whose contract is the floor and no request id.
    stub("post", ASK, 200, refusedDegraded);
    const { container } = renderAsk();
    await askQuestion();
    const refusal = await screen.findByRole("status", { name: "Answer outcome" });
    const provenance = screen.getByRole("status", { name: "Answer provenance" });
    expect(refusal.textContent).toContain("has never been calibrated");
    expect(refusal.textContent).not.toContain(refusedDegraded.degraded.reason);
    expect(provenance.textContent).toContain(refusedDegraded.degraded.reason);
    expect(provenance.textContent).not.toContain("has never been calibrated");
    expect(container.textContent).toContain(refusedDegraded.reason);
  });

  test("a trace with no tool calls survives its null tools list", async () => {
    // `tools` serialises as null when the loop made no tool call, which a
    // provider failure on turn one always does — the commonest degradation
    // there is. A .map on null throws and unmounts the tree.
    expect(answeredDegraded.llm.tools).toBeNull();
    stub("post", ASK, 200, answeredDegraded);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    expect(container.textContent).toContain("none");
    expect(container.textContent).toContain(answeredDegraded.llm.stop);
  });

  test("a deployment with no model renders no provenance block at all", async () => {
    // The shipped default sends neither field, and an unconfigured console must
    // look exactly as it did.
    stub("post", ASK, 200, answered);
    const { container } = renderAsk();
    await askQuestion();
    await screen.findByRole("heading", { name: "Answer" });
    expect(screen.queryByRole("status", { name: "Answer provenance" })).toBeNull();
    expect(container.textContent).not.toContain("What the model did");
  });
});
