import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import Refusal from "./Refusal";
import type { Floor } from "../api/types";

const uncalibrated: Floor = { value: -1, calibrated: false, applicable: true };

// Every way a UI draws a scale. axe covers none of them: an unlabelled
// decorative div with a script-computed width violates no axe-core rule, which
// is why the style.width sweep below is the assertion that catches the escape
// rather than a stylistic nicety.
function assertNoScale(container: HTMLElement) {
  expect(screen.queryByRole("meter")).toBeNull();
  expect(screen.queryByRole("progressbar")).toBeNull();
  expect(container.querySelector("[aria-valuenow]")).toBeNull();
  expect(container.querySelector("progress")).toBeNull();
  expect(container.querySelector("meter")).toBeNull();
  const widths = [...container.querySelectorAll<HTMLElement>("*")].map((e) => e.style.width);
  expect(widths.every((w) => !/^\d/.test(w))).toBe(true);
  expect(container.textContent).not.toMatch(/\d+%/);
}

describe("the floor, in words", () => {
  test("a refusal says the floor is uncalibrated and draws no score scale", () => {
    const { container } = render(
      <Refusal reason="below_floor" detail="d" floor={uncalibrated} mode="hybrid" topScore={0.41} />,
    );
    expect(container.textContent).toContain("has never been calibrated");
    expect(container.textContent).toContain(
      "It is a mechanism, not a measured threshold — the number is measured in P6.",
    );
    assertNoScale(container);
  });

  // A unit test of the console's own branch, from a hand-built prop, and it
  // says so. The gateway cannot emit a self-consistent calibrated below-floor
  // payload: detail() appends ". That floor is not calibrated; its value is
  // measured in P6." unconditionally (read.go:390-393), never reading
  // f.Calibrated, so a payload with floor.calibrated true carries a detail
  // that contradicts it. That defect is P6's; recorded, not fixed here.
  test("Refusal draws the comparison when given a calibrated floor", () => {
    const calibrated: Floor = { value: 0.35, calibrated: true, applicable: true };
    const { container } = render(
      <Refusal reason="below_floor" detail="d" floor={calibrated} mode="hybrid" topScore={0.21} />,
    );
    expect(container.textContent).toContain("The best match scored 0.21 against a calibrated floor of 0.35.");
    // The suppression is conditional, not an absent feature: the uncalibrated
    // disclaimer must be gone here. A hard-coded disclaimer passes the
    // uncalibrated test and then lies the day the floor is measured.
    expect(container.textContent).not.toContain("has never been calibrated");
    expect(container.textContent).not.toContain("measured in P6");
  });

  test("a lexical-only refusal says no floor was applied", () => {
    const notApplicable: Floor = { value: -1, calibrated: false, applicable: false };
    const { container } = render(
      <Refusal reason="no_spans" detail="d" floor={notApplicable} mode="lexical" topScore={null} />,
    );
    expect(container.textContent).toContain(
      "This deployment retrieves in lexical mode, which produces no similarity score, so no floor was applied.",
    );
    expect(container.textContent).not.toContain("has never been calibrated");
    assertNoScale(container);
  });

  test("a refusal is a status with its own name, not an alert, and carries no request id", () => {
    const { container } = render(
      <Refusal reason="no_spans" detail="d" floor={uncalibrated} mode="hybrid" topScore={null} />,
    );
    expect(screen.getByRole("status", { name: "Answer outcome" })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(container.textContent).not.toMatch(/request id/i);
    // And no retry control: a refusal is not something to try again.
    expect(screen.queryByRole("button")).toBeNull();
  });
});
