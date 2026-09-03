const KEY = "codetrail.recent-jobs";

export type RecentJob = { id: string; remote: string; ref: string };

// There is no endpoint that lists jobs, and re-submitting after a job has
// finished appends a new row rather than returning the old one — the dedupe
// index only collapses jobs that are still active (spec:145-149). So the only
// way back to a job whose tab was closed is its id, and the console keeps the
// last few locally. Browser-local and lost with the profile; the README says
// so. A GET /api/jobs listing is the retention decision spec:318-321 defers,
// and answering it inside a console phase would be answering it by accident.
const KEEP = 5;

export function readRecent(): RecentJob[] {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw === null) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(
      (j): j is RecentJob =>
        typeof j === "object" && j !== null && typeof (j as RecentJob).id === "string",
    );
  } catch {
    // A private window, cleared site data, or a browser refusing storage. A
    // console that threw here would lose the submission over the bookkeeping.
    return [];
  }
}

export function rememberJob(job: RecentJob): void {
  try {
    const kept = [job, ...readRecent().filter((j) => j.id !== job.id)].slice(0, KEEP);
    localStorage.setItem(KEY, JSON.stringify(kept));
  } catch {
    /* see readRecent */
  }
}
