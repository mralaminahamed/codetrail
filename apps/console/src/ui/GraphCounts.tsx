import { Link } from "react-router";
import type { RepoDetail } from "../api/types";

// P4's Open question 13, answered by a consumer: four numbers and a sentence,
// never a percentage badge. The aggregate is a per-repo fact and `provenance`
// is a per-row label (read.go:218-223); a badge is read as a quality score,
// and the numbers are read as counts, which is what they are.
export default function GraphCounts({ repo }: { repo: RepoDetail }) {
  return (
    <div className="meta">
      <p>
        {`${repo.symbols} definitions, ${repo.edges} call edges: ${repo.edges_resolved} resolved and ${repo.edges_syntactic} syntactic.`}
      </p>
      <p>
        {`${repo.edges_resolved} of ${repo.edges} call edges name a definition in this repository; the rest name something codetrail could not resolve.`}
      </p>
      {/* routes.tsx has registered the two symbol routes since P4 and nothing
          in the tree linked to either: seven <Link to=…> and not one of them
          reached the graph these numbers describe. The numbers are where the
          link belongs — a reader who has just been told there are 1,234
          definitions is the reader who wants to open one. */}
      <p>
        <Link to={`/repos/${repo.id}/symbols`}>Find a definition and who calls it</Link>
      </p>
    </div>
  );
}
