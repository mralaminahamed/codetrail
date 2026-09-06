import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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

// The copy buttons and the folded check. Behaviour, never a class name: what is
// asserted is which values reach the clipboard and whether the check is open,
// both of which a reader can feel.
describe("copying a citation, and where the check is folded", () => {
  test("the digest and the commit each get a copy button whose name says which", async () => {
    const user = userEvent.setup();
    render(<Citation citation={github} />);

    // Named, not counted: two unlabelled buttons on one card are a coin toss
    // for a screen-reader user, and the labels are the whole fix.
    await user.click(screen.getByRole("button", { name: "Copy the digest" }));
    expect(await window.navigator.clipboard.readText()).toBe(github.digest);
    // The confirmation is on the accessible name, because the button has no
    // text of its own — it cannot have any, see Copy.tsx.
    expect(screen.getByRole("button", { name: "Copied the digest" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Copy the commit" }));
    expect(await window.navigator.clipboard.readText()).toBe(github.commit);
    // And the digest button has gone back to idle, so two checkmarks never
    // claim two copies are on the clipboard at once.
    expect(screen.getByRole("button", { name: "Copy the digest" })).toBeInTheDocument();
  });

  test("the check command is copyable as one line, pipes and all", async () => {
    const user = userEvent.setup();
    render(<Citation citation={github} check="open" />);
    await user.click(screen.getByRole("button", { name: "Copy the check command" }));
    const copied = await window.navigator.clipboard.readText();
    expect(copied).toBe(
      `git show ${github.commit}:${github.path} | sed -n '${github.start_line},${github.end_line}p' | head -c -1 | sha256sum`,
    );
    // The one flag a copy must not lose: without it sha256sum hashes one byte
    // more than the digest covers and every check a reader runs fails.
    expect(copied).toContain("| head -c -1 |");
  });

  test("the tuple's values survive the copy buttons sitting inside them", () => {
    // The regression this guards: a copy button with a text label inside the
    // <dd> makes the dd read "abc123…Copy", and the tuple's exactness is the
    // thing every other assertion in this file rests on.
    const { container } = render(<Citation citation={github} />);
    expect(term(container, "Digest")).toBe(github.digest);
    expect(term(container, "Commit")).toBe(github.commit);
  });

  test("the check is shut in a list and open where the citation is the subject", () => {
    const list = render(<Citation citation={github} />);
    expect(list.container.querySelector("details")?.open).toBe(false);
    // Shut, not absent: the command is still in the DOM for find-in-page and
    // the summary is still in the tab order.
    expect(list.container.textContent).toContain("head -c -1");
    list.unmount();

    const subject = render(<Citation citation={github} check="open" />);
    expect(subject.container.querySelector("details")?.open).toBe(true);
  });

  test("a browser with no clipboard says so rather than doing nothing", async () => {
    // navigator.clipboard is undefined outright on an insecure origin in
    // Chrome, and this console has no deployment yet: every hand-run of it so
    // far has been over plain http on localhost.
    const user = userEvent.setup();
    render(<Citation citation={github} />);
    const original = Object.getOwnPropertyDescriptor(window.navigator, "clipboard");
    Object.defineProperty(window.navigator, "clipboard", { value: undefined, configurable: true });
    try {
      await user.click(screen.getByRole("button", { name: "Copy the digest" }));
      expect(
        screen.getByRole("button", {
          name: "Could not copy the digest — this browser refused the clipboard",
        }),
      ).toBeInTheDocument();
    } finally {
      if (original) Object.defineProperty(window.navigator, "clipboard", original);
    }
  });
});
