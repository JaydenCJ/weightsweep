# Cache layouts weightsweep understands

This document records the on-disk facts each scanner relies on, so layout
changes upstream can be checked against something concrete. All paths are
the tools' defaults; every one of them is overridable via weightsweep's
`--hf/--ollama/--lmstudio` flags.

## Hugging Face hub cache (`~/.cache/huggingface/hub`)

```
hub/
├── models--org--name/
│   ├── blobs/
│   │   ├── <sha256>                 LFS file, named by its content hash
│   │   ├── <etag>                   small file, named by a short etag
│   │   └── <sha256>.incomplete      interrupted download
│   ├── snapshots/
│   │   └── <commit>/
│   │       └── model.safetensors -> ../../blobs/<sha256>
│   └── refs/
│       └── main                     text file: the commit `main` points at
├── datasets--org--name/ …           same shape (also spaces--)
```

Facts the scanner uses:

- `refs/*` may nest (`refs/pr/1`); file content is the commit hash.
- Snapshot entries are relative symlinks into `../../blobs/`. Some older
  clients copied files instead of linking; regular files in a snapshot own
  their bytes and are not blobs.
- A blob is **live** if any snapshot a ref points at links to it, **stale**
  if only detached snapshots link to it, **orphan** if none do.
- 64-hex filenames are trusted as sha256 (that is how the hub names LFS
  blobs). Shorter etag names are *not* content hashes and are never
  trusted; those files join duplicate detection only via computed hashes.

## Ollama store (`~/.ollama/models`, or `$OLLAMA_MODELS`)

```
models/
├── blobs/
│   ├── sha256-<hex>                 content-addressed layer/config file
│   └── sha256-<hex>-partial…        interrupted pull
└── manifests/
    └── registry.ollama.ai/library/<model>/<tag>
                                     OCI-style JSON manifest
```

Facts the scanner uses:

- Manifests are JSON with `config.digest` and `layers[].digest` in
  `sha256:<hex>` form; the referenced blob file is `sha256-<hex>`.
- `registry.ollama.ai/library/gemma3/4b` is displayed as `gemma3:4b`;
  non-default hosts/namespaces stay visible.
- A blob is **live** iff at least one manifest references its digest.
  Manifests referencing missing blobs produce warnings (broken model).
- The sha256 in the filename is the hash of the blob's full content —
  directly comparable with HF LFS names and computed hashes.

## LM Studio models (`~/.lmstudio/models`, legacy `~/.cache/lm-studio/models`)

```
models/
└── <publisher>/<model>/
    ├── file.gguf                    plain files, no blob indirection
    └── parts/file-00001-of-00002.gguf
```

Facts the scanner uses:

- There is no content-addressed layer: filenames carry no hash, so
  digests are computed — but only for files whose byte size collides
  with another blob somewhere in the inventory (or under `--hash always`).
- Ownership is the first two path segments (`publisher/model`); deeper
  nesting (split GGUFs) still belongs to that pair.
- Files directly under the root are flagged "outside publisher/model
  layout" rather than guessed at.
- Temp suffixes `.partial`, `.download`, `.crdownload`, `.tmp` mark
  interrupted downloads; hidden files and symlinks are ignored.

## Why sha256 equality means "same model file"

Ollama verifies pulled layers against their sha256; the HF hub names LFS
blobs by sha256 and huggingface_hub verifies downloads; a GGUF fetched by
LM Studio from the hub is byte-identical to the hub's LFS blob. So two
files with equal sha256 across stores are the same artifact, and one of
them is redundant — which copy to keep is a policy question (weightsweep
keeps the most-referenced copy and marks the rest `review`).

## The prune cascade

Deleting a detached snapshot directory (safe: it is symlink skeleton)
turns blobs only it referenced into orphans on the *next* scan. weightsweep
does not chain these deletions inside one plan on purpose: every plan is
verifiable against the disk as it currently is. Run `plan` again after an
apply to collect the newly orphaned blobs as safe actions.
