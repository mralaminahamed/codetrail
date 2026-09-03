import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "jest-axe";
import { MemoryRouter } from "react-router";
import AppRoutes from "../routes";

function renderApp(at = "/") {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <AppRoutes />
    </MemoryRouter>,
  );
}

describe("the shell", () => {
  test("the skip link is the first focusable element and moves focus to main", async () => {
    const user = userEvent.setup();
    renderApp();
    // Tab from the document body: a mutant that renders the skip link anywhere
    // but first is caught here and nowhere else, because getByRole finds it in
    // both trees.
    await user.tab();
    const skip = screen.getByRole("link", { name: "Skip to content" });
    expect(document.activeElement).toBe(skip);
    expect(skip).toHaveAttribute("href", "#main");

    await user.tab();
    expect(document.activeElement).toBe(
      screen.getByRole("link", { name: "codetrail" }),
    );

    const main = document.querySelector("main");
    expect(main).toHaveAttribute("id", "main");
    expect(main).toHaveAttribute("tabindex", "-1");
  });

  test("there is exactly one h1, one main landmark and one navigation landmark", () => {
    renderApp();
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    expect(screen.getAllByRole("main")).toHaveLength(1);
    const navs = screen.getAllByRole("navigation");
    expect(navs).toHaveLength(1);
    expect(navs[0]).toHaveAccessibleName("Corpus");
  });

  test("the live region is polite and atomic, and is not an alert", async () => {
    const { default: Live } = await import("./Live");
    const { container } = render(<Live>Queued.</Live>);
    const region = screen.getByRole("status", { name: "Indexing progress" });
    expect(region).toHaveAttribute("aria-live", "polite");
    expect(region).toHaveAttribute("aria-atomic", "true");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(container.querySelector('[aria-live="assertive"]')).toBeNull();
  });

  test("the shell has no axe violations", async () => {
    const { container } = renderApp();
    expect(await axe(container)).toHaveNoViolations();
  });
});
