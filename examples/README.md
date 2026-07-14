# weightsweep examples

This directory contains a script that fabricates a realistic three-store
cache tree so you can exercise every command without touching (or even
having) real model caches. Everything runs offline and is deterministic.

## 1. Build the demo caches

```bash
cd examples
bash make-demo-caches.sh          # ~88 MiB under ./demo-caches
```

The tree contains, on purpose:

- the **same 24 MiB "weights" blob in all three stores** (HF LFS blob,
  Ollama layer, LM Studio GGUF) — the cross-store duplicate;
- an HF model whose **old revision is detached** (refs moved on) — a
  stale snapshot plus 8 MiB of stale weights;
- an HF repo nothing references anymore — an **orphaned blob**;
- an Ollama layer no manifest pins — another orphan;
- two **interrupted downloads** (`.incomplete`, `-partial`).

## 2. Sweep it

```bash
alias ws='weightsweep --hf demo-caches/hf/hub \
  --ollama demo-caches/ollama/models --lmstudio demo-caches/lmstudio/models'

ws scan          # totals + duplicate/orphan summary; hashes exactly 1 file
ws dupes         # the 3-copy group, correlated by sha256
ws orphans       # orphan/stale/partial listing with reasons
```

## 3. Plan, review, prune

```bash
ws plan --out plan.json
cat plan.json    # 8 actions: 5 safe, 3 review — read what would happen
ws prune --plan plan.json            # dry run, deletes nothing
ws prune --plan plan.json --apply    # safe actions only
ws scan          # note: the stale blob is now an orphan (the cascade)
ws plan --out plan2.json && ws prune --plan plan2.json --apply
```

Review-level actions (the duplicate copies) never run without explicit
consent: `--include-review` for all of them, or `--only ws-0006` for one.

## 4. Try the guards

```bash
ws plan --out plan.json
echo extra >> demo-caches/hf/hub/models--acme--retired-model/blobs/*
ws prune --plan plan.json --apply    # that action is skipped: size changed
```

Re-running `make-demo-caches.sh` rebuilds the tree from scratch, so the
whole walkthrough is repeatable.
