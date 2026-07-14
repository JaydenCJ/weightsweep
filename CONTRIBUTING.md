# Contributing to weightsweep

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go 1.22 or newer; there are no other dependencies of any kind.

```bash
git clone https://github.com/JaydenCJ/weightsweep.git
cd weightsweep
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, generates a synthetic three-store
cache tree and drives the full scan → dupes → plan → prune lifecycle
against it; it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (all 91 tests).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   packages (`hfhub`, `ollama`, `lmstudio`, `correlate`, `plan`) rather
   than in the CLI layer.

## Ground rules

- Zero runtime dependencies is a core feature: the `go.mod` require list
  stays empty. Adding a dependency needs strong justification in the PR.
- No network calls, ever — weightsweep reads cache directories and
  deletes what an approved plan says, nothing else. No telemetry.
- Deletion stays behind the plan/guard pipeline: no code path outside
  `plan.Execute` may remove files, and every new action type needs an
  explicit safety level with a rationale.
- Scanner facts (what each store's layout guarantees) are documented in
  `docs/cache-layouts.md`; layout-behavior changes update that file in
  the same PR.
- Code comments and doc comments are written in English.

## Reporting bugs

Please include the output of `weightsweep --version`, the command line
you ran, the relevant `--json` output (paths may be redacted), and — for
misclassification reports — an `ls -laR` of the smallest cache subtree
that reproduces the wrong class.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
