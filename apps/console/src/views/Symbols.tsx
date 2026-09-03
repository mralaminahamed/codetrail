import { useEffect, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { listSymbols } from "../api/client";
import type { Outcome, SymbolList } from "../api/types";
import PageTitle from "../ui/PageTitle";
import ErrorPanel from "../ui/ErrorPanel";

export default function Symbols() {
  const { repo = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const name = params.get("name") ?? "";
  const suffix = params.get("suffix") === "true";
  const [outcome, setOutcome] = useState<Outcome<SymbolList> | null>(null);

  useEffect(() => {
    if (name === "") return;
    let cancelled = false;
    void (async () => {
      const out = await listSymbols(repo, { name, suffix });
      if (!cancelled) setOutcome(out);
    })();
    return () => {
      cancelled = true;
    };
  }, [repo, name, suffix]);

  return (
    <>
      <PageTitle>Definitions</PageTitle>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const f = new FormData(e.currentTarget);
          setParams({
            name: String(f.get("name") ?? ""),
            ...(f.get("suffix") === "on" ? { suffix: "true" } : {}),
          });
        }}
      >
        <p>
          <label htmlFor="name">Symbol name</label>
          <input id="name" name="name" type="text" defaultValue={name} />
        </p>
        <p>
          {/* P4's Open question 11, answered here: a labelled opt-in, because
              P3's lexical arm reaches Store.Get through its parts and a user
              who found a method by searching will type Get. */}
          <input id="suffix" name="suffix" type="checkbox" defaultChecked={suffix} />
          <label htmlFor="suffix">also match a method by its last segment</label>
        </p>
        <button type="submit">Find definitions</button>
      </form>

      {outcome?.kind === "ok" && (
        <>
          <p>
            {`${outcome.value.count} definitions. `}
            {/* matched is rendered beside the results so a widening is visible
                in the answer and not only in the question (graph.go:166-169). */}
            {`Matched: ${outcome.value.matched}.`}
          </p>
          {outcome.value.truncated && <p>This list was truncated; there are more.</p>}
          <ul>
            {/* The server's order — path, then start line — rendered as given. */}
            {outcome.value.symbols.map((s) => (
              <li key={s.id}>
                <Link to={`/repos/${repo}/symbols/${s.id}`}>{s.name}</Link>{" "}
                <code>{`${s.path}:${s.start_line}-${s.end_line}`}</code> in <code>{s.pkg}</code>
              </li>
            ))}
          </ul>
        </>
      )}
      {outcome && outcome.kind !== "ok" && (
        <ErrorPanel
          title="codetrail could not list these definitions."
          detail={outcome.detail}
          requestId={outcome.kind === "failed" ? outcome.requestId : null}
        />
      )}
    </>
  );
}
