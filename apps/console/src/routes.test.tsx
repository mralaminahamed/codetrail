import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import AppRoutes from "./routes";

function renderApp(at = "/") {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <AppRoutes />
    </MemoryRouter>,
  );
}

describe("routing", () => {
  test("a route change moves focus to the new page's h1", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.click(screen.getByRole("link", { name: "Corpus" }));
    const h1 = await screen.findByRole("heading", { level: 1, name: "Corpus" });
    // Element identity, not a class or an attribute: a mutant that re-adds
    // tabIndex without focusing still fails here.
    expect(document.activeElement).toBe(h1);
  });

  test("a page load does not move focus, so the skip link stays reachable", async () => {
    const user = userEvent.setup();
    renderApp();
    expect(document.activeElement).toBe(document.body);
    await user.tab();
    expect(document.activeElement).toBe(
      screen.getByRole("link", { name: "Skip to content" }),
    );
  });

  test("a route change sets document.title to the page's name", async () => {
    const user = userEvent.setup();
    renderApp();
    expect(document.title).toBe("Submit a repository — codetrail");
    await user.click(screen.getByRole("link", { name: "Corpus" }));
    await screen.findByRole("heading", { level: 1, name: "Corpus" });
    expect(document.title).toBe("Corpus — codetrail");
  });

  test("an unknown route renders a not-found page with its own h1, not a blank main", () => {
    renderApp("/no/such/page");
    expect(
      screen.getByRole("heading", { level: 1, name: "Page not found" }),
    ).toBeInTheDocument();
    expect(document.querySelector("main")?.textContent).toContain(
      "codetrail has no page at this address.",
    );
  });
});
