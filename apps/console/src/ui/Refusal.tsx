import type { Floor, RefusalReason } from "../api/types";

// The floor in words, and no scale of any kind while it is uncalibrated.
//
// spec:315 puts the floor's value in P6, and floorView.Calibrated is on the
// wire (read.go:102-113) so a reader can tell a placeholder from a threshold. A
// bar, meter, percentage or "N% above the floor" drawn from an uncalibrated
// number renders a guess as a measurement, which is exactly what spec:315
// defers.
//
// Measured consequence this must not paper over: at the shipped default of -1,
// below_floor is unreachable — Decide refuses when top < f.Value and a cosine
// similarity is never below -1 — so a default deployment only ever produces
// no_spans and unscored.
export function floorSentence(floor: Floor, mode: string, topScore: number | null): string {
  if (!floor.applicable) {
    return `This deployment retrieves in ${mode} mode, which produces no similarity score, so no floor was applied.`;
  }
  if (!floor.calibrated) {
    // No phase identifier. "the number is measured in P6" shipped to users, and
    // a reader outside this repository cannot act on a plan reference — while
    // the sentence that replaces it keeps the whole epistemic claim: the number
    // is a setting, not a finding, so this refusal reports where a knob is and
    // not that the question was unanswerable. The second half is the actionable
    // part, and Search is a button away on the same page.
    return `codetrail's score floor is ${floor.value} and has never been calibrated: it is a mechanism, not a measured threshold. This refusal reports the setting, not a judgement about the question — searching the same repository still ranks spans.`;
  }
  const best = topScore === null ? "no similarity score" : String(topScore);
  return `The best match scored ${best} against a calibrated floor of ${floor.value}.`;
}

// role="status", not role="alert". A refusal is an outcome, not a failure: the
// gateway answers 200 for exactly that reason (read.go:162-165), and filing it
// as an error would undo that at the last mile.
//
// The accessible name is required, not decoration: the Shell's indexing region
// is also a role="status", so a tree holding both makes a bare
// getByRole("status") throw.
//
// This panel carries the floor and never a request id. ErrorPanel carries a
// request id and never a floor. Neither can render the other's evidence, which
// is what makes the two distinguishable by test rather than by eye.
export default function Refusal({
  reason,
  detail,
  floor,
  mode,
  topScore,
}: {
  reason: RefusalReason;
  detail: string;
  floor: Floor;
  mode: string;
  topScore: number | null;
}) {
  return (
    <div className="panel panel-refusal" role="status" aria-label="Answer outcome">
      <h2>No answer — and no error.</h2>
      {/* The server's sentence, one per reason (read.go:386-398). Never a
          single "no answer" string: spec §10 forbids a generic refusal and
          "refused" with a bare label is one. */}
      <p>{detail}</p>
      <p>
        Reason: <code>{reason}</code>
      </p>
      <p>{floorSentence(floor, mode, topScore)}</p>
    </div>
  );
}
