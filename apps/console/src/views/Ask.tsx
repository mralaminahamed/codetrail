import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router";
import { ask, getRepo, search } from "../api/client";
import type { AskResult, Outcome, RepoDetail, SearchResult } from "../api/types";
import PageTitle from "../ui/PageTitle";
import Answer from "../ui/Answer";
import Refusal from "../ui/Refusal";
import Hits from "../ui/Hits";
import ErrorPanel from "../ui/ErrorPanel";
import GraphCounts from "../ui/GraphCounts";
import Staleness from "../ui/Staleness";

type Result =
  | { of: "ask"; outcome: Outcome<AskResult> }
  | { of: "search"; outcome: Outcome<SearchResult> };

export default function Ask() {
  const { repo = "" } = useParams();
  const [result, setResult] = useState<Result | null>(null);
  const [inFlight, setInFlight] = useState(false);
  const [detail, setDetail] = useState<RepoDetail | null>(null);
  const region = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const out = await getRepo(repo);
      if (!cancelled && out.kind === "ok") setDetail(out.value);
    })();
    return () => {
      cancelled = true;
    };
  }, [repo]);

  // Two buttons on one form, not a mode dropdown. They are two endpoints, and
  // calling the toggle "mode" beside a response field literally named `mode` —
  // which the request may not carry (read.go:452-455) — would invite exactly
  // the confusion the server refuses.
  async function run(of: "ask" | "search", q: string) {
    if (q.trim() === "") return;
    setInFlight(true);
    const outcome = of === "ask" ? await ask(repo, q) : await search(repo, q);
    setInFlight(false);
    setResult({ of, outcome } as Result);
    region.current?.focus();
  }

  function onSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const q = String(new FormData(e.currentTarget).get("q") ?? "");
    const of = (e.nativeEvent as SubmitEvent).submitter?.getAttribute("value") === "search" ? "search" : "ask";
    void run(of, q);
  }

  const outcome = result?.outcome;
  return (
    <>
      <PageTitle>Ask this repository</PageTitle>
      {detail !== null && (
        <>
          <p>
            <code>{detail.remote}</code> at <code>{detail.commit}</code>
          </p>
          <p>{`${detail.files} files, ${detail.files_with_spans} with spans, ${detail.spans} spans.`}</p>
          <GraphCounts repo={detail} />
          <Staleness staleness={detail.staleness} />
        </>
      )}
      <form onSubmit={onSubmit}>
        <p>
          <label htmlFor="q">Question</label>
          <input id="q" name="q" type="text" required />
        </p>
        <div className="buttons">
          <button type="submit" name="op" value="ask" data-primary disabled={inFlight}>
            Ask
          </button>
          <button type="submit" name="op" value="search" disabled={inFlight}>
            Search
          </button>
        </div>
      </form>

      <div ref={region} tabIndex={-1}>
        {result?.of === "ask" && result.outcome.kind === "ok" && (
          // The discriminant is `refused`, never whether `answer` is non-empty:
          // an answered response can carry answer: "" (answer.go:83-89), and
          // branching on the answer renders a refusal with no reason.
          result.outcome.value.refused ? (
            <Refusal
              reason={result.outcome.value.reason}
              detail={result.outcome.value.detail}
              floor={result.outcome.value.floor}
              mode={result.outcome.value.mode}
              topScore={result.outcome.value.top_score}
            />
          ) : (
            <Answer answer={result.outcome.value} />
          )
        )}

        {result?.of === "search" && result.outcome.kind === "ok" && (
          <Hits repo={repo} result={result.outcome.value} />
        )}

        {outcome && outcome.kind === "failed" && (
          <ErrorPanel title="codetrail could not answer." detail={outcome.detail} requestId={outcome.requestId} />
        )}
        {outcome && outcome.kind === "unreachable" && (
          <ErrorPanel title="codetrail could not be reached." detail={outcome.detail} requestId={null} />
        )}
        {outcome && (outcome.kind === "rejected" || outcome.kind === "missing" || outcome.kind === "gone") && (
          <ErrorPanel title="codetrail refused this question." detail={outcome.detail} requestId={null} />
        )}
      </div>
    </>
  );
}
