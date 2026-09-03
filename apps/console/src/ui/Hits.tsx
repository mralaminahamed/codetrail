import type { SearchResult } from "../api/types";
import Citation from "./Citation";
import { rankText, scoreText } from "./Score";
import { Link } from "react-router";

export default function Hits({ repo, result }: { repo: string; result: SearchResult }) {
  return (
    <div>
      <h2>Ranked spans</h2>
      <p>
        Retrieved in <code>{result.mode}</code> mode. Best cosine similarity:{" "}
        {scoreText(result.top_score)}.
      </p>
      {/* No floor on this route. Search ranks; the floor is the answer's
          decision, and reporting one here would imply a filter that did not
          run (read.go:313-314). */}
      <ol>
        {/* The server's order, rendered as given and never sorted: fused rank
            order is the API's claim. */}
        {result.hits.map((h) => (
          <li key={h.span_id}>
            <p>
              <Link to={`/repos/${repo}/spans/${h.span_id}`}>
                {`${h.path}:${h.start_line}-${h.end_line}`}
              </Link>
            </p>
            <p>
              Cosine similarity: {scoreText(h.vector_score)}. Vector rank {rankText(h.vector_rank)},
              lexical rank {rankText(h.lexical_rank)}.
            </p>
            <Citation citation={h.citation} />
          </li>
        ))}
      </ol>
    </div>
  );
}
