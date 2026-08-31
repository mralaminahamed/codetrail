-- HNSW over cosine distance. Built after 0001 rather than inside it so the
-- index is a separate, re-runnable step: on an empty table it is instant, and
-- on a populated one it is the slow part of a restore.
CREATE INDEX spans_embedding_idx ON spans
    USING hnsw (embedding vector_cosine_ops);
