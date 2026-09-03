import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useJob } from "./useJob";
import { stub, stubSequence, stubUnreachable } from "../test/msw";
import { CEILING_MS, intervalFor } from "./pollSchedule";
import jobPending from "../api/fixtures/job-pending.json";
import jobLeased from "../api/fixtures/job-leased.json";
import jobDone from "../api/fixtures/job-done.json";
import jobFailed from "../api/fixtures/job-failed.json";
import error404job from "../api/fixtures/error-404-job.json";
import error500 from "../api/fixtures/error-500.json";

const PATH = "/api/jobs/:id";

// The request count the schedule produces over a window, computed from
// intervalFor itself so the expectation cannot drift from the table — and then
// asserted as a literal below, so a mutation to intervalFor changes both the
// prediction and the observation and the test still fails.
function scheduledCalls(windowMs: number): number {
  let t = 0;
  let calls = 1; // the immediate poll at t=0
  for (;;) {
    const iv = intervalFor(t);
    if (iv === null) return calls;
    t += iv;
    if (t > windowMs) return calls;
    calls++;
  }
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

// Real timers cannot detect a polling-policy bug and fake timers can deadlock
// instead of failing, so every advance is awaited.
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

describe("the job poller", () => {
  test("a pending job is polled at the scheduled spacing, not at a fixed interval", async () => {
    const rec = stub("get", PATH, 200, jobPending);
    renderHook(() => useJob("j1"));
    await advance(0);
    expect(rec.calls).toBe(1);

    // 11 minutes crosses three stages. A 10-second window could not separate a
    // constant 1s interval from the schedule, because the first stage IS 1s.
    await advance(11 * 60_000);
    const want = scheduledCalls(11 * 60_000);
    expect(rec.calls).toBe(want);
    // The literal, so the count is pinned independently of the helper above.
    expect(want).toBe(168);
  });

  test("the spacing widens stage by stage rather than staying at one second", async () => {
    const rec = stub("get", PATH, 200, jobPending);
    renderHook(() => useJob("j1"));
    await advance(0);

    await advance(15_000);
    const firstStage = rec.calls;
    await advance(15_000);
    const secondStage = rec.calls - firstStage;
    // 16 polls in the 1s stage against 7 in the 2s stage: a constant interval
    // makes these two numbers equal, which is the whole assertion.
    expect(firstStage).toBe(16);
    expect(secondStage).toBe(7);
  });

  test("a done job stops the poller", async () => {
    const rec = stubSequence("get", PATH, [jobPending, jobLeased, jobDone]);
    const { result } = renderHook(() => useJob("j1"));
    await advance(5_000);
    expect(result.current.phase).toBe("done");
    const settled = rec.calls;
    await advance(60_000);
    expect(rec.calls).toBe(settled);
  });

  test("a failed job stops the poller", async () => {
    const rec = stubSequence("get", PATH, [jobPending, jobFailed]);
    const { result } = renderHook(() => useJob("j1"));
    await advance(5_000);
    expect(result.current.phase).toBe("failed");
    const settled = rec.calls;
    await advance(60_000);
    expect(rec.calls).toBe(settled);
  });

  test("a 404 stops the poller immediately and does not retry", async () => {
    const rec = stub("get", PATH, 404, error404job);
    const { result } = renderHook(() => useJob("nope"));
    await advance(0);
    // The count first, because it is the discriminating claim: a mutant that
    // retries a 404 is visible here over a fixed window whatever it does to
    // the phase. One request, and sixty seconds later still one.
    expect(rec.calls).toBe(1);
    await advance(60_000);
    expect(rec.calls).toBe(1);
    expect(result.current.phase).toBe("missing");
    expect(result.current.detail).toBe("no such job");
  });

  test("a 500 backs off and retries, and is not treated as a terminal job state", async () => {
    const rec = stub("get", PATH, 500, error500);
    const { result } = renderHook(() => useJob("j1"));
    await advance(0);
    expect(rec.calls).toBe(1);
    expect(result.current.phase).toBe("polling");

    // 1s, then 2s, then 4s: the backoff, not the schedule.
    await advance(1_000);
    expect(rec.calls).toBe(2);
    await advance(1_000);
    expect(rec.calls).toBe(2);
    await advance(1_000);
    expect(rec.calls).toBe(3);
    await advance(4_000);
    expect(rec.calls).toBe(4);
  });

  test("ten consecutive transport failures stop the poller and report it as unreachable", async () => {
    const rec = stubUnreachable("get", PATH);
    const { result } = renderHook(() => useJob("j1"));
    await advance(0);
    await advance(10 * 60_000);
    expect(result.current.phase).toBe("unreachable");
    // 1 immediate + 9 retries; the tenth failure stops instead of scheduling.
    expect(rec.calls).toBe(10);
    const settled = rec.calls;
    await advance(10 * 60_000);
    expect(rec.calls).toBe(settled);
  });

  test("a hidden tab issues no requests, and becoming visible polls at once", async () => {
    const rec = stub("get", PATH, 200, jobPending);
    renderHook(() => useJob("j1"));
    await advance(0);
    expect(rec.calls).toBe(1);

    const hidden = vi.spyOn(document, "hidden", "get").mockReturnValue(true);
    await advance(30_000);
    const whileHidden = rec.calls;
    // At most the one poll already in flight when the tab went away.
    expect(rec.calls).toBeLessThanOrEqual(2);

    hidden.mockReturnValue(false);
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
      await Promise.resolve();
    });
    expect(rec.calls).toBe(whileHidden + 1);
    hidden.mockRestore();
  });

  // 31 minutes is 244 awaited timer callbacks, which is slower than the 5s
  // default. Raised rather than shortened: a window that does not cross the
  // ceiling cannot test the ceiling.
  test("polling stops at the ceiling and the manual check works after it", { timeout: 30_000 }, async () => {
    const rec = stub("get", PATH, 200, jobPending);
    const { result } = renderHook(() => useJob("j1"));
    await advance(0);
    await advance(CEILING_MS + 60_000);

    expect(result.current.phase).toBe("ceiling");
    const atCeiling = rec.calls;
    expect(atCeiling).toBe(scheduledCalls(CEILING_MS));
    expect(atCeiling).toBe(244);

    // And it stays stopped rather than quietly continuing.
    await advance(10 * 60_000);
    expect(rec.calls).toBe(atCeiling);

    // The manual check issues exactly one more. Driven with act + an awaited
    // timer advance rather than waitFor: waitFor polls on a real interval and
    // deadlocks against fake timers, which is an incident rather than a claim
    // failing.
    await act(async () => {
      result.current.checkNow();
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(rec.calls).toBe(atCeiling + 1);
    await advance(60_000);
    expect(rec.calls).toBe(atCeiling + 1);
  });
});
