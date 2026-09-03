// role="alert", and the interruption is the point: this is codetrail saying it
// broke. The request id is what ties a caller's report to the log line the
// operator reads the real error from (handler.go:189-197), so it is rendered
// whenever there is one and its absence is stated when there is not.
//
// This panel never carries a floor and never carries a refusal reason. That is
// the assertable difference between it and Refusal, and it is what stops "both
// are boxes with text in them" from passing both tests.
export default function ErrorPanel({
  title,
  detail,
  requestId,
}: {
  title: string;
  detail: string;
  requestId: string | null;
}) {
  return (
    <div role="alert">
      <h2>{title}</h2>
      <p>{detail}</p>
      {requestId === null ? (
        <p>codetrail did not return a request id for this failure.</p>
      ) : (
        <p>
          Quote request id <code>{requestId}</code>.
        </p>
      )}
    </div>
  );
}
