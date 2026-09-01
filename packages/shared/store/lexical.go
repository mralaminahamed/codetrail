package store

import (
	"context"
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// lexWeights is {D, C, B, A}: a symbol match is worth two and a half body
// matches. Postgres's own default is {0.1, 0.2, 0.4, 1.0} and this is it,
// written out because the weights are half of what migration 0008 buys and a
// reader should not have to know the default to see them.
const lexWeights = `{0.1, 0.2, 0.4, 1.0}`

// LexicalSearch ranks a repo's spans against the OR of terms (spec §8).
//
// terms come from rag.Terms, whose charset is letters and digits, so joining
// them with " | " cannot produce an operator the caller did not ask for. That
// is the whole reason the query text never reaches Postgres: handing a question
// about code to to_tsquery raw is a 42601 for any of &, |, !, : or (.
//
// OR, not AND: this arm exists to widen what fusion has to work with, and an
// AND over a tokenised question finds nothing as soon as one word is missing.
//
// ts_rank_cd rather than ts_rank. Under OR the cover-density algorithm does not
// run at all — measured: two spans with the same terms adjacent and 300 tokens
// apart score identically — so what actually separates the two functions here
// is that ts_rank_cd sums a term's weight per occurrence while ts_rank
// saturates and rewards matching distinct terms instead. Neither is measured
// against a corpus; spec:316 makes that P6's experiment, and a test pins which
// one ships so the swap is visible rather than silent.
func (s *Store) LexicalSearch(ctx context.Context, repoID string, terms []string, limit int) ([]models.Cite, error) {
	// No round trip: to_tsquery('simple', '') is a syntax error, and "the
	// question had no words in it" is a result, not a failure.
	if len(terms) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo_id, file_id, path, kind, symbol, start_line, end_line,
		       text, digest, ts_rank_cd('`+lexWeights+`', lex, q)::float8 AS score
		FROM spans, to_tsquery('simple', $2) AS q
		WHERE repo_id = $1 AND lex @@ q
		ORDER BY score DESC, path, start_line, id
		LIMIT $3`, repoID, strings.Join(terms, " | "), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Cite
	for rows.Next() {
		var c models.Cite
		var kind string
		var score float64
		if err := rows.Scan(&c.ID, &c.RepoID, &c.FileID, &c.Path, &kind, &c.Symbol,
			&c.StartLine, &c.EndLine, &c.Text, &c.Digest, &score); err != nil {
			return nil, err
		}
		c.Kind, c.Score = models.SpanKind(kind), float32(score)
		out = append(out, c)
	}
	return out, rows.Err()
}
