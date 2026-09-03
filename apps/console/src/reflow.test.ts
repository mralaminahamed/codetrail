import { describe, expect, test } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// Read off disk from the project root: vitest transforms import.meta.url into
// something that is not a file: URL, and this file asserts the stylesheet's
// text rather than importing it.
const css = readFileSync(resolve(process.cwd(), "src/index.css"), "utf8");

// These assert the SOURCE, not the rendering, and that is all they can do:
// jsdom does not lay out or composite, so no test in this suite can measure a
// reflow. The measurement is in docs/a11y-sweep-2026-09-03.md — 320px viewport,
// scrollWidth 3409 before and 320 after. What this guards is silent deletion of
// the rules that measurement closed.
describe("the reflow rules the 320px sweep added", () => {
  test("pre scrolls inside its own box rather than widening the page", () => {
    expect(css).toMatch(/pre\s*\{[^}]*overflow-x:\s*auto/);
    expect(css).toMatch(/pre\s*\{[^}]*max-width:\s*100%/);
  });

  test("an inline code span wraps, because a digest is one unbreakable token", () => {
    expect(css).toMatch(/:not\(pre\)\s*>\s*code\s*\{[^}]*overflow-wrap:\s*anywhere/);
  });

  test("the tuple's dd wraps too, since it holds the commit and the digest", () => {
    expect(css).toMatch(/dd\s*\{[^}]*overflow-wrap:\s*anywhere/);
  });

  test("the skip link is off-screen until focused and comes back on focus", () => {
    expect(css).toMatch(/\.skip\s*\{[^}]*left:\s*-9999px/);
    expect(css).toMatch(/\.skip:focus\s*\{[^}]*left:\s*0/);
  });
});
