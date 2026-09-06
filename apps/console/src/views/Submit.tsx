import { useRef, useState } from "react";
import { useNavigate } from "react-router";
import { submitRepo } from "../api/client";
import type { Outcome, Job } from "../api/types";
import PageTitle from "../ui/PageTitle";
import Rejected from "../ui/Rejected";
import ErrorPanel from "../ui/ErrorPanel";
import { rememberJob } from "../ui/recent";

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
    // Focus follows direct submission — the one place in this phase focus moves
    // without a route change, because the user asked for this. A poll never
    // does (see hooks/useJob).
    result.current?.focus();

    if (out.kind === "ok") {
      rememberJob({ id: out.value.id, remote: out.value.remote, ref: out.value.ref });
      await navigate(`/jobs/${out.value.id}`);
    }
  }

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
      <div ref={result} tabIndex={-1}>
        {outcome?.kind === "rejected" && <Rejected rule={outcome.rule} detail={outcome.detail} />}
        {outcome?.kind === "failed" && (
          <ErrorPanel title="codetrail could not accept this submission." detail={outcome.detail} requestId={outcome.requestId} />
        )}
        {outcome?.kind === "unreachable" && (
          <ErrorPanel title="codetrail could not be reached." detail={outcome.detail} requestId={null} />
        )}
      </div>
    </>
  );
}
