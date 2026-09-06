import type { Answered } from "../api/types";
import Citation from "./Citation";

export default function Answer({ answer }: { answer: Answered }) {
  return (
    <div>
      <h2>Answer</h2>
      {/* answered_by has one value until P7 and is rendered anyway:
          read.go:66-70 says the field exists so a client written before P7 can
          notice a silent downgrade, and a client that does not render it is
          that client. */}
      <p>
        Answered by <code>{answer.answered_by}</code>.
      </p>

      {answer.answer === "" ? (
        <p>codetrail assembled no citations for this answer.</p>
      ) : (
        <pre>{answer.answer}</pre>
      )}

      {answer.dropped > 0 && (
        // answer.go:48-50: an answer built from 2 of 7 spans is a different
        // claim from one built from all of them.
        <p>{`${answer.dropped} more ranked spans did not fit the answer's budget.`}</p>
      )}

      <ol className="markers">
        {answer.citations.map((c) => (
          <li key={c.span_id}>
            <p>
              [{c.marker}] {c.symbol}
            </p>
            <Citation citation={c.citation} />
          </li>
        ))}
      </ol>
    </div>
  );
}
