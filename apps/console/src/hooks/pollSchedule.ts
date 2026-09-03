// The polling policy, alone in a file and as pure functions, so it can be
// tested as a table with no timers at all and so a mutation lands on a number
// rather than on a useEffect.

// A tick landing exactly on a stage boundary belongs to the UPPER stage: the
// comparisons are strict `<`, so elapsed 15000 is already the 2s stage. Fixed
// here rather than left to the code, because the request-count assertions
// depend on it.
const STAGES: ReadonlyArray<readonly [until: number, interval: number]> = [
  [15_000, 1_000], // the first 15s: someone just pressed the button
  [120_000, 2_000], // to 2m: the README measures rs/zerolog at about 2.5m
  [600_000, 5_000], // to 10m
  [1_800_000, 15_000], // to 30m
];

// A job still pending after thirty minutes is not a slow repository, it is a
// queue nobody is serving — a different problem, which the UI should say rather
// than poll at. This endpoint has no auth and no rate limit (spec:145-147).
export const CEILING_MS = 1_800_000;

// null means stop.
export function intervalFor(elapsedMs: number): number | null {
  for (const [until, interval] of STAGES) {
    if (elapsedMs < until) return interval;
  }
  return null;
}

// Transport failure has its own backoff, separate from the schedule above: the
// schedule is about how fast a job is expected to move, this is about a
// connection that is not working. No jitter — there is one browser tab, not a
// thundering herd, and jitter would force every test to assert a range instead
// of a value.
export const MAX_TRANSPORT_FAILURES = 10;
const BACKOFF_CEILING_MS = 30_000;

// failures is the count of consecutive failures including this one. null means
// stop and say so.
export function backoffFor(failures: number): number | null {
  if (failures >= MAX_TRANSPORT_FAILURES) return null;
  return Math.min(1_000 * 2 ** (failures - 1), BACKOFF_CEILING_MS);
}
