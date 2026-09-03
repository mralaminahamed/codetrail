import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import { axe } from "jest-axe";
import { MemoryRouter, Route, Routes } from "react-router";
import Corpus from "./Corpus";
import { stub, stubUnreachable } from "../test/msw";
import repos from "../api/fixtures/repos.json";
import error500 from "../api/fixtures/error-500.json";

const PATH = "/api/repos";

function renderCorpus() {
  return render(
    <MemoryRouter initialEntries={["/repos"]}>
      <Routes>
        <Route path="/repos" element={<Corpus />} />
        <Route path="/repos/:repo" element={<h1>Ask this repository</h1>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("the corpus listing", () => {
  test("repositories render in the order the server sent them, and are not sorted", async () => {
    const want = repos.repos.map((r) => r.remote);
    // The fixture is emitted so that server order, alphabetical order and
    // insertion order are three different orders (see the Go emitter's
    // assertReposOrder). Without that, a client that sorts is invisible.
    expect([...want].sort()).not.toEqual(want);

    stub("get", PATH, 200, repos);
    renderCorpus();
    const items = await screen.findAllByRole("listitem");
    expect(items.map((li) => li.querySelector("a")?.textContent)).toEqual(want);
  });

  test("every row names its ref and its commit, so two commits of one repo are distinguishable", async () => {
    stub("get", PATH, 200, repos);
    renderCorpus();
    const items = await screen.findAllByRole("listitem");
    items.forEach((li, i) => {
      const row = repos.repos[i]!;
      expect(li.textContent).toContain(row.ref);
      expect(li.textContent).toContain(row.commit);
      expect(li.querySelector("a")).toHaveAttribute("href", `/repos/${row.id}`);
    });
  });

  test("the count the server sent is stated, not recomputed", async () => {
    stub("get", PATH, 200, repos);
    renderCorpus();
    expect(
      await screen.findByText(`${repos.count} repositories, most recently used first.`),
    ).toBeInTheDocument();
  });

  test("a 500 renders the error panel with its request id", async () => {
    stub("get", PATH, 500, error500);
    renderCorpus();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("codetrail could not list the corpus.");
    expect(alert.textContent).toContain(error500.request_id);
  });

  test("an unreachable gateway renders an error and no request id", async () => {
    stubUnreachable("get", PATH);
    renderCorpus();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("codetrail did not return a request id for this failure.");
    expect(screen.queryByRole("listitem")).toBeNull();
  });

  test("the corpus listing has no axe violations", async () => {
    stub("get", PATH, 200, repos);
    const { container } = renderCorpus();
    await screen.findAllByRole("listitem");
    expect(await axe(container)).toHaveNoViolations();
  });
});
