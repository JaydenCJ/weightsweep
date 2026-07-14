# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- Scanners for the three major local-model caches: Hugging Face hub
  (blobs / snapshot symlink farms / refs, incl. datasets and spaces),
  Ollama (OCI-style manifests + `sha256-` blobs), and LM Studio
  (`publisher/model` file trees, split GGUFs).
- Store-neutral inventory with liveness classes: `live`, `stale` (only a
  detached HF revision links it), `orphan`, `partial` (interrupted
  downloads across all three stores' temp-file conventions).
- Cross-store duplicate correlation by sha256 with a size-collision
  prefilter: name-addressed blobs cost a readdir, unnamed files are
  hashed only when their exact size collides (`--hash auto|always|never`).
- `scan` (per-store totals, reclaimable bytes, duplicate/orphan summary),
  `dupes` and `orphans` reports, all with `--json` variants.
- `plan`: versioned JSON prune plans, every action triaged `safe`
  (orphans, partials, stale snapshot skeletons, empty repo shells) or
  `review` (stale blobs, redundant duplicate copies with a named keeper).
- `prune`: dry-run by default; `--apply`, `--include-review`, `--only ID`
  consent model; guards refuse paths outside the plan's recorded store
  roots, relative paths, kind mismatches, and files whose size changed
  since planning; already-gone entries are idempotent skips.
- `scan --verify`: re-hash name-addressed blobs and report cache
  corruption (exit code 1), correcting digests before correlation.
- Root resolution from flags, then `HF_HUB_CACHE`/`HF_HOME`/
  `OLLAMA_MODELS`, then home-directory conventions; missing stores scan
  as empty instead of erroring.
- `examples/make-demo-caches.sh`, a deterministic offline three-store
  demo tree with a cross-store duplicate, orphans, a stale revision and
  partial downloads.
- 91 deterministic offline tests (`go test ./...`) and an end-to-end
  `scripts/smoke.sh` that prints `SMOKE OK`.

[0.1.0]: https://github.com/JaydenCJ/weightsweep/releases/tag/v0.1.0
