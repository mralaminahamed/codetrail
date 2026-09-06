import { useEffect, useState } from "react";
import { Link } from "react-router";
import { listRepos } from "../api/client";
import type { Outcome, RepoList } from "../api/types";
import PageTitle from "../ui/PageTitle";
import ErrorPanel from "../ui/ErrorPanel";

export default function Corpus() {
  const [outcome, setOutcome] = useState<Outcome<RepoList> | null>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const out = await listRepos();
      if (!cancelled) setOutcome(out);
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <>
      <PageTitle>Corpus</PageTitle>
      {outcome?.kind === "ok" && (
        <>
          <p>{`${outcome.value.count} repositories, most recently used first.`}</p>
          <ul className="rows">
            {/* The server's order, rendered as given and never sorted:
                ListRepos is ORDER BY last_queried_at DESC, id
                (packages/shared/store/read.go:36) and that order is the API's
                claim, not a detail the client may improve on. */}
            {outcome.value.repos.map((r) => (
              <li key={r.id}>
                <Link to={`/repos/${r.id}`}>{r.remote}</Link> at <code>{r.ref}</code>,{" "}
                <code>{r.commit}</code>
              </li>
            ))}
          </ul>
        </>
      )}
      {outcome && outcome.kind !== "ok" && (
        <ErrorPanel
          title="codetrail could not list the corpus."
          detail={outcome.detail}
          requestId={outcome.kind === "failed" ? outcome.requestId : null}
        />
      )}
    </>
  );
}
