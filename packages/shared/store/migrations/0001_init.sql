-- Repos, files and spans. The vector column is fixed at 768 dimensions to match
-- nomic-embed-text, because an ANN index requires a known dimension. The model
-- and dim are recorded on every span anyway, and store.New refuses to start
-- against a schema whose dimension disagrees with the configured embedder —
-- two vector spaces sharing a table would rank nonsense confidently, and
-- nothing about the query would look wrong.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE repos (
    id         TEXT PRIMARY KEY,
    remote     TEXT NOT NULL,
    ref        TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (remote, commit_sha)
);

CREATE TABLE files (
    id      TEXT PRIMARY KEY,
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    path    TEXT NOT NULL,
    -- git's own blob hash: an unchanged file across commits is recognised
    -- without reading it, which is what makes incremental re-indexing cheap.
    blob    TEXT NOT NULL,
    lang    TEXT NOT NULL,
    lines   INTEGER NOT NULL,
    UNIQUE (repo_id, path)
);
CREATE INDEX files_blob_idx ON files (blob);

CREATE TABLE spans (
    id          TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    file_id     TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    symbol      TEXT NOT NULL DEFAULT '',
    -- 1-based and inclusive, matching every editor and every file:line
    -- convention. An off-by-one here is a citation pointing at the wrong code.
    start_line  INTEGER NOT NULL CHECK (start_line >= 1),
    end_line    INTEGER NOT NULL CHECK (end_line >= start_line),
    text        TEXT NOT NULL,
    digest      TEXT NOT NULL,
    embed_model TEXT NOT NULL,
    embed_dim   INTEGER NOT NULL,
    embedding   vector(768)
);
CREATE INDEX spans_repo_idx ON spans (repo_id);
CREATE INDEX spans_path_idx ON spans (repo_id, path);
