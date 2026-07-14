#!/usr/bin/env bash
# Build a synthetic three-store cache tree under ./demo-caches so you can
# try weightsweep without touching (or having) real model caches.
# Entirely offline and deterministic; safe to re-run (the tree is rebuilt
# from scratch). Uses ~88 MiB of disk.
set -euo pipefail

DEST="${1:-demo-caches}"
rm -rf "$DEST"
HF="$DEST/hf/hub"
OL="$DEST/ollama/models"
LM="$DEST/lmstudio/models"
mkdir -p "$DEST"

sha() { sha256sum "$1" | cut -d' ' -f1; }

# blob FILE SIZE_MIB SEED — deterministic filler content. The `|| true`
# absorbs the SIGPIPE that `head` sends `yes` under `set -o pipefail`.
blob() { { yes "weightsweep-demo-$3" || true; } | head -c "$(($2 * 1024 * 1024))" > "$1"; }

# The same "weights" bytes end up in all three stores — the duplicate
# weightsweep exists to find.
blob "$DEST/.w.tmp" 24 shared-weights
W_HASH="$(sha "$DEST/.w.tmp")"

# --- Hugging Face hub cache -------------------------------------------
REPO="$HF/models--acme--coder-2b"
mkdir -p "$REPO/blobs" "$REPO/refs" \
  "$REPO/snapshots/1111aaaa1111aaaa1111" "$REPO/snapshots/2222bbbb2222bbbb2222"
cp "$DEST/.w.tmp" "$REPO/blobs/$W_HASH"
printf '{"architectures":["DemoForCausalLM"]}' > "$REPO/blobs/0a1b2c3d4e5f6789"
blob "$DEST/.old.tmp" 8 old-revision-weights
cp "$DEST/.old.tmp" "$REPO/blobs/$(sha "$DEST/.old.tmp")"
printf 'partial' > "$REPO/blobs/${W_HASH}0000.incomplete"
ln -s "../../blobs/$W_HASH" "$REPO/snapshots/2222bbbb2222bbbb2222/model.gguf"
ln -s "../../blobs/0a1b2c3d4e5f6789" "$REPO/snapshots/2222bbbb2222bbbb2222/config.json"
ln -s "../../blobs/$(sha "$DEST/.old.tmp")" "$REPO/snapshots/1111aaaa1111aaaa1111/model.gguf"
printf '2222bbbb2222bbbb2222' > "$REPO/refs/main"   # old revision is detached

ORPHREPO="$HF/models--acme--retired-model"
mkdir -p "$ORPHREPO/blobs"
blob "$DEST/.orph.tmp" 5 nothing-references-this
cp "$DEST/.orph.tmp" "$ORPHREPO/blobs/$(sha "$DEST/.orph.tmp")"

# --- Ollama store ------------------------------------------------------
mkdir -p "$OL/blobs" "$OL/manifests/registry.ollama.ai/library/coder"
CFG='{"model_format":"gguf","model_family":"demo"}'
printf '%s' "$CFG" > "$DEST/.cfg.tmp"
cp "$DEST/.cfg.tmp" "$OL/blobs/sha256-$(sha "$DEST/.cfg.tmp")"
cp "$DEST/.w.tmp" "$OL/blobs/sha256-$W_HASH"
blob "$DEST/.gone.tmp" 3 layer-nobody-wants
cp "$DEST/.gone.tmp" "$OL/blobs/sha256-$(sha "$DEST/.gone.tmp")"
printf 'hal' > "$OL/blobs/sha256-${W_HASH}-partial"
cat > "$OL/manifests/registry.ollama.ai/library/coder/2b" <<EOF
{"schemaVersion":2,
 "config":{"digest":"sha256:$(sha "$DEST/.cfg.tmp")","size":${#CFG}},
 "layers":[{"mediaType":"application/vnd.ollama.image.model",
            "digest":"sha256:$W_HASH","size":$(wc -c < "$DEST/.w.tmp")}]}
EOF

# --- LM Studio ---------------------------------------------------------
mkdir -p "$LM/acme/coder-2b-GGUF"
cp "$DEST/.w.tmp" "$LM/acme/coder-2b-GGUF/coder-2b-q4_k_m.gguf"

rm -f "$DEST"/.*.tmp
echo "demo caches ready under $DEST/"
echo "try: weightsweep --hf $HF --ollama $OL --lmstudio $LM scan"
