import { beforeEach, describe, expect, test } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { axe } from "jest-axe";
import { MemoryRouter, Route, Routes } from "react-router";
import Submit from "./Submit";
import { stub } from "../test/msw";
import { readRecent } from "../ui/recent";
import jobPending from "../api/fixtures/job-pending.json";
import scheme from "../api/fixtures/error-400-scheme.json";
import host from "../api/fixtures/error-400-host.json";
import formPath from "../api/fixtures/error-400-form-path.json";
import formRef from "../api/fixtures/error-400-form-ref.json";
import error500 from "../api/fixtures/error-500.json";

// All four sentences POST /api/repos can refuse with. Each test asserts its own
// present and the other three absent — three, not two: form-path and form-ref
// share rule "form" and differ only in detail, and that pair is what proves the
// console renders detail rather than the rule.
const SENTENCES = [
  scheme.error,
  host.error,
  formPath.error,
  formRef.error,
];

function renderSubmit() {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <Routes>
        <Route path="/" element={<Submit />} />
        <Route path="/jobs/:id" element={<h1>Indexing job</h1>} />
      </Routes>
    </MemoryRouter>,
  );
}

async function submit(remote: string, ref = "") {
  const user = userEvent.setup();
  await user.clear(screen.getByLabelText("Repository URL"));
  await user.type(screen.getByLabelText("Repository URL"), remote);
  if (ref !== "") await user.type(screen.getByLabelText("Ref (optional)"), ref);
  await user.click(screen.getByRole("button", { name: "Index this repository" }));
}

function assertOnlySentence(container: HTMLElement, want: string) {
  expect(container.textContent).toContain(want);
  for (const other of SENTENCES) {
    if (other === want) continue;
    expect(container.textContent).not.toContain(other);
  }
}

beforeEach(() => localStorage.clear());

describe("submitting a repository", () => {
  test("submitting a URL sends it to the gateway without validating it first", async () => {
    // Driven with an http:// URL, which a client-side guard would short-circuit.
    // The assertion is that the request WAS MADE: a test that only checks the
    // message appears cannot see this, because the mutant renders an
    // identical-looking message of its own.
    const rec = stub("post", "/api/repos", 400, scheme);
    renderSubmit();
    await submit("http://github.com/rs/zerolog");
    await waitFor(() => expect(rec.calls).toBe(1));
    expect(rec.bodies[0]).toEqual({ remote: "http://github.com/rs/zerolog" });
  });

  test("a scheme rejection names the scheme rule and shows the server's sentence", async () => {
    stub("post", "/api/repos", 400, scheme);
    const { container } = renderSubmit();
    await submit("http://github.com/rs/zerolog");
    await screen.findByText(scheme.error);
    assertOnlySentence(container, scheme.error);
    expect(screen.getByText("scheme")).toBeInTheDocument();
  });

  test("a host rejection names the host rule and shows the server's sentence", async () => {
    stub("post", "/api/repos", 400, host);
    const { container } = renderSubmit();
    await submit("https://example.com/rs/zerolog");
    await screen.findByText(host.error);
    assertOnlySentence(container, host.error);
    expect(screen.getByText("host")).toBeInTheDocument();
    // A 400 is not an error: nothing broke and there is no id to quote.
    expect(screen.queryByRole("alert")).toBeNull();
    expect(container.textContent).not.toContain("request id");
  });

  test("a bad path shows the server's path sentence", async () => {
    stub("post", "/api/repos", 400, formPath);
    const { container } = renderSubmit();
    await submit("https://github.com/rs/zerolog/tree/master");
    await screen.findByText(formPath.error);
    assertOnlySentence(container, formPath.error);
  });

  test("a bad ref shows the server's ref sentence, which shares its rule with the path one", async () => {
    stub("post", "/api/repos", 400, formRef);
    const { container } = renderSubmit();
    await submit("https://github.com/rs/zerolog", "--upload-pack=x");
    await screen.findByText(formRef.error);
    // The discriminating pair: both carry rule "form", so a console that
    // rendered the rule would show the same text for this and for the bad path.
    assertOnlySentence(container, formRef.error);
    expect(formRef.rule).toBe(formPath.rule);
    expect(screen.getByText("form")).toBeInTheDocument();
  });

  test("no rejection ever renders the words 'invalid URL'", async () => {
    for (const fixture of [scheme, host, formPath, formRef]) {
      stub("post", "/api/repos", 400, fixture);
      const { container, unmount } = renderSubmit();
      await submit("https://github.com/rs/zerolog");
      await screen.findByText(fixture.error);
      expect(container.textContent).not.toMatch(/invalid url/i);
      unmount();
    }
  });

  test("an accepted submission navigates to the job and remembers its id", async () => {
    stub("post", "/api/repos", 202, jobPending);
    renderSubmit();
    await submit("https://github.com/codetrail-live/pending");
    await screen.findByRole("heading", { name: "Indexing job" });
    expect(readRecent()[0]?.id).toBe(jobPending.id);
    expect(readRecent()[0]?.remote).toBe(jobPending.remote);
  });

  test("a pasted URL with surrounding whitespace is trimmed before it is sent", async () => {
    // Without this the trim is unobservable: every other test types a clean
    // URL. A pasted "  https://...  " reaches url.Parse with a leading space,
    // fails admit's scheme check, and comes back as a 400 the user cannot act
    // on.
    const rec = stub("post", "/api/repos", 202, jobPending);
    const user = userEvent.setup();
    renderSubmit();
    await user.type(screen.getByLabelText("Repository URL"), "  https://github.com/rs/zerolog  ");
    await user.click(screen.getByRole("button", { name: "Index this repository" }));
    await waitFor(() => expect(rec.calls).toBe(1));
    expect(rec.bodies[0]).toEqual({ remote: "https://github.com/rs/zerolog" });
  });

  test("an empty ref is sent as absent, not as HEAD", async () => {
    const rec = stub("post", "/api/repos", 202, jobPending);
    renderSubmit();
    await submit("https://github.com/rs/zerolog");
    await waitFor(() => expect(rec.calls).toBe(1));
    expect(rec.bodies[0]).toEqual({ remote: "https://github.com/rs/zerolog" });
  });

  test("the form submits on Enter and the button is a real submit button", async () => {
    const rec = stub("post", "/api/repos", 202, jobPending);
    const user = userEvent.setup();
    renderSubmit();
    expect(screen.getByRole("button", { name: "Index this repository" })).toHaveAttribute("type", "submit");
    await user.type(screen.getByLabelText("Repository URL"), "https://github.com/rs/zerolog{Enter}");
    await waitFor(() => expect(rec.calls).toBe(1));
  });

  test("the rejection is IN the region before focus reaches it, and the region has a name", async () => {
    // Same discrimination as Ask.test's: the test this replaces asserted
    // activeElement afterwards and passed under the broken version too, because
    // focus() ran on the line after setOutcome — before React had rendered —
    // and the element focused is the same either way.
    let atFocus: string | null = null;
    const on = (e: FocusEvent) => {
      const el = e.target as HTMLElement;
      if (atFocus === null && el.getAttribute?.("tabindex") === "-1") atFocus = el.textContent ?? "";
    };
    document.addEventListener("focusin", on);
    try {
      stub("post", "/api/repos", 400, host);
      renderSubmit();
      await submit("https://example.com/rs/zerolog");
      await screen.findByText(host.error);
      expect(document.activeElement).toBe(screen.getByRole("region", { name: "Submission result" }));
      expect(atFocus).toContain(host.error);
    } finally {
      document.removeEventListener("focusin", on);
    }
  });

  test("a rejection is a status with its own name, so it is announced at all", async () => {
    // Rejected had NO role until now: the panel naming which admission rule
    // refused the submission was inserted into the page silently.
    stub("post", "/api/repos", 400, scheme);
    renderSubmit();
    await submit("http://github.com/rs/zerolog");
    const panel = await screen.findByRole("status", { name: "Submission outcome" });
    expect(panel.textContent).toContain(scheme.error);
    // A status, never an alert: nothing broke and there is no id to quote.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  test("a 500 renders the error panel with the request id, not the rejection panel", async () => {
    stub("post", "/api/repos", 500, error500);
    const { container } = renderSubmit();
    await submit("https://github.com/rs/zerolog");
    const alert = await screen.findByRole("alert");
    expect(alert).toBeInTheDocument();
    expect(container.textContent).toContain(error500.request_id);
    // And none of the four rejection sentences: a 500 has no rule.
    for (const s of SENTENCES) expect(container.textContent).not.toContain(s);
    expect(container.textContent).not.toContain("codetrail refused this submission.");
  });

  test("submit has no axe violations in its default, rejected and failed states", async () => {
    const plain = renderSubmit();
    expect(await axe(plain.container)).toHaveNoViolations();
    plain.unmount();

    stub("post", "/api/repos", 400, host);
    const rejected = renderSubmit();
    await submit("https://example.com/rs/zerolog");
    await screen.findByText(host.error);
    expect(await axe(rejected.container)).toHaveNoViolations();
    rejected.unmount();

    stub("post", "/api/repos", 500, error500);
    const failed = renderSubmit();
    await submit("https://github.com/rs/zerolog");
    await screen.findByRole("alert");
    expect(await axe(failed.container)).toHaveNoViolations();
  });
});
