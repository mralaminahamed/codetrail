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

// The remote without its scheme, for the browser tab only. Every remote this
// API serves is https by construction (admit refuses anything else), so the
// eight characters are the same on every one of them and buy nothing in a
// 20-character tab.
function tabName(remote: string): string {
  return remote.replace(/^https:\/\//, "");
}

export default function Ask() {
  const { repo = "" } = useParams();
  const [result, setResult] = useState<Result | null>(null);
  const [inFlight, setInFlight] = useState(false);
  // Stamped with the repository it was read FOR, and read back only when the
  // two still agree. That is what makes a pending view distinguishable from a
  // failed one and a stale view impossible at the same time: on a change of
  // :repo the stamp stops matching and the outcome is null again, with no
  // setState in an effect body to reset it.
  const [read, setRead] = useState<{ of: string; outcome: Outcome<RepoDetail> } | null>(null);
  const region = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const out = await getRepo(repo);
      // The WHOLE outcome, not just the value. This used to be
      // `if (out.kind === "ok") setDetail(out.value)`, which threw away a 404
      // and a 410: the header simply vanished, and the server's eviction
      // sentence — "this repository was indexed and has since been evicted" —
      // was fetched, parsed and dropped on the floor.
      if (!cancelled) setRead({ of: repo, outcome: out });
    })();
    return () => {
      cancelled = true;
    };
  }, [repo]);

  const repoOutcome = read?.of === repo ? read.outcome : null;
  const detail = repoOutcome?.kind === "ok" ? repoOutcome.value : null;

  // Two buttons on one form, not a mode dropdown. They are two endpoints, and
  // calling the toggle "mode" beside a response field literally named `mode` —
  // which the request may not carry (read.go:452-455) — would invite exactly
  // the confusion the server refuses.
  async function run(of: "ask" | "search", q: string) {
    if (q.trim() === "") return;
    setInFlight(true);
    // The previous result goes FIRST, before the await. Without this a second
    // question rendered the first question's answer — and its citations —
    // beside the new question for as long as the embedding and the vector
    // search took. For a product whose claim is a checkable citation, showing
    // one that answers a different question is the worst available stale state.
    setResult(null);
    const outcome = of === "ask" ? await ask(repo, q) : await search(repo, q);
    setInFlight(false);
    setResult({ of, outcome } as Result);
  }

  // Focus moves in an EFFECT keyed on the result, not on the line after
  // setResult. React 19 batches a state update made in a promise continuation,
  // so at the moment run() returned the DOM had not been updated: focus landed
  // on an empty div and the answer was inserted into it silently afterwards.
  //
  // Refused and failed survived that by accident — Refusal is a role="status"
  // and ErrorPanel is a role="alert", so both announce themselves on insertion.
  // ANSWERED had neither, which made "it worked" the one outcome a screen
  // reader did not hear.
  useEffect(() => {
    if (result !== null) region.current?.focus();
  }, [result]);

  function onSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const q = String(new FormData(e.currentTarget).get("q") ?? "");
    const of = (e.nativeEvent as SubmitEvent).submitter?.getAttribute("value") === "search" ? "search" : "ask";
    void run(of, q);
  }

  const outcome = result?.outcome;
  const asked = result?.of === "ask" && result.outcome.kind === "ok" ? result.outcome.value : null;
  return (
    <>
      <PageTitle title={detail ? tabName(detail.remote) : undefined}>Ask this repository</PageTitle>
      {repoOutcome === null && <p className="pending">Reading this repository&apos;s facts…</p>}
      {repoOutcome !== null && repoOutcome.kind !== "ok" && (
        <ErrorPanel
          title="codetrail could not read this repository."
          detail={repoOutcome.detail}
          requestId={repoOutcome.kind === "failed" ? repoOutcome.requestId : null}
        />
      )}
      {detail !== null && (
        <>
          <div className="facts">
            <p>
              <code>{detail.remote}</code> at <code>{detail.commit}</code>
            </p>
            <p>{`${detail.files} files, ${detail.files_with_spans} with spans, ${detail.spans} spans.`}</p>
          </div>
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

      {/* A named region, because a div with tabIndex={-1} and no role has no
          accessible name: focusing it announces nothing at all, which is what
          made the focus fix above only half a fix. */}
      <div ref={region} tabIndex={-1} role="region" aria-label="Result">
        {/* Its own polite status rather than aria-busy on the region: aria-busy
            tells assistive tech to DEFER announcements from the subtree, which
            would suppress the one sentence a caller most needs to hear. An ask
            embeds the question and then runs a vector search, and until now the
            only sign either was happening was two buttons that had no visible
            affordance to lose. */}
        {inFlight && (
          <p className="pending" role="status" aria-label="Request progress">
            codetrail is embedding the question and searching this repository…
          </p>
        )}

        {asked !== null &&
          // The discriminant is `refused`, never whether `answer` is non-empty:
          // an answered response can carry answer: "" (answer.go:83-89), and
          // branching on the answer renders a refusal with no reason.
          (asked.refused ? (
            <Refusal
              reason={asked.reason}
              detail={asked.detail}
              floor={asked.floor}
              mode={asked.mode}
              topScore={asked.top_score}
            />
          ) : (
            <Answer answer={asked} />
          ))}

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
