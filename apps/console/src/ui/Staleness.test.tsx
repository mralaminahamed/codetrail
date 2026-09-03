import { describe, expect, test } from "vitest";
import { render } from "@testing-library/react";
import Staleness from "./Staleness";
import repo from "../api/fixtures/repo.json";
import repoStale from "../api/fixtures/repo-stale.json";
import type { Staleness as StalenessValue } from "../api/types";

const unknown = repo.staleness as StalenessValue;
const superseded = repoStale.staleness as StalenessValue;

// Every paraphrase a client might compose instead of rendering the server's
// sentence. cite.go writes a negative claim about codetrail's own knowledge;
// each of these is a claim about the code that nobody made.
const PARAPHRASE = /may be out of date|possibly stale|might have changed|last updated/i;

describe("staleness", () => {
  test("the staleness note is rendered verbatim", () => {
    const a = render(<Staleness staleness={unknown} />);
    expect(a.container.textContent).toContain(
      "Correct at commit 70f51ac, indexed 3 days ago. codetrail has not checked whether main has moved since.",
    );
    a.unmount();

    // Two states with two different sentences: a mutant that hard-codes one of
    // them passes a single-state fixture.
    const b = render(<Staleness staleness={superseded} />);
    expect(b.container.textContent).toContain(
      "main is also indexed here at the later commit 61c3d15, so this citation is from an older commit. codetrail did not ask the forge, so main may have moved again.",
    );
  });

  test("the console never paraphrases staleness", () => {
    // The negative half. A mutant that renders the note AND its own paraphrase
    // beside it escapes a positive-only assertion; this is what closes that.
    for (const s of [unknown, superseded]) {
      const { container, unmount } = render(<Staleness staleness={s} />);
      expect(container.textContent).not.toMatch(PARAPHRASE);
      unmount();
    }
  });

  test("a superseded citation names the later commit and is labelled in words, not only in colour", () => {
    const { container } = render(<Staleness staleness={superseded} />);
    expect(container.textContent).toContain("superseded");
    expect(container.textContent).toContain("61c3d15");
    // And the unknown state must not claim it.
    const other = render(<Staleness staleness={unknown} />);
    expect(other.container.textContent).not.toContain("superseded");
  });
});
