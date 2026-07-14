#!/usr/bin/env bash
# End-to-end smoke test for weightsweep. No network, idempotent, runs
# from a clean tree. This script plus 'go test ./...' is the whole
# verification story — the repository intentionally ships no CI.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

BIN="$WORKDIR/weightsweep"

echo "[1/9] build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/weightsweep) || fail "build failed"

echo "[2/9] --version matches the manifest version"
VERSION_OUT="$("$BIN" --version)"
[ "$VERSION_OUT" = "weightsweep 0.1.0" ] || fail "unexpected version output: $VERSION_OUT"

echo "[3/9] build demo caches (three stores, one shared blob)"
(cd "$WORKDIR" && bash "$ROOT/examples/make-demo-caches.sh" demo >/dev/null) \
  || fail "demo cache script failed"
HF="$WORKDIR/demo/hf/hub"
OL="$WORKDIR/demo/ollama/models"
LM="$WORKDIR/demo/lmstudio/models"
WS() { "$BIN" --hf "$HF" --ollama "$OL" --lmstudio "$LM" "$@"; }

echo "[4/9] scan sees all three stores and the cross-store duplicate"
SCAN_OUT="$(WS scan)"
echo "$SCAN_OUT" | grep -q "huggingface" || fail "scan missing huggingface row"
echo "$SCAN_OUT" | grep -q "duplicates: 1 group(s), 2 redundant copies" \
  || fail "scan did not find the duplicate group: $SCAN_OUT"
echo "$SCAN_OUT" | grep -q "hashed 1 file(s)" || fail "size-collision hashing did not run"

echo "[5/9] dupes correlates huggingface+ollama+lmstudio by sha256"
WS dupes | grep -q "huggingface+ollama+lmstudio" || fail "dupes missing cross-store group"

echo "[6/9] orphans lists orphan, stale and partial files"
ORPH_OUT="$(WS orphans)"
for want in orphan stale partial; do
  echo "$ORPH_OUT" | grep -q "$want" || fail "orphans missing class $want"
done

echo "[7/9] plan proposes safe + review actions"
WS plan --out "$WORKDIR/plan.json" | grep -q "wrote" || fail "plan not written"
grep -q '"format_version": 1' "$WORKDIR/plan.json" || fail "plan format header missing"
grep -q '"reason": "duplicate"' "$WORKDIR/plan.json" || fail "plan missing duplicate action"

echo "[8/9] prune dry run deletes nothing; --apply removes only safe actions"
ORPHAN_BLOB="$(ls "$HF/models--acme--retired-model/blobs/" | head -1)"
WS prune --plan "$WORKDIR/plan.json" | grep -q "DRY RUN" || fail "dry run banner missing"
[ -f "$HF/models--acme--retired-model/blobs/$ORPHAN_BLOB" ] || fail "dry run deleted a file"
WS prune --plan "$WORKDIR/plan.json" --apply | grep -q "freed" || fail "apply did not report"
[ ! -e "$HF/models--acme--retired-model/blobs/$ORPHAN_BLOB" ] || fail "orphan survived apply"
[ -f "$LM/acme/coder-2b-GGUF/coder-2b-q4_k_m.gguf" ] || fail "review-level duplicate was deleted"

echo "[9/9] re-apply is idempotent and the live model is intact"
WS prune --plan "$WORKDIR/plan.json" --apply >/dev/null || fail "re-apply errored"
RESCAN="$(WS scan)"
echo "$RESCAN" | grep -q "partial: 0 file(s)" || fail "partials remain after prune"
echo "$RESCAN" | grep -q "duplicates: 1 group(s)" || fail "live duplicate copies disturbed"
# Removing the stale snapshot skeleton demoted its blob stale -> orphan
# (the documented cascade); a fresh plan would now collect it as safe.
echo "$RESCAN" | grep -q "orphans:    1 file(s)" || fail "stale->orphan cascade missing"
echo "$RESCAN" | grep -q "stale: 0 file(s)" || fail "stale files remain after prune"

echo "SMOKE OK"
