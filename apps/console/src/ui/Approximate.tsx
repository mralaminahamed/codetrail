import type { Approximate as ApproximateValue } from "../api/types";
import Citation from "./Citation";
import Provenance from "./Provenance";

// The name-matched block, with its own heading and its own count, never merged
// into the precise callers. graph.go:94-97: "A sibling field with its own
// count, never merged into callers — if a console flattens the two it does so
// knowingly." This console does not flatten them.
export default function Approximate({
  approximate,
  toName,
}: {
  approximate: ApproximateValue;
  toName: string;
}) {
  // failed is not "empty". An empty list means nothing matched; a failed query
  // means we do not know (graph.go:100-102, P4 Open question 14). Collapsing
  // them destroys the distinction the field was added for.
  if (approximate.failed) {
    return (
      <section>
        <h2>Name-matched callers</h2>
        <p>The name-matched list could not be built. The precise callers above are unaffected.</p>
      </section>
    );
  }

  return (
    <section>
      <h2>Name-matched callers</h2>
      <p>
        {`${approximate.count} definitions call something spelled `}
        <code>{toName}</code>
        {`. codetrail cannot say whether they call this one.`}
      </p>
      <p>Matched on: {approximate.matched_on}.</p>
      {approximate.truncated && <p>This list was truncated; there are more.</p>}
      {approximate.count === 0 && <p>Nothing in this repository calls a name spelled that way.</p>}
      <ul className="rows">
        {approximate.callers.map((c) => (
          <li key={`${c.symbol.id}:${c.call.path}:${c.call.line}`}>
            <p>
              {c.symbol.name} — <Provenance provenance={c.provenance} /> — calls{" "}
              <code>{c.to_name}</code> at <code>{`${c.call.path}:${c.call.line}`}</code>
            </p>
            <Citation citation={c.citation} symbol={c.symbol} />
          </li>
        ))}
      </ul>
    </section>
  );
}
