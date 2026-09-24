# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Entries are grouped
by the date their pull requests were merged into `trunk`.

## [Unreleased]

### Changed

- Local development ports moved to codetrail's 8400 block (#22).

## 2026-09-06 — LLM tool loop, audit fixes and documentation

### Added

- P7: bounded LLM tool loop, fusion weights and incremental re-index (#14).
- Console visual design, and the console now shows the degradation the server reports (#17).

### Fixed

- Indexer data defects: symbol links, containment and a fail-open allowlist (#15).
- Gateway correctness and availability defects (#16).
- Leftover fixes that spanned the three parallel fix branches (#18).

### Changed

- Documentation brought up to what the repository actually is (#19).
- README rewritten as product documentation (#20).

## 2026-09-03 — Console, eval and deployment

### Added

- P6: the retrieval eval and its answer to whether fusion helps (#11).
- P8: container images, Terraform, CD workflow and Prometheus alerts with rule tests (#12).
- P5: the React console, with refusals, citations and a forge link only when one exists (#13).

## 2026-09-02 — Retrieval and symbol graph

### Added

- P3: retrieval, citations, extractive ask and an uncalibrated relevance floor that says so (#6).
- P4: symbol graph with per-edge provenance and a type-checker that fetches nothing (#9).

### Fixed

- P3 review follow-ups: a non-numeric integer setting now refuses to boot, plus tighter store and
  gateway tests and corrected comments (#8).

## 2026-09-01 — Ingestion and chunking

### Added

- P1: ingestion with admission, sandboxed clone, job queue, caps and LRU eviction (#1).
- Project icon and screenshots of the ingestion phase (#2).
- P2: AST chunking, embeddings, spans and a line-window fallback (#4).
