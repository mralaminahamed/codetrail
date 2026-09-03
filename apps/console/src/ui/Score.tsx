// A score that may not exist. null is not 0: read.go:123-128 — "0 there is a
// real similarity and would read as one". A cosine similarity of zero means
// orthogonal, which is a real and much worse answer than "we did not compute
// one".
export function scoreText(score: number | null): string {
  return score === null ? "no similarity score" : score.toFixed(4);
}

// Rank 0 is how Fuse spells "this arm did not return this span" (rag.go:52).
// An em dash, never "0", which would read as a rank.
export function rankText(rank: number): string {
  return rank === 0 ? "—" : String(rank);
}

export function Score({ score }: { score: number | null }) {
  return <span>{scoreText(score)}</span>;
}
