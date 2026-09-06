import type { Degraded as DegradedValue, LLMTrace } from "../api/types";

// The two fields the console ignored for a whole phase.
//
// read.go has put `degraded` ({from, reason}) and `llm` (a trace) on both
// answerResponse and refusalResponse since P7. types.ts declared neither,
// parse.ts read neither, and nothing rendered either — so a DEGRADED answer and
// a plain extractive answer rendered byte-identically, because read.go stamps
// answered_by "extractive" on both and Answer.tsx rendered only that field.
//
// This is deliberately a SIBLING of Refusal rather than a section inside it.
// Refusal's contract is exactly "the floor and no request id", stated in its own
// file and asserted in both directions, and a degradation is evidence of a
// different kind: it says how the answer was written, not whether there is one.
//
// No gloss table for `reason`. agent/trace.go calls the stop set "a closed set,
// a metric label and a response field, so a tenth reason cannot be added
// without a series and a payload key" — and a map of eleven sentences in this
// file is a twelfth place that has to change, which would silently render the
// twelfth reason as nothing. The token is shown as the server spelled it, and
// the sentence beside it is true of every value it can take.
function Panel({ degraded }: { degraded: DegradedValue }) {
  return (
    <div className="panel panel-degraded" role="status" aria-label="Answer provenance">
      <h2>This answer is the fallback, not the model&apos;s.</h2>
      <p>
        codetrail asked <code>{degraded.from}</code> and it did not write an answer, so the
        extractive answerer did. It stopped at <code>{degraded.reason}</code>.
      </p>
      <p className="meta">
        A degraded answer and a plain extractive answer carry the same{" "}
        <code>answered_by</code>. This block is the only thing that tells them apart.
      </p>
    </div>
  );
}

function Trace({ trace }: { trace: LLMTrace }) {
  const { usage } = trace;
  return (
    <details className="trace">
      <summary>What the model did</summary>
      <dl className="tuple">
        <dt>Model</dt>
        <dd>
          <code>{trace.model}</code>
        </dd>
        <dt>Stopped at</dt>
        <dd>
          <code>{trace.stop}</code>
        </dd>
        <dt>Steps</dt>
        <dd>{trace.steps}</dd>
        <dt>Tool calls</dt>
        <dd>{trace.tool_calls}</dd>
        <dt>Tools</dt>
        <dd>
          {/* Names and durations, never arguments: an argument to search_code
              is the model's rewriting of the user's question, and the server
              does not send them (agent/trace.go:63-69). */}
          {trace.tools.length === 0
            ? "none"
            : trace.tools.map((t) => `${t.name} (${t.ms} ms)`).join(", ")}
        </dd>
        <dt>Tokens</dt>
        <dd>
          {`${usage.input_tokens} in, ${usage.output_tokens} out`}
          {usage.estimated ? " — estimated, not reported by the provider" : ""}
        </dd>
      </dl>
      {trace.citations_dropped > 0 && (
        // The citation gate, visible. Markers are built by codetrail from the
        // spans the tools returned and never parsed out of the model's text, so
        // a marker naming a span the loop never opened resolves to nothing and
        // is stripped (agent/cite.go:13-30). That it happened is worth saying.
        <p>
          {`${trace.citations_dropped} citation ${
            trace.citations_dropped === 1 ? "marker" : "markers"
          } pointed at a span the model had not opened, and ${
            trace.citations_dropped === 1 ? "was" : "were"
          } removed.`}
        </p>
      )}
    </details>
  );
}

// Renders nothing at all for the shipped default: an extractive deployment
// sends neither field, so an unconfigured console looks exactly as it did.
export default function Degraded({
  degraded,
  llm,
}: {
  degraded: DegradedValue | null;
  llm: LLMTrace | null;
}) {
  if (degraded === null && llm === null) return null;
  return (
    <>
      {degraded !== null && <Panel degraded={degraded} />}
      {llm !== null && <Trace trace={llm} />}
    </>
  );
}
