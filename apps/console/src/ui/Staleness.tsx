import type { Staleness as StalenessValue } from "../api/types";

// The note is the server's sentence and is rendered verbatim. The console
// composes nothing from indexed_at, commit or ref: cite.go's ago() is
// deliberately coarse — "Minutes would imply a freshness check that did not
// happen" (cite.go:118-119) — and a client that recomputes it undoes that
// silently. "May be out of date" is a claim about the code that nobody made and
// nothing checked; the server's claim is about codetrail's own knowledge.
export default function Staleness({ staleness }: { staleness: StalenessValue }) {
  return (
    <p className="stale">
      {staleness.state === "superseded" && (
        // A word, not a colour. Colour alone is not a signal: the badge below
        // is a box drawn around the word, never a box instead of it.
        <span className="stale-flag">superseded</span>
      )}
      {staleness.note}
    </p>
  );
}
