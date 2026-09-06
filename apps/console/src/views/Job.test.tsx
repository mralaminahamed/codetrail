import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { axe } from "jest-axe";
import { MemoryRouter, Route, Routes } from "react-router";
import Job from "./Job";
import { stub, stubSequence, stubUnreachable } from "../test/msw";
import jobPending from "../api/fixtures/job-pending.json";
import jobLeased from "../api/fixtures/job-leased.json";
import jobDone from "../api/fixtures/job-done.json";
import jobFailed from "../api/fixtures/job-failed.json";
import error404job from "../api/fixtures/error-404-job.json";

const PATH = "/api/jobs/:id";

function renderJob() {
  return render(
    <MemoryRouter initialEntries={["/jobs/j1"]}>
      <Routes>
        <Route path="/jobs/:id" element={<Job />} />
        <Route path="/repos/:repo" element={<h1>Repository</h1>} />
      </Routes>
    </MemoryRouter>,
  );
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

// Queried by accessible name, never a bare getByRole("status"): Task 6's
// refusal panel is also a role="status", and a tree holding both makes a bare
// query throw "found multiple elements" — an incident, not a kill.
function liveRegion() {
  return screen.getByRole("status", { name: "Indexing progress" });
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("the job view", () => {
  test("the live region says one sentence per status and does not change while the status does not", async () => {
    stub("get", PATH, 200, jobPending);
    renderJob();
    await advance(0);
    const before = liveRegion().textContent;
    expect(before).toBe("Queued.");

    // Ten polls with an unchanged status. A test that asserts the sentence is
    // present cannot see a counter in the region: it is present in both.
    await advance(10_000);
    expect(liveRegion().textContent).toBe(before);
    expect(liveRegion().textContent).not.toMatch(/\d/);
  });

  test("the live region's sentence changes with the status and only with it", async () => {
    stubSequence("get", PATH, [jobPending, jobLeased, jobDone]);
    renderJob();
    await advance(0);
    expect(liveRegion().textContent).toBe("Queued.");
    await advance(1_000);
    expect(liveRegion().textContent).toBe("Indexing.");
    await advance(1_000);
    expect(liveRegion().textContent).toBe("Indexing finished.");
  });

  test("a status change while the user is typing does not move focus", async () => {
    stubSequence("get", PATH, [jobPending, jobLeased, jobDone]);
    renderJob();
    await advance(0);

    // An input the user is in when the job completes.
    const input = document.createElement("input");
    input.name = "q";
    document.body.appendChild(input);
    input.focus();
    expect(document.activeElement).toBe(input);

    await advance(5_000);
    // getByRole, not findByRole: findByRole waits on a real interval and
    // deadlocks against fake timers, which is an incident rather than a claim
    // failing. The advance above has already driven the job to done.
    screen.getByRole("link", { name: "Ask this repository" });
    // Element identity: nothing that only asserts the link rendered can see it.
    expect(document.activeElement).toBe(input);
    input.remove();
  });

  test("a failed job says codetrail does not report why, in those words", async () => {
    stub("get", PATH, 200, jobFailed);
    renderJob();
    await advance(0);
    expect(liveRegion().textContent).toBe("Indexing failed.");
    const sentence = screen.getByText(/codetrail does not report why a job failed/);
    expect(sentence).toBeInTheDocument();
    expect(sentence.textContent).toContain("withholds the indexer's stderr");
    // Not softened to a shrug.
    const { container } = renderJob();
    expect(container.textContent).not.toMatch(/something went wrong/i);
  });

  test("a done job links to the repository the job produced", async () => {
    stub("get", PATH, 200, jobDone);
    renderJob();
    await advance(0);
    const link = screen.getByRole("link", { name: "Ask this repository" });
    // The exact href built from repo_id, not "a link exists".
    expect(link).toHaveAttribute("href", `/repos/${jobDone.repo_id}`);
  });

  test("a pending job offers no repository link and no retry control", async () => {
    stub("get", PATH, 200, jobPending);
    renderJob();
    await advance(0);
    expect(screen.queryByRole("link", { name: "Ask this repository" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Check again" })).toBeNull();
  });

  test("a 404 says the job is unknown, not that codetrail is down", async () => {
    stub("get", PATH, 404, error404job);
    renderJob();
    await advance(0);
    expect(liveRegion().textContent).toBe("codetrail has no job with this id.");
    expect(screen.getByText("no such job")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  test("the ceiling offers a manual check and says codetrail stopped on purpose", async () => {
    stub("get", PATH, 200, jobPending);
    renderJob();
    await advance(0);
    await advance(31 * 60_000);
    expect(
      screen.getByText(/codetrail has stopped checking automatically; a repository this large is possible/),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Check again" })).toBeInTheDocument();
  }, 30_000);

  test("an unreachable gateway is an alert, and a refusal-shaped status is not used for it", async () => {
    stubUnreachable("get", PATH);
    renderJob();
    await advance(0);
    await advance(10 * 60_000);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(liveRegion().textContent).toBe("codetrail could not be reached.");
  });

  // axe-core schedules its own work on real timers, so this one runs on real
  // timers and settles by awaiting the poll rather than by advancing.
  test("the job view has no axe violations in pending, done, failed and unreachable states", async () => {
    vi.useRealTimers();
    for (const [status, fixture] of [
      ["pending", jobPending],
      ["done", jobDone],
      ["failed", jobFailed],
    ] as const) {
      stub("get", PATH, 200, fixture);
      const { container, unmount } = renderJob();
      await screen.findByRole("status", { name: "Indexing progress" });
      const results = await axe(container);
      expect(results, `axe on ${status}`).toHaveNoViolations();
      unmount();
    }

    stubUnreachable("get", PATH);
    const { container, unmount } = renderJob();
    await screen.findByRole("status", { name: "Indexing progress" });
    expect(await axe(container)).toHaveNoViolations();
    unmount();
  }, 30_000);

  test("the browser tab tracks the job's phase, because nobody watches the page", async () => {
    // PageTitle has taken a `title` prop since P5 and no caller passed one:
    // every view's tab read one static string for its whole life, and this is
    // the view where that costs the most — indexing takes minutes and the tab
    // is backgrounded for all of them.
    stubSequence("get", PATH, [jobPending, jobLeased, jobDone]);
    renderJob();
    await advance(0);
    expect(document.title).toBe("Queued — codetrail");
    await advance(1_000);
    expect(document.title).toBe("Indexing — codetrail");
    await advance(1_000);
    expect(document.title).toBe("Indexed — codetrail");
  });
});
