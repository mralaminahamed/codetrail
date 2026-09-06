// A 400 is the caller's input being refused by a named rule. It is not an
// error: nothing broke, and there is no request id to quote. So this is not
// role="alert" and carries none — which is what a test can assert.
//
// It IS role="status", and it had no role at all until now: the panel naming
// which admission rule refused the submission was inserted into the page
// silently, so the one sentence telling a caller what to change was the one
// thing a screen reader was not told. polite rather than assertive for the same
// reason Refusal is: the caller asked for this, so interrupting them mid-word
// buys nothing.
//
// The accessible name is required, not decoration: this console has three
// role="status" regions — indexing progress, the answer outcome and this one —
// and a bare getByRole("status") in a tree holding two of them throws.
//
// The server's `detail` is rendered and never a message of this console's own.
// `rule` is a *category*: handler.go returns rule "form" for a bad path and for
// a bad ref and for five more admit details besides, so rendering the rule
// would show the same text for different refusals. Spec:258 wants the rule
// named, and it is — as a label beside the sentence that says what to change.
export default function Rejected({ rule, detail }: { rule: string; detail: string }) {
  return (
    <div className="panel panel-rejected" role="status" aria-label="Submission outcome">
      <h2>codetrail refused this submission.</h2>
      <p>{detail}</p>
      {rule !== "" && (
        <p>
          Rule: <code>{rule}</code>
        </p>
      )}
    </div>
  );
}
