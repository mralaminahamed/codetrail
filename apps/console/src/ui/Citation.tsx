import type { Citation as CitationValue, Symbol } from "../api/types";
import CodeRef from "./CodeRef";
import Copy from "./Copy";
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

function location(citation: CitationValue): string {
  return `${citation.path}:${citation.start_line}-${citation.end_line}`;
}

// Four rows, label left and value right, in a two-column grid.
//
// Every <dd> here holds its value AND NOTHING ELSE — the copy button is an
// <svg>-only element with no text nodes, so `dd.textContent` is still exactly
// the string the label names. That is not an accident of styling: the
// citation's tests read a value as "the dd beside the dt spelled Digest",
// which is strictly more discriminating than searching the tree for a string
// (it catches a mutant that transposed the commit and the digest), and it only
// works while a dd holds one value.
function Tuple({ citation }: { citation: CitationValue }) {
  return (
    <dl className="tuple">
      <dt>Repository</dt>
      <dd>{citation.remote}</dd>
      <dt>Commit</dt>
      <dd>
        <code>{citation.commit}</code>
        <Copy what="the commit" value={citation.commit} />
      </dd>
      <dt>Location</dt>
      <dd>
        <code>{location(citation)}</code>
      </dd>
      <dt>Digest</dt>
      <dd>
        {/* The one value a reader compares by eye against what sha256sum
            printed, 64 characters long, wrapped across two or three lines on a
            narrow viewport. Copying it by hand is the failure this button
            exists for. */}
        <code>{citation.digest}</code>
        <Copy what="the digest" value={citation.digest} />
      </dd>
    </dl>
  );
}

export default function Citation({
  citation,
  symbol,
  check = "closed",
}: {
  citation: CitationValue | null;
  symbol?: Symbol;
  // Open where the citation is the page's subject, shut where it is one row of
  // ten. See CodeRef.
  check?: "open" | "closed";
}) {
  // Rendering three of three: a definition too long for one span has no digest
  // (graph.go:335-345), but it still has a path and a line range on the symbol
  // row. Hiding the row, or rendering an empty digest, both claim something
  // false.
  if (citation === null) {
    if (!symbol) return null;
    return (
      <div className="cite cite-nodigest">
        <p className="cite-loc">
          <code>{`${symbol.path}:${symbol.start_line}-${symbol.end_line}`}</code>
        </p>
        <p className="cite-nolink">
          This declaration has no span of its own, so there is no digest to check it against.
        </p>
      </div>
    );
  }

  const hasLink = linkable(citation.permalink);
  return (
    <div className="cite">
      {/* The card's title is WHERE THE CODE IS, in mono, at the top. A citation
          is a location before it is anything else, and the previous rendering
          buried it in row three of ten undifferentiated lines. */}
      {hasLink ? (
        // Verbatim from the payload, never composed. cite.go keeps a *table* of
        // forge shapes because the two allowlisted forges genuinely differ
        // (/blob/<sha>/ against /src/commit/<sha>/), and composing here
        // re-introduces the guess the server refused to make.
        //
        // rel="noreferrer" and no target: the forge does not need to be told
        // which console page a reader came from, and a link that opens a window
        // the reader did not ask for is a 3.2.5 problem this does not need.
        <p className="cite-loc">
          <a href={citation.permalink} rel="noreferrer">
            {location(citation)}
          </a>
        </p>
      ) : (
        <>
          <p className="cite-loc">{location(citation)}</p>
          {/* Rendering two of three. Not a disabled link and not nothing: an
              <a href=""> reloads the console, and a dead link that looks live
              reads as "the code is gone" rather than "we do not know the
              shape". */}
          <p className="cite-nolink">
            codetrail does not know this forge&apos;s URL shape, so there is no link. The tuple below
            is the claim; a link would only have been a convenience.
          </p>
        </>
      )}
      <Tuple citation={citation} />
      <CodeRef
        commit={citation.commit}
        path={citation.path}
        startLine={citation.start_line}
        endLine={citation.end_line}
        digest={citation.digest}
        open={check === "open"}
      />
      <Staleness staleness={citation.staleness} />
    </div>
  );
}
