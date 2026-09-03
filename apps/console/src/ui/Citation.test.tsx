import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import { axe } from "jest-axe";
import Citation from "./Citation";
import span from "../api/fixtures/span.json";
import codeberg from "../api/fixtures/citation-codeberg.json";
import noLink from "../api/fixtures/citation-nolink.json";
import badScheme from "../api/fixtures/citation-badscheme.json";
import symbolNoSpan from "../api/fixtures/symbol-nospan.json";
import type { Citation as CitationValue, Symbol } from "../api/types";

const github = span.citation as CitationValue;
const forgejo = codeberg.citation as CitationValue;
const unknownForge = noLink.citation as CitationValue;
const notHttps = badScheme.citation as CitationValue;
const spanless = symbolNoSpan.symbol as Symbol;

// The value rendered under its own label in the tuple, not "this string appears
// somewhere in the tree". The digest and the line range each render twice — in
// the tuple and in the runnable check — so getByText finds two and throws,
// which is an incident rather than a claim failing. Reading the <dd> beside a
// named <dt> is also strictly more discriminating: a mutant that transposed the
// commit and the digest fails this and would pass getByText.
function term(container: HTMLElement, label: string): string {
  const dt = [...container.querySelectorAll("dt")].find((e) => e.textContent === label);
  return dt?.nextElementSibling?.textContent ?? "";
}

describe("the citation", () => {
  test("a github citation links to exactly the permalink the server sent", () => {
    render(<Citation citation={github} />);
    const link = screen.getByRole("link");
    // The exact href, not "a link exists": a composer produces a
    // byte-identical string for GitHub, which is why this fixture alone proves
    // nothing and the codeberg one below is required.
    expect(link).toHaveAttribute(
      "href",
      `https://github.com/codetrail-live/zerolog/blob/${github.commit}/calc/calc.go#L19-L23`,
    );
    expect(link).toHaveAttribute("href", github.permalink);
  });

  test("a codeberg citation links to exactly the permalink the server sent", () => {
    render(<Citation citation={forgejo} />);
    const link = screen.getByRole("link");
    // Codeberg runs Forgejo: /src/commit/<sha>/, not /blob/<sha>/ (cite.go:151).
    // A client-side composer gets this wrong and only this fixture can see it.
    expect(link).toHaveAttribute("href", forgejo.permalink);
    expect(link.getAttribute("href")).toContain("/src/commit/");
    expect(link.getAttribute("href")).not.toContain("/blob/");
  });

  test("a citation with no permalink renders the whole tuple, no link, and says why", () => {
    const { container } = render(<Citation citation={unknownForge} />);
    // No <a> at all. An <a href=""> reloads the console and reads as "the code
    // is gone" rather than "we do not know this forge's shape".
    expect(screen.queryByRole("link")).toBeNull();
    expect(container.querySelector("a")).toBeNull();

    expect(
      screen.getByText(/codetrail does not know this forge's URL shape, so there is no link\./),
    ).toBeInTheDocument();

    // And all four tuple values, each under its own label, as exact strings:
    // hiding the claim when the convenience is missing deletes the one thing
    // this product asserts.
    expect(term(container, "Repository")).toBe(unknownForge.remote);
    expect(term(container, "Commit")).toBe(unknownForge.commit);
    expect(term(container, "Location")).toBe("calc/calc.go:19-23");
    expect(term(container, "Digest")).toBe(unknownForge.digest);
  });

  test("every citation renders repo, commit, path, line range and digest", () => {
    for (const c of [github, forgejo, unknownForge]) {
      const { container, unmount } = render(<Citation citation={c} />);
      expect(term(container, "Repository")).toBe(c.remote);
      expect(term(container, "Commit")).toBe(c.commit);
      expect(term(container, "Location")).toBe(`${c.path}:${c.start_line}-${c.end_line}`);
      expect(term(container, "Digest")).toBe(c.digest);
      unmount();
    }
  });

  test("a null citation renders the definition's own location and says there is no digest", () => {
    render(<Citation citation={null} symbol={spanless} />);
    // The exact location string, which only the location rendering produces.
    expect(screen.getByText("big.go:2-208")).toBeInTheDocument();
    expect(
      screen.getByText("This declaration has no span of its own, so there is no digest to check it against."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("link")).toBeNull();
  });

  test("a permalink that is not https is not rendered as a link", () => {
    const { container } = render(<Citation citation={notHttps} />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(container.innerHTML).not.toContain("javascript:");
    // The tuple still renders: the guard removes the link, not the claim.
    expect(term(container, "Digest")).toBe(notHttps.digest);
  });

  test("the staleness note travels with the citation and is not paraphrased", () => {
    const { container } = render(<Citation citation={github} />);
    expect(container.textContent).toContain(
      "codetrail has not checked whether main has moved since.",
    );
    expect(container.textContent).not.toMatch(
      /may be out of date|possibly stale|might have changed|last updated/i,
    );
  });

  test("the citation has no axe violations in all three renderings", async () => {
    for (const el of [
      <Citation key="a" citation={github} />,
      <Citation key="b" citation={unknownForge} />,
      <Citation key="c" citation={null} symbol={spanless} />,
    ]) {
      const { container, unmount } = render(el);
      expect(await axe(container)).toHaveNoViolations();
      unmount();
    }
  });
});
