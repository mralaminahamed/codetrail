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
  // Stamped with the span it was read for. Navigating from one span to another
  // therefore shows "reading…" rather than the previous span's text under the
  // new span's URL — and it cannot show the old one at all, because the stamp
  // no longer matches.
  const key = `${repo}/${span}`;
  const [read, setRead] = useState<{ of: string; outcome: Outcome<SpanRead> } | null>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const out = await getSpan(repo, span);
      if (!cancelled) setRead({ of: `${repo}/${span}`, outcome: out });
    })();
    return () => {
      cancelled = true;
    };
  }, [repo, span]);

  const outcome = read?.of === key ? read.outcome : null;

  const ok = outcome?.kind === "ok" ? outcome.value : null;
  return (
    <>
      <PageTitle title={ok ? `${ok.span.path}:${ok.span.start_line}-${ok.span.end_line}` : undefined}>
        Span
      </PageTitle>
      {outcome === null && <p className="pending">Reading the span…</p>}
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
          <Citation citation={outcome.value.citation} check="open" />
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
