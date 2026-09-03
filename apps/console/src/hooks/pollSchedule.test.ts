import { describe, expect, test } from "vitest";
import { backoffFor, CEILING_MS, intervalFor, MAX_TRANSPORT_FAILURES } from "./pollSchedule";

describe("the polling schedule", () => {
  test("the interval is 1s in the first 15 seconds", () => {
    expect(intervalFor(0)).toBe(1_000);
    expect(intervalFor(14_999)).toBe(1_000);
  });

  test("the interval is 2s from 15 seconds to 2 minutes", () => {
    // The boundary belongs to the upper stage.
    expect(intervalFor(15_000)).toBe(2_000);
    expect(intervalFor(119_999)).toBe(2_000);
  });

  test("the interval is 5s from 2 minutes to 10 minutes", () => {
    expect(intervalFor(120_000)).toBe(5_000);
    expect(intervalFor(599_999)).toBe(5_000);
  });

  test("the interval is 15s from 10 minutes to 30 minutes", () => {
    expect(intervalFor(600_000)).toBe(15_000);
    expect(intervalFor(1_799_999)).toBe(15_000);
  });

  test("polling stops at 30 minutes", () => {
    expect(intervalFor(CEILING_MS)).toBeNull();
    expect(intervalFor(CEILING_MS + 1)).toBeNull();
    expect(CEILING_MS).toBe(30 * 60_000);
  });

  test("the transport backoff doubles from 1s to a 30s ceiling and never jitters", () => {
    // The whole sequence as a literal. A range assertion would pass for a
    // jittered schedule, which is exactly what this must not become.
    const seq = Array.from({ length: MAX_TRANSPORT_FAILURES }, (_, i) => backoffFor(i + 1));
    expect(seq).toEqual([1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000, 30_000, null]);
  });

  test("ten consecutive transport failures stop the backoff", () => {
    expect(backoffFor(MAX_TRANSPORT_FAILURES)).toBeNull();
    expect(backoffFor(MAX_TRANSPORT_FAILURES + 1)).toBeNull();
  });

  test("the schedule is deterministic: the same elapsed time always gives the same interval", () => {
    for (const t of [0, 7_000, 15_000, 60_000, 300_000, 900_000]) {
      expect(intervalFor(t)).toBe(intervalFor(t));
    }
  });
});
