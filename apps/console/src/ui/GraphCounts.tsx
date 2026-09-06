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
    </div>
  );
}
