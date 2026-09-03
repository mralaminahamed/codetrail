import { useEffect, useState } from "react";
import { useParams } from "react-router";
import { getSpan } from "../api/client";
import type { Outcome, SpanRead } from "../api/types";
import PageTitle from "../ui/PageTitle";
import Citation from "../ui/Citation";
import ErrorPanel from "../ui/ErrorPanel";

// The in-app half of "jump to source". The forge permalink is the out-of-app
// half; a reader with no network access to the forge still has the code.
export default function Span() {
  const { repo = "", span = "" } = useParams();
  const [outcome, setOutcome] = useState<Outcome<SpanRead> | null>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const out = await getSpan(repo, span);
      if (!cancelled) setOutcome(out);
    })();
    return () => {
      cancelled = true;
    };
  }, [repo, span]);

  return (
    <>
      <PageTitle>Span</PageTitle>
      {outcome?.kind === "ok" && (
        <>
          <p>
            <code>{`${outcome.value.span.path}:${outcome.value.span.start_line}-${outcome.value.span.end_line}`}</code>{" "}
            (<code>{outcome.value.span.kind}</code>)
          </p>
          {/* The whole span, never truncated: a digest is a claim about the
              whole text, so showing part of it under a digest of all of it is a
              citation that lies (answer.go:61-64). */}
          <pre>{outcome.value.span.text}</pre>
          <Citation citation={outcome.value.citation} />
        </>
      )}
      {outcome && outcome.kind !== "ok" && (
        <ErrorPanel
          title="codetrail could not read this span."
          detail={outcome.detail}
          requestId={outcome.kind === "failed" ? outcome.requestId : null}
        />
      )}
    </>
  );
}
