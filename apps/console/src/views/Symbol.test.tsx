import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import SymbolView from "./Symbol";
import { stub, stubByQuery } from "../test/msw";
import symbol from "../api/fixtures/symbol.json";
import callers from "../api/fixtures/callers.json";
import depth400 from "../api/fixtures/error-400-depth.json";

const SYMBOL = "/api/repos/:repo/symbols/:symbol";
const CALLERS = "/api/repos/:repo/symbols/:symbol/callers";

function renderSymbol(query = "") {
  return render(
    <MemoryRouter initialEntries={[`/repos/r1/symbols/s1${query}`]}>
      <Routes>
        <Route path="/repos/:repo/symbols/:symbol" element={<SymbolView />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("the definition view", () => {
  test("depth offers 1 to 3 and a server rejection of depth names the bound", async () => {
    const select = () => screen.getByLabelText("Depth");
    stub("get", SYMBOL, 200, symbol);
    stub("get", CALLERS, 200, callers);
    renderSymbol();
    await screen.findByRole("heading", { name: "Callers" });

    const options = within_(select()).map((o) => o.value);
    expect(options).toEqual(["1", "2", "3"]);

    // A hand-edited URL the select cannot produce. The console must NOT clamp
    // it: graph.go:358-361 answers a 400 naming the bound rather than silently
    // serving 5, and a client that clamps re-hides that one layer up.
    // The handler answers 400 only for a depth the server would refuse, so a
    // client that clamps to 5 gets a 200 and never sees the sentence. An
    // unconditional 400 would hand the clamping client the same body and the
    // text assertion would pass under the mutant.
    const rec = stubByQuery(
      "get",
      CALLERS,
      "depth",
      (v) => v !== null && Number(v) > 5,
      { status: 400, body: depth400 },
      { status: 200, body: callers },
    );
    stub("get", SYMBOL, 200, symbol);
    renderSymbol("?depth=40");
    expect(await screen.findByText("depth must be between 1 and 3")).toBeInTheDocument();
    expect(rec.urls[0]).toBe("/api/repos/r1/symbols/s1/callers?depth=40");
  });

  test("changing the depth re-asks at that depth", async () => {
    stub("get", SYMBOL, 200, symbol);
    const rec = stub("get", CALLERS, 200, callers);
    renderSymbol();
    await screen.findByRole("heading", { name: "Callers" });
    // depth is always sent explicitly: it is what the select shows, and the
    // server's default is 1 either way.
    expect(rec.urls[0]).toBe("/api/repos/r1/symbols/s1/callers?depth=1");

    await userEvent.setup().selectOptions(screen.getByLabelText("Depth"), "3");
    await screen.findByRole("heading", { name: "Callers" });
    expect(rec.urls.at(-1)).toBe("/api/repos/r1/symbols/s1/callers?depth=3");
  });

  test("the definition renders its citation and its staleness", async () => {
    stub("get", SYMBOL, 200, symbol);
    stub("get", CALLERS, 200, callers);
    const { container } = renderSymbol();
    await screen.findByRole("heading", { name: "Callers" });
    expect(container.textContent).toContain(symbol.citation.digest);
    expect(container.textContent).toContain(symbol.staleness.note);
  });
});

function within_(select: HTMLElement): HTMLOptionElement[] {
  return [...select.querySelectorAll("option")];
}
