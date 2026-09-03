import type { Provenance as ProvenanceValue } from "../api/types";

// A word, never only a colour. Colour is not a signal for a screen reader, a
// monochrome display or the ~8% of men with a red-green deficiency, and
// resolved-versus-syntactic is the single most important fact on a caller row.
//
// Not a title attribute either: getByText does not match one, and neither does
// a keyboard user.
export default function Provenance({ provenance }: { provenance: ProvenanceValue }) {
  return <span>{provenance}</span>;
}
