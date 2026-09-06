import { describe, expect, test } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { axe } from "jest-axe";
import { MemoryRouter } from "react-router";
import CallersList from "./Callers";
import { parseCallers } from "../api/parse";
import callersFixture from "../api/fixtures/callers.json";
import approxEmpty from "../api/fixtures/callers-approx-empty.json";
import approxFailed from "../api/fixtures/callers-approx-failed.json";

const callers = parseCallers(callersFixture)!;
const empty = parseCallers(approxEmpty)!;
const failed = parseCallers(approxFailed)!;

function renderCallers(v = callers) {
  return render(
    <MemoryRouter>
      <CallersList callers={v} />
    </MemoryRouter>,
  );
}

function section(name: string) {
  return screen.getByRole("heading", { name }).closest("section") as HTMLElement;
}

describe("the caller lists", () => {
  test("precise callers and approximate callers are two lists with two headings", () => {
    renderCallers();
    expect(screen.getByRole("heading", { name: "Callers" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Name-matched callers" })).toBeInTheDocument();
    // Two <ul>s, not one.
    expect(within(section("Callers")).getAllByRole("list")).toHaveLength(1);
    expect(within(section("Name-matched callers")).getAllByRole("list")).toHaveLength(1);
  });

  test("a symbol appearing in both lists appears once in each, never once in a merged list", () => {
    // The fixture's precise list and approximate list share the symbol name
    // Total. A fixture with disjoint names cannot see a merge: the merged list
    // would just be longer, and a length assertion passes under a mutant that
    // dropped a row instead.
    const shared = "Total";
    expect(callers.callers.some((c) => c.symbol.name === shared)).toBe(true);
    expect(callers.approximate.callers.some((c) => c.symbol.name === shared)).toBe(true);

    renderCallers();
    const precise = within(section("Callers")).getAllByRole("listitem");
    const approximate = within(section("Name-matched callers")).getAllByRole("listitem");
    expect(precise.filter((li) => li.textContent?.includes(shared))).toHaveLength(1);
    expect(approximate.filter((li) => li.textContent?.includes(shared))).toHaveLength(1);
    expect(precise).toHaveLength(callers.callers.length);
    expect(approximate).toHaveLength(callers.approximate.callers.length);
  });

  test("every caller row shows its call site as path and line", () => {
    renderCallers();
    for (const c of callers.callers) {
      expect(screen.getAllByText(`${c.call.path}:${c.call.line}`).length).toBeGreaterThan(0);
    }
  });

  test("a caller row with a null citation still renders its location", () => {
    const without = callers.callers.find((c) => c.citation === null);
    expect(without).toBeDefined();
    renderCallers();
    const row = within(section("Callers"))
      .getAllByRole("listitem")
      .find((li) => li.textContent?.includes(without!.symbol.name))!;
    // Its own location off the symbol row, and the sentence saying there is no
    // digest — not a dropped row and not an empty digest.
    expect(row.textContent).toContain(
      `${without!.symbol.path}:${without!.symbol.start_line}-${without!.symbol.end_line}`,
    );
    expect(row.textContent).toContain("no digest to check it against");
  });

  test("provenance is rendered as a word on every row", () => {
    renderCallers();
    const precise = within(section("Callers")).getAllByRole("listitem");
    for (const li of precise) expect(li.textContent).toContain("resolved");
    const approximate = within(section("Name-matched callers")).getAllByRole("listitem");
    for (const li of approximate) expect(li.textContent).toContain("syntactic");
    // And not hidden in a title attribute, which getByText does not match and
    // a keyboard user cannot reach.
    expect(document.querySelector("[title]")).toBeNull();
  });

  test("the approximate heading says what the match was", () => {
    renderCallers();
    const block = section("Name-matched callers");
    expect(block.textContent).toContain(
      `${callers.approximate.count} definitions call something spelled`,
    );
    expect(block.textContent).toContain("codetrail cannot say whether they call this one.");
    expect(block.textContent).toContain(`Matched on: ${callers.approximate.matched_on}.`);
  });

  test("a failed approximate query says the list could not be built and the precise list stands", () => {
    renderCallers(failed);
    expect(
      screen.getByText("The name-matched list could not be built. The precise callers above are unaffected."),
    ).toBeInTheDocument();
    // The precise answer is untouched.
    expect(within(section("Callers")).getAllByRole("listitem")).toHaveLength(failed.callers.length);
    expect(failed.callers.length).toBeGreaterThan(0);
  });

  test("an empty approximate list and a failed approximate query read differently", () => {
    // The two fixtures differ only in `failed`. Either alone would make both
    // renderings look correct.
    expect(approxEmpty.approximate.failed).toBe(false);
    expect(approxFailed.approximate.failed).toBe(true);
    expect(approxEmpty.approximate.callers).toHaveLength(0);
    expect(approxFailed.approximate.callers).toHaveLength(0);

    const a = renderCallers(empty);
    expect(a.container.textContent).toContain("Nothing in this repository calls a name spelled that way.");
    expect(a.container.textContent).not.toContain("could not be built");
    a.unmount();

    const b = renderCallers(failed);
    expect(b.container.textContent).toContain("The name-matched list could not be built.");
    expect(b.container.textContent).not.toContain("Nothing in this repository calls a name spelled that way.");
  });

  test("the callers list is a list, not a tree", () => {
    const { container } = renderCallers();
    expect(screen.queryByRole("tree")).toBeNull();
    expect(screen.queryAllByRole("treeitem")).toHaveLength(0);
    expect(container.querySelectorAll("ul").length).toBeGreaterThan(0);
  });

  test("the graph views have no axe violations", async () => {
    for (const v of [callers, empty, failed]) {
      const { container, unmount } = renderCallers(v);
      expect(await axe(container)).toHaveNoViolations();
      unmount();
    }
  });

  test("a caller row links to its own definition, in both lists", () => {
    // repo_id and symbol.id were both in hand and the name was rendered as
    // plain text, which made every caller row a dead end: the graph could be
    // walked one hop and no further.
    renderCallers();
    for (const c of callers.callers) {
      const link = screen.getAllByRole("link", { name: c.symbol.name })[0]!;
      expect(link).toHaveAttribute("href", `/repos/${callers.repo_id}/symbols/${c.symbol.id}`);
    }
    // Only the in-app links: every row also renders a citation whose permalink
    // is an absolute forge URL, and a bare getAllByRole("link") interleaves the
    // two.
    const approximate = within(section("Name-matched callers"))
      .getAllByRole("link")
      .filter((a) => a.getAttribute("href")?.startsWith("/repos/"));
    for (const c of callers.approximate.callers) {
      expect(approximate.map((a) => a.getAttribute("href"))).toContain(
        `/repos/${callers.repo_id}/symbols/${c.symbol.id}`,
      );
    }
    expect(approximate.length).toBe(callers.approximate.callers.length);
  });
});
