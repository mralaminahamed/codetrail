import { useCallback, useEffect, useRef, useState } from "react";
import { getJob } from "../api/client";
import type { Job } from "../api/types";
import { backoffFor, intervalFor } from "./pollSchedule";

// What the poller has decided. Each is a different sentence and a different
// affordance, which is why they are not collapsed into "loading | error".
export type Phase =
  | "polling"
  | "done"
  | "failed"
  | "missing" // 404: a job id that does not exist will not start existing
  | "ceiling" // still pending after 30 minutes; codetrail stopped checking
  | "unreachable" // ten consecutive transport failures
  | "error"; // a 5xx that kept failing

export type JobState = {
  job: Job | null;
  phase: Phase;
  detail: string;
  requestId: string | null;
  checkNow: () => void;
};

export function useJob(id: string): JobState {
  const [job, setJob] = useState<Job | null>(null);
  const [phase, setPhase] = useState<Phase>("polling");
  const [detail, setDetail] = useState("");
  const [requestId, setRequestId] = useState<string | null>(null);

  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const started = useRef(0);
  const failures = useRef(0);
  const stopped = useRef(false);

  const clear = useCallback(() => {
    if (timer.current !== null) {
      clearTimeout(timer.current);
      timer.current = null;
    }
  }, []);

  useEffect(() => {
    started.current = Date.now();
    failures.current = 0;
    stopped.current = false;
    let cancelled = false;

    const stop = (p: Phase, why = "", rid: string | null = null) => {
      stopped.current = true;
      clear();
      setPhase(p);
      setDetail(why);
      setRequestId(rid);
    };

    const retry = (why: string, rid: string | null, p: Phase) => {
      failures.current += 1;
      const wait = backoffFor(failures.current);
      if (wait === null) {
        stop(p, why, rid);
        return;
      }
      timer.current = setTimeout(() => void pump(), wait);
    };

    const pump = async (): Promise<void> => {
      if (cancelled || stopped.current) return;
      // A hidden tab issues no requests. visibilitychange resumes it; nothing
      // is scheduled in the meantime, so a backgrounded tab costs nothing.
      if (document.hidden) return;

      // The ceiling is checked before the request, not after, so the 30-minute
      // mark stops the poller rather than costing one more call.
      if (intervalFor(Date.now() - started.current) === null) {
        stop("ceiling");
        return;
      }

      const out = await getJob(id);
      if (cancelled || stopped.current) return;

      switch (out.kind) {
        case "ok": {
          failures.current = 0;
          setJob(out.value);
          if (out.value.status === "done") return stop("done");
          if (out.value.status === "failed") return stop("failed");
          break;
        }
        case "missing":
          // Terminal and immediate. A job id that is not in the table will not
          // appear later (handler.go:180-182); retrying is a loop with no
          // success state and a message that blames the network for a typo.
          return stop("missing", out.detail);
        case "gone":
        case "rejected":
          return stop("error", out.detail);
        case "failed":
          return retry(out.detail, out.requestId, "error");
        case "unreachable":
          return retry(out.detail, null, "unreachable");
      }

      const next = intervalFor(Date.now() - started.current);
      if (next === null) {
        stop("ceiling");
        return;
      }
      timer.current = setTimeout(() => void pump(), next);
    };

    const onVisible = () => {
      if (cancelled || stopped.current || document.hidden) return;
      // Becoming visible resets the TRANSPORT backoff and polls at once. It
      // does not reset the elapsed clock: elapsed time is a property of the
      // job, not of the tab.
      failures.current = 0;
      clear();
      void pump();
    };
    document.addEventListener("visibilitychange", onVisible);

    void pump();
    return () => {
      cancelled = true;
      clear();
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [id, clear]);

  // The manual check the ceiling offers. One request, and it does not restart
  // the schedule: the reason the schedule stopped has not changed.
  const checkNow = useCallback(() => {
    void (async () => {
      const out = await getJob(id);
      if (out.kind === "ok") {
        setJob(out.value);
        setPhase(out.value.status === "done" || out.value.status === "failed" ? out.value.status : "ceiling");
      } else if (out.kind === "missing") {
        setPhase("missing");
        setDetail(out.detail);
      }
    })();
  }, [id]);

  return { job, phase, detail, requestId, checkNow };
}
