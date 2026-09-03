import type { Citation as CitationValue, Symbol } from "../api/types";
import CodeRef from "./CodeRef";
import Staleness from "./Staleness";

// The permalink is rendered as a link only when it is one. The shipped code
// cannot currently produce a non-https permalink — Permalink re-admits the
// remote through admit.Check (cite.go:180-183), which refuses any other scheme
// — so this guard is defence against a future forges entry, not against
// anything reachable today. One line, and a corpus string reaching an href is
// the shape of an XSS.
function linkable(permalink: string): boolean {
  return permalink.startsWith("https://");
}

function Tuple({ citation }: { citation: CitationValue }) {
  return (
    <dl>
      <dt>Repository</dt>
      <dd>{citation.remote}</dd>
      <dt>Commit</dt>
      <dd>
        <code>{citation.commit}</code>
      </dd>
      <dt>Location</dt>
      <dd>
        <code>{`${citation.path}:${citation.start_line}-${citation.end_line}`}</code>
      </dd>
      <dt>Digest</dt>
      <dd>
        <code>{citation.digest}</code>
      </dd>
    </dl>
  );
}

export default function Citation({
  citation,
  symbol,
}: {
  citation: CitationValue | null;
  symbol?: Symbol;
}) {
  // Rendering three of three: a definition too long for one span has no digest
  // (graph.go:335-345), but it still has a path and a line range on the symbol
  // row. Hiding the row, or rendering an empty digest, both claim something
  // false.
  if (citation === null) {
    if (!symbol) return null;
    return (
      <div>
        <p>
          <code>{`${symbol.path}:${symbol.start_line}-${symbol.end_line}`}</code>
        </p>
        <p>This declaration has no span of its own, so there is no digest to check it against.</p>
      </div>
    );
  }

  const hasLink = linkable(citation.permalink);
  return (
    <div>
      {hasLink ? (
        // Verbatim from the payload, never composed. cite.go keeps a *table* of
        // forge shapes because the two allowlisted forges genuinely differ
        // (/blob/<sha>/ against /src/commit/<sha>/), and composing here
        // re-introduces the guess the server refused to make.
        <p>
          <a href={citation.permalink}>
            {`${citation.path}:${citation.start_line}-${citation.end_line}`}
          </a>
        </p>
      ) : (
        // Rendering two of three. Not a disabled link and not nothing: an
        // <a href=""> reloads the console, and a dead link that looks live
        // reads as "the code is gone" rather than "we do not know the shape".
        <p>
          codetrail does not know this forge&apos;s URL shape, so there is no link. The tuple below
          is the claim; a link would only have been a convenience.
        </p>
      )}
      <Tuple citation={citation} />
      <CodeRef
        commit={citation.commit}
        path={citation.path}
        startLine={citation.start_line}
        endLine={citation.end_line}
        digest={citation.digest}
      />
      <Staleness staleness={citation.staleness} />
    </div>
  );
}
