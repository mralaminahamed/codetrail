import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import Span from "./Span";
import { stub } from "../test/msw";
import span from "../api/fixtures/span.json";
import error404 from "../api/fixtures/error-404-repo.json";

const PATH = "/api/repos/:repo/spans/:span";

function renderSpan() {
  return render(
    <MemoryRouter initialEntries={["/repos/r1/spans/s1"]}>
      <Routes>
        <Route path="/repos/:repo/spans/:span" element={<Span />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("the span reader", () => {
  test("a span page renders the whole span text and its citation", async () => {
    stub("get", PATH, 200, span);
    const { container } = renderSpan();
    // findByText normalises whitespace and cannot match a multi-line span, so
    // the wait is on the location and the claim is the exact textContent below.
    await screen.findByRole("link");

    // The WHOLE text, byte for byte: a span is never truncated because the
    // digest is a claim about all of it (answer.go:61-64), so a reader
    // comparing what is shown against the digest must find they agree.
    const pre = container.querySelector("pre");
    expect(pre?.textContent).toBe(span.span.text);
    expect(container.textContent).toContain(span.citation.digest);
    expect(screen.getByRole("link")).toHaveAttribute("href", span.citation.permalink);
  });

  test("a missing span renders an error rather than an empty page", async () => {
    stub("get", PATH, 404, error404);
    renderSpan();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("codetrail could not read this span.");
    expect(alert.textContent).toContain(error404.error);
  });
});
