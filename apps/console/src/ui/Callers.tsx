import type { Callers as CallersValue } from "../api/types";
import Citation from "./Citation";
import Provenance from "./Provenance";
import Approximate from "./Approximate";

// A <ul>, not an ARIA tree. A tree promises arrow-key navigation and
// expand/collapse over a hierarchy; the payload is a flat list with a depth
// integer. Announcing a tree and serving a list is a worse a11y outcome than
// serving an honest list.
export default function Callers({ callers }: { callers: CallersValue }) {
  return (
    <>
      <section>
        <h2>Callers</h2>
        <p>{`${callers.callers.length} definitions call this one, to depth ${callers.depth}.`}</p>
        {callers.truncated && <p>This list was truncated; there are more.</p>}
        {callers.callers.length === 0 && <p>Nothing in this repository calls this definition.</p>}
        <ul>
          {callers.callers.map((c) => (
            <li key={`${c.symbol.id}:${c.call.path}:${c.call.line}`}>
              <p>
                {c.symbol.name} — <Provenance provenance={c.provenance} /> — depth {c.depth} — calls
                at <code>{`${c.call.path}:${c.call.line}`}</code>
              </p>
              <Citation citation={c.citation} symbol={c.symbol} />
            </li>
          ))}
        </ul>
      </section>
      <Approximate approximate={callers.approximate} toName={callers.symbol.name} />
    </>
  );
}
