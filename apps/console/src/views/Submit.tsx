import { useEffect, useRef, useState } from "react";
import { submitRepo } from "../api/client";
import type { Outcome, Job } from "../api/types";
import PageTitle from "../ui/PageTitle";
import Rejected from "../ui/Rejected";
import ErrorPanel from "../ui/ErrorPanel";
import { Link, useNavigate } from "react-router";
import { rememberJob, readRecent } from "../ui/recent";

// recent.ts has written localStorage since P5 and nothing has ever read it back:
// readRecent was imported by its own test and by no component. Its own header
// says why it exists — "there is no endpoint that lists jobs … so the only way
// back to a job whose tab was closed is its id" — and a store nobody renders
// answers that with nothing.
//
// Read during render rather than held in state, so an accepted submission that
// does not navigate (it always does today) would still see its own row. The
// read is total: a private window, cleared site data or a browser refusing
// storage all come back as an empty list rather than a thrown render.
function Recent() {
  const jobs = readRecent();
  if (jobs.length === 0) return null;
  return (
    <section>
      <h2>Jobs you started here</h2>
      <p className="meta">
        codetrail has no endpoint that lists jobs, so this list is kept in this browser and is lost
        with the profile. The id is the only way back to a job whose tab was closed.
      </p>
      <ul className="rows">
        {jobs.map((j) => (
          <li key={j.id}>
            <Link to={`/jobs/${j.id}`}>{j.remote}</Link> at <code>{j.ref}</code> —{" "}
            <code>{j.id}</code>
          </li>
        ))}
      </ul>
    </section>
  );
}

export default function Submit() {
  const [outcome, setOutcome] = useState<Outcome<Job> | null>(null);
  const [inFlight, setInFlight] = useState(false);
  const result = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();


  async function onSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = new FormData(e.currentTarget);
    const remote = String(form.get("remote") ?? "").trim();
    // The only client-side check, and it is not validation: an empty submit is
    // a request that cannot succeed and there is no rule to learn from it.
    //
    // codetrail does NOT check the scheme or the host here. ALLOWED_HOSTS is
    // deployment configuration the browser cannot see (main.go:100-102), so a
    // client-side allowlist is either stricter than the deployment — refusing a
    // URL that would have worked, with a sentence no rule produced — or looser,
    // in which case it did nothing. The server's refusal is the answer, and
    // spec:258 says it names which rule.
    if (remote === "") return;

    setInFlight(true);
    const out = await submitRepo(remote, String(form.get("ref") ?? ""));
    setInFlight(false);
    setOutcome(out);

    if (out.kind === "ok") {
      rememberJob({ id: out.value.id, remote: out.value.remote, ref: out.value.ref });
      await navigate(`/jobs/${out.value.id}`);
    }
  }

  // Focus follows direct submission — the one place in this phase focus moves
  // without a route change, because the user asked for this. A poll never does
  // (see hooks/useJob). In an effect rather than on the line after setOutcome,
  // for the reason written out in Ask.tsx: the region was empty and unnamed at
  // the moment it took focus.
  //
  // Not on an accepted submission: that navigates to the job, and focusing a
  // region on a view about to unmount announces nothing and steals the route
  // change's own focus move.
  useEffect(() => {
    if (outcome !== null && outcome.kind !== "ok") result.current?.focus();
  }, [outcome]);

  return (
    <>
      <PageTitle>Submit a repository</PageTitle>
      <form onSubmit={onSubmit}>
        <p>
          <label htmlFor="remote">Repository URL</label>
          <input id="remote" name="remote" type="text" required />
        </p>
        <p>
          {/* Empty by default and the server fills it: handler.go:156-159
              already substitutes HEAD, and a client that sends "main" is
              guessing at a branch the repository may not have. */}
          <label htmlFor="ref">Ref (optional)</label>
          <input id="ref" name="ref" type="text" />
        </p>
        {/* Disabled only while the request is in flight. Disabling on a "looks
            invalid" heuristic is the client-side validation this view refuses,
            wearing different clothes. */}
        <button type="submit" data-primary disabled={inFlight}>
          Index this repository
        </button>
      </form>
      <div
        ref={result}
        tabIndex={-1}
        role="region"
        aria-label="Submission result"
        aria-busy={inFlight}
      >
        {outcome?.kind === "rejected" && <Rejected rule={outcome.rule} detail={outcome.detail} />}
        {outcome?.kind === "failed" && (
          <ErrorPanel title="codetrail could not accept this submission." detail={outcome.detail} requestId={outcome.requestId} />
        )}
        {outcome?.kind === "unreachable" && (
          <ErrorPanel title="codetrail could not be reached." detail={outcome.detail} requestId={null} />
        )}
      </div>
      <Recent />
    </>
  );
}
