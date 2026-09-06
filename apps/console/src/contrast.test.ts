import { describe, expect, test } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// The contrast test the automated suite could not previously have.
//
// axe-core's color-contrast rule CANNOT RUN under jsdom: jsdom does not lay out
// or composite, so getComputedStyle cannot resolve what a colour is drawn
// against. docs/a11y-sweep-2026-09-03.md records the console's contrast as
// zero-covered by the suite for exactly that reason.
//
// This does not fix that — it cannot; nothing in jsdom knows which token ends
// up behind which text. What it does is close the other half: it reads the
// palette out of src/index.css and computes WCAG 2.x contrast for every pair
// the design actually uses, in BOTH themes. A palette edit that drops a pair
// under its threshold fails here rather than in a browser nobody opened.
//
// The formula is the one WCAG defines, on sRGB, and the palette is authored in
// hex for that reason: the 2026-09-03 sweep recorded a false PASS caused by a
// naive parser reading oklch as a ratio of 1.0.

const css = readFileSync(resolve(process.cwd(), "src/index.css"), "utf8");

// The two :root blocks: the default (light) and the one inside the
// prefers-color-scheme: dark query. Read by position, because the dark block is
// the only :root inside an @media in this file.
function tokensAfter(marker: string): Record<string, string> {
  const at = css.indexOf(marker);
  expect(at, `${marker} is not in index.css`).toBeGreaterThanOrEqual(0);
  const open = css.indexOf("{", at);
  const close = css.indexOf("}", open);
  const block = css.slice(open, close);
  const out: Record<string, string> = {};
  for (const m of block.matchAll(/(--[a-z-]+):\s*(#[0-9a-fA-F]{6})/g)) {
    out[m[1]!] = m[2]!;
  }
  return out;
}

const light = tokensAfter(":root {");
const dark = tokensAfter("@media (prefers-color-scheme: dark)");

function luminance(hex: string): number {
  const parts = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
  const lin = parts.map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * lin[0]! + 0.7152 * lin[1]! + 0.0722 * lin[2]!;
}

export function contrast(fg: string, bg: string): number {
  const a = luminance(fg);
  const b = luminance(bg);
  const [hi, lo] = a > b ? [a, b] : [b, a];
  return (hi + 0.05) / (lo + 0.05);
}

// Every pair the stylesheet actually draws, with the threshold that applies to
// it. 4.5 is 1.4.3 (text). 3.0 is 1.4.11 (non-text) and is claimed ONLY for
// things 1.4.11 covers: the boundary of a control you have to find, and the
// focus ring. A decorative card edge is not a UI component and is deliberately
// NOT in this table — claiming 3:1 for a hairline would be inventing a pass.
const TEXT: [string, string, string][] = [
  ["body text", "--ink", "--paper"],
  ["body text on a card", "--ink", "--surface"],
  ["body text on a refusal", "--ink", "--note-bg"],
  ["code text", "--ink", "--code-bg"],
  ["secondary text", "--quiet", "--paper"],
  ["secondary text on a card", "--quiet", "--surface"],
  ["secondary text on a refusal", "--quiet", "--note-bg"],
  ["secondary text on code", "--quiet", "--code-bg"],
  ["link", "--link", "--paper"],
  ["link on a card", "--link", "--surface"],
  ["link on code", "--link", "--code-bg"],
  ["visited link", "--visited", "--paper"],
  ["visited link on a card", "--visited", "--surface"],
  ["the citation's amber label", "--amber", "--surface"],
  ["the citation's amber label on paper", "--amber", "--paper"],
  ["error heading", "--alarm-ink", "--alarm-bg"],
  ["error heading on paper", "--alarm-ink", "--paper"],
  ["degradation heading", "--warn-ink", "--warn-bg"],
  ["degradation heading on paper", "--warn-ink", "--paper"],
  ["degradation heading on a card", "--warn-ink", "--surface"],
];

const NONTEXT: [string, string, string][] = [
  ["a control's border", "--control", "--paper"],
  ["a control's border on a card", "--control", "--surface"],
  ["a control's border on a refusal", "--control", "--note-bg"],
  ["the focus ring", "--link", "--paper"],
  ["the focus ring on a card", "--link", "--surface"],
  ["the focus ring on code", "--link", "--code-bg"],
];

describe("the palette's contrast, computed from index.css", () => {
  for (const [name, theme] of [
    ["light", light],
    ["dark", dark],
  ] as const) {
    test(`${name}: every text pair clears WCAG 1.4.3 at 4.5:1`, () => {
      for (const [what, fg, bg] of TEXT) {
        const f = theme[fg];
        const b = theme[bg];
        expect(f, `${fg} is missing from the ${name} palette`).toBeDefined();
        expect(b, `${bg} is missing from the ${name} palette`).toBeDefined();
        const ratio = contrast(f!, b!);
        expect(
          ratio,
          `${name}: ${what} — ${fg} ${f} on ${bg} ${b} is ${ratio.toFixed(2)}:1`,
        ).toBeGreaterThanOrEqual(4.5);
      }
    });

    test(`${name}: every control boundary and focus ring clears WCAG 1.4.11 at 3:1`, () => {
      for (const [what, fg, bg] of NONTEXT) {
        const f = theme[fg];
        const b = theme[bg];
        expect(f, `${fg} is missing from the ${name} palette`).toBeDefined();
        expect(b, `${bg} is missing from the ${name} palette`).toBeDefined();
        const ratio = contrast(f!, b!);
        expect(
          ratio,
          `${name}: ${what} — ${fg} ${f} on ${bg} ${b} is ${ratio.toFixed(2)}:1`,
        ).toBeGreaterThanOrEqual(3);
      }
    });
  }

  // The negative half. Without it a mutant that set every token to the same
  // near-black passes every assertion above by making all ratios 1.0 fail...
  // no: it would fail. What it would NOT fail is a light palette silently
  // shipped as the dark one, where every pair still clears its threshold and
  // the page is white in a dark-mode browser.
  test("the two themes are actually different, so dark mode is not the light palette", () => {
    expect(dark["--paper"]).not.toBe(light["--paper"]);
    expect(dark["--ink"]).not.toBe(light["--ink"]);
    // And the polarity is inverted, which is the property that makes it a dark
    // theme rather than a second light one.
    expect(luminance(light["--paper"]!)).toBeGreaterThan(luminance(light["--ink"]!));
    expect(luminance(dark["--paper"]!)).toBeLessThan(luminance(dark["--ink"]!));
  });

  test("the stylesheet declares color-scheme, so form controls and scrollbars follow the theme", () => {
    expect(css).toMatch(/color-scheme:\s*light dark/);
  });
});
