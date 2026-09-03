// The command that makes a citation checkable without a forge. This is why the
// digest is in the tuple at all: when there is no permalink, this is the claim's
// only check, so it is a component with its own test rather than a detail of
// Citation.
//
// head -c -1 is not decoration. A span's text ends at the last byte of its last
// line, not at the newline after it, so without it sha256sum hashes one byte
// more than the digest covers and every check fails.
export function checkCommand(commit: string, path: string, start: number, end: number): string {
  return `git show ${commit}:${path} | sed -n '${start},${end}p' | head -c -1 | sha256sum`;
}

export default function CodeRef({
  commit,
  path,
  startLine,
  endLine,
  digest,
}: {
  commit: string;
  path: string;
  startLine: number;
  endLine: number;
  digest: string;
}) {
  return (
    <div>
      <p>Check it:</p>
      <pre>
        <code>{checkCommand(commit, path, startLine, endLine)}</code>
      </pre>
      <p>
        should print <code>{digest}</code>
      </p>
    </div>
  );
}
