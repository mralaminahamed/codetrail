import Copy from "./Copy";

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

// Folded into a <details>, and which way it opens is the caller's decision
// rather than a constant, because the same component is the subject of one page
// and a footnote on another. On /repos/:repo/spans/:span the check IS the page,
// so it is open. In a ten-hit search result ten open checks are thirty lines of
// shell nobody asked for, so they are shut — and shut is not hidden: the
// summary is a control, it is in the tab order, and the command is still in the
// DOM for find-in-page.
export default function CodeRef({
  commit,
  path,
  startLine,
  endLine,
  digest,
  open = false,
}: {
  commit: string;
  path: string;
  startLine: number;
  endLine: number;
  digest: string;
  open?: boolean;
}) {
  const command = checkCommand(commit, path, startLine, endLine);
  return (
    <details className="check" open={open}>
      <summary>Check it</summary>
      <div className="codeblock">
        <pre>
          <code>{command}</code>
        </pre>
        <Copy what="the check command" value={command} className="copy copy-float" />
      </div>
      <p>
        should print <code>{digest}</code>
      </p>
    </details>
  );
}
