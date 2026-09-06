import { Link, useParams } from "react-router";
import { useJob, type Phase } from "../hooks/useJob";
import type { Job as JobValue } from "../api/types";
import PageTitle from "../ui/PageTitle";
import Live from "../ui/Live";
import ErrorPanel from "../ui/ErrorPanel";

// One sentence per status and nothing else. The live region re-announces
// whenever its text changes, so a counter, a timestamp or an elapsed time in
// here would make a screen reader say the whole sentence again on every poll.
function announcement(phase: Phase, job: JobValue | null): string {
  switch (phase) {
    case "done":
      return "Indexing finished.";
    case "failed":
      return "Indexing failed.";
    case "missing":
      return "codetrail has no job with this id.";
    case "ceiling":
      return "Still indexing after 30 minutes. codetrail has stopped checking automatically.";
    case "unreachable":
      return "codetrail could not be reached.";
    case "error":
      return "codetrail failed while reading this job.";
    case "polling":
      return job?.status === "leased" ? "Indexing." : "Queued.";
  }
}

export default function Job() {
  const { id = "" } = useParams();
  const { job, phase, detail, requestId, checkNow } = useJob(id);

  return (
    <>
      <PageTitle>Indexing job</PageTitle>
      <Live>{announcement(phase, job)}</Live>

      {job !== null && (
        <dl className="tuple">
          <dt>Repository</dt>
          <dd>{job.remote}</dd>
          <dt>Ref</dt>
          <dd>{job.ref}</dd>
          <dt>Status</dt>
          <dd>{job.status}</dd>
        </dl>
      )}

      {phase === "done" && job !== null && job.repo_id !== "" && (
        // The first focusable element in the newly revealed region, so a
        // keyboard user's next Tab lands on it. Focus is NOT moved here: a
        // status change the user did not initiate must not take focus.
        <p>
          <Link to={`/repos/${job.repo_id}`}>Ask this repository</Link>
        </p>
      )}

      {phase === "failed" && (
        <p>
          Indexing failed. codetrail does not report why a job failed: the gateway deliberately
          withholds the indexer&apos;s stderr, which has carried filesystem paths and
          credential-bearing URLs.
        </p>
      )}

      {phase === "ceiling" && (
        <>
          <p>
            Still indexing after 30 minutes. codetrail has stopped checking automatically; a
            repository this large is possible, and so is an indexer that is not running.
          </p>
          <button type="button" data-primary onClick={checkNow}>
            Check again
          </button>
        </>
      )}

      {phase === "missing" && <p>{detail}</p>}

      {(phase === "unreachable" || phase === "error") && (
        <ErrorPanel
          title={phase === "unreachable" ? "codetrail could not be reached." : "codetrail failed while reading this job."}
          detail={detail}
          requestId={requestId}
        />
      )}
    </>
  );
}
