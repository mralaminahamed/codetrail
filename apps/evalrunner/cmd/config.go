package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// readChunk reads the chunk geometry the *indexer* was run with, from the same
// environment variables with the same defaults.
//
// It has to be told rather than read: nothing in the schema records which arm
// produced a corpus or whether it was stripped (P2's finding; kind=file does
// not say, and embed_model records only the embedder). The leakage probe
// checks stripping by its effect, which is stronger than the flag, but the
// geometry has nowhere to come from at all — so it is recorded from the
// environment and a reader can compare it against the recipe.
//
// CHUNK_STRATEGY is deliberately not read: the strategy is the arm's name, and
// one variable cannot describe two arms.
func readChunk() (chunk.Options, bool, error) {
	def := chunk.Defaults()
	opt := chunk.Options{Strategy: def.Strategy}
	var err error
	if opt.WindowLines, err = config.GetInt("CHUNK_WINDOW_LINES", def.WindowLines); err != nil {
		return opt, false, err
	}
	if opt.WindowOverlap, err = config.GetInt("CHUNK_WINDOW_OVERLAP", def.WindowOverlap); err != nil {
		return opt, false, err
	}
	if opt.MaxDeclLines, err = config.GetInt("CHUNK_MAX_DECL_LINES", def.MaxDeclLines); err != nil {
		return opt, false, err
	}
	v := config.Get("STRIP_DOC_COMMENTS", "false")
	strip, err := strconv.ParseBool(v)
	if err != nil {
		return opt, false, fmt.Errorf("STRIP_DOC_COMMENTS must be a boolean, got %q", v)
	}
	return opt, strip, nil
}

func retrievalConfig(cfg Config) RetrievalConfig {
	return RetrievalConfig{
		Mode: cfg.Mode, K: cfg.K, Candidates: cfg.Candidates, Split: cfg.Split,
		Limit: cfg.Limit, Ks: cfg.Ks,
		FloorValue: cfg.Floor.Value, FloorCalibrated: cfg.Floor.Calibrated,
		AnswerMaxSpans: cfg.Budget.MaxSpans, AnswerMaxChars: cfg.Budget.MaxChars,
		EmbedProvider: config.Get("EMBED_PROVIDER", "ollama"),
		EmbedModel:    cfg.Emb.Model(), EmbedDim: cfg.Emb.Dim(),
	}
}

func armConfig(a *openedArm, opt chunk.Options, strip bool, counters string) (ArmConfig, error) {
	c := ArmConfig{
		Name: a.Arm.Name, Database: a.Arm.Corpus.Database(),
		Strategy: a.Arm.Name, Strip: strip,
		WindowLines: opt.WindowLines, WindowOverlap: opt.WindowOverlap, MaxDeclLines: opt.MaxDeclLines,
		Files: a.Arm.Stats.Files, FilesWithSpans: a.Arm.Stats.FilesWithSpans, Spans: a.Arm.Stats.Spans,
	}
	if counters == "" {
		return c, nil
	}
	for _, part := range strings.Split(counters, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return c, fmt.Errorf("-%s-counters: %q is not name=number", a.Arm.Name, part)
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return c, fmt.Errorf("-%s-counters: %q: %w", a.Arm.Name, part, err)
		}
		switch strings.TrimSpace(k) {
		case "vanished":
			c.Vanished = n
		case "unstrippable":
			c.Unstrippable = n
		case "tokenless":
			c.Tokenless = n
		case "unparsed":
			c.Unparsed = n
		default:
			// A closed set, because a typo would otherwise be recorded as
			// nothing and read as a zero.
			return c, fmt.Errorf("-%s-counters: unknown counter %q", a.Arm.Name, k)
		}
	}
	return c, nil
}

// readPostgres reads the versions and the two ANN settings rather than quoting
// them. infra/docker-compose.yml pins ollama by digest and postgres by the
// floating tag pgvector/pgvector:pg17, so a claim like "pgvector 0.8.6 (pinned
// image)" describes a container that may already have moved — and
// RETRIEVAL_CANDIDATES=40 is justified by hnsw.ef_search's boot value.
func readPostgres(ctx context.Context, s *store.Store) (PostgresInfo, error) {
	var pg PostgresInfo
	row := s.Pool().QueryRow(ctx, `
		SELECT version(),
		       coalesce((SELECT extversion FROM pg_extension WHERE extname = 'vector'), ''),
		       coalesce((SELECT setting FROM pg_settings WHERE name = 'hnsw.ef_search'), ''),
		       coalesce((SELECT setting FROM pg_settings WHERE name = 'hnsw.iterative_scan'), '')`)
	if err := row.Scan(&pg.Version, &pg.PgvectorVersion, &pg.HNSWEfSearch, &pg.HNSWIterativeScan); err != nil {
		return pg, fmt.Errorf("reading postgres settings: %w", err)
	}
	return pg, nil
}
