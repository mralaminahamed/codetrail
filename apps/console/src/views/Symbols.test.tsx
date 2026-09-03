import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import { axe } from "jest-axe";
import { MemoryRouter, Route, Routes } from "react-router";
import Symbols from "./Symbols";
import { stub } from "../test/msw";
import symbols from "../api/fixtures/symbols.json";
import symbolsOne from "../api/fixtures/symbols-one.json";
import error410 from "../api/fixtures/error-410-repo.json";
import { stubUnreachable } from "../test/msw";

const PATH = "/api/repos/:repo/symbols";

function renderSymbols(query = "?name=Total&suffix=true") {
  return render(
    <MemoryRouter initialEntries={[`/repos/r1/symbols${query}`]}>
      <Routes>
        <Route path="/repos/:repo/symbols" element={<Symbols />} />
        <Route path="/repos/:repo/symbols/:symbol" element={<h1>Definition</h1>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("the definitions list", () => {
  test("two definitions for one name are both rendered, in the server's order", async () => {
    expect(symbols.symbols).toHaveLength(2);
    stub("get", PATH, 200, symbols);
    renderSymbols();
    // Waited on the list, not on a link by name: both definitions share the
    // name — that is what the fixture is for — so a name query finds two and
    // throws, which is an incident rather than a claim failing.
    const rows = await screen.findAllByRole("listitem");
    expect(rows).toHaveLength(2);
    // The server's order — path then start_line (graph.go's ORDER BY) — and
    // each row's own location, so a mutant that renders rows[0] twice fails.
    expect(rows[0]!.textContent).toContain(
      `${symbols.symbols[0]!.path}:${symbols.symbols[0]!.start_line}-${symbols.symbols[0]!.end_line}`,
    );
    expect(rows[1]!.textContent).toContain(
      `${symbols.symbols[1]!.path}:${symbols.symbols[1]!.start_line}-${symbols.symbols[1]!.end_line}`,
    );
    expect(rows[0]!.textContent).not.toBe(rows[1]!.textContent);
    // And the two packages differ, which the rows must show.
    expect(rows[0]!.textContent).toContain(symbols.symbols[0]!.pkg);
    expect(rows[1]!.textContent).toContain(symbols.symbols[1]!.pkg);
  });

  test("the response says whether the match was exact or by suffix, and the console shows it", async () => {
    stub("get", PATH, 200, symbols);
    const wide = renderSymbols("?name=Total&suffix=true");
    expect(await screen.findByText(/Matched: suffix\./)).toBeInTheDocument();
    expect(symbols.matched).toBe("suffix");
    wide.unmount();

    stub("get", PATH, 200, symbolsOne);
    renderSymbols("?name=Total");
    expect(await screen.findByText(/Matched: exact\./)).toBeInTheDocument();
    expect(symbolsOne.matched).toBe("exact");
  });

  test("the suffix checkbox is a labelled opt-in and is sent only when checked", async () => {
    const rec = stub("get", PATH, 200, symbolsOne);
    renderSymbols("?name=Total");
    await screen.findByText(/Matched: exact\./);
    expect(rec.urls[0]).toBe("/api/repos/r1/symbols?name=Total");
    expect(screen.getByLabelText("also match a method by its last segment")).not.toBeChecked();
  });

  test("a truncated definition list says so", async () => {
    stub("get", PATH, 200, { ...symbols, truncated: true });
    renderSymbols();
    expect(await screen.findByText("This list was truncated; there are more.")).toBeInTheDocument();

    stub("get", PATH, 200, symbols);
    const complete = renderSymbols();
    await screen.findAllByRole("listitem");
    expect(complete.container.textContent).not.toContain("This list was truncated");
  });

  test("an evicted repository renders the server's eviction sentence", async () => {
    stub("get", PATH, 410, error410);
    renderSymbols();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("this repository was indexed and has since been evicted");
  });

  test("an unreachable gateway renders an error and no rows", async () => {
    stubUnreachable("get", PATH);
    renderSymbols();
    await screen.findByRole("alert");
    expect(screen.queryByRole("listitem")).toBeNull();
  });

  test("the definitions view has no axe violations", async () => {
    stub("get", PATH, 200, symbols);
    const { container } = renderSymbols();
    await screen.findAllByRole("listitem");
    expect(await axe(container)).toHaveNoViolations();
  });
});
