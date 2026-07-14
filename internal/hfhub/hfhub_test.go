// Tests for the Hugging Face hub cache scanner, built on synthetic but
// structurally faithful cache trees: blobs/ + snapshots/<commit> symlink
// farms + refs/<name> files, exactly as huggingface_hub lays them out.
package hfhub

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

func hashHex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// repo builds one models--*/datasets--* cache repo. blobs maps blob
// filename -> content; snapshots maps commit -> (file in snapshot ->
// blob filename); refs maps refname -> commit.
func repo(t *testing.T, root, dir string, blobs map[string]string,
	snapshots map[string]map[string]string, refs map[string]string) {
	t.Helper()
	repoDir := filepath.Join(root, dir)
	for name, content := range blobs {
		p := filepath.Join(repoDir, "blobs", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for commit, files := range snapshots {
		for file, blobName := range files {
			link := filepath.Join(repoDir, "snapshots", commit, file)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(filepath.Dir(link), filepath.Join(repoDir, "blobs", blobName))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rel, link); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, commit := range refs {
		p := filepath.Join(repoDir, "refs", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(commit+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func find(t *testing.T, s *inventory.StoreScan, suffix string) inventory.Blob {
	t.Helper()
	for _, b := range s.Blobs {
		if strings.HasSuffix(b.Path, suffix) {
			return b
		}
	}
	t.Fatalf("no blob ending in %q (have %d blobs)", suffix, len(s.Blobs))
	return inventory.Blob{}
}

func TestScanMissingRootIsEmptyNotError(t *testing.T) {
	s, err := Scan(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Found || len(s.Blobs) != 0 {
		t.Fatalf("missing root should scan empty: %+v", s)
	}
}

func TestScanClassifiesLiveBlob(t *testing.T) {
	root := t.TempDir()
	weights := hashHex("weights-v1")
	repo(t, root, "models--org--bert",
		map[string]string{weights: "weights-v1"},
		map[string]map[string]string{"commitaaa": {"model.safetensors": weights}},
		map[string]string{"main": "commitaaa"})
	s, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Found || s.Models != 1 {
		t.Fatalf("found=%v models=%d", s.Found, s.Models)
	}
	b := find(t, s, weights)
	if b.Class != inventory.ClassLive {
		t.Fatalf("class = %s", b.Class)
	}
	if len(b.Refs) != 1 || b.Refs[0] != "org/bert@main" {
		t.Fatalf("refs = %v", b.Refs)
	}
	// LFS blobs are named by their sha256 — the digest is free.
	if b.Digest != "sha256:"+weights || !b.DigestFromName {
		t.Fatalf("digest = %q fromName=%v", b.Digest, b.DigestFromName)
	}
}

func TestScanShortEtagBlobHasNoDigest(t *testing.T) {
	// Non-LFS blobs are named by a short etag, not a sha256; trusting
	// that as a content hash would fabricate duplicate matches.
	root := t.TempDir()
	repo(t, root, "models--org--m",
		map[string]string{"abc123def456": `{"cfg":1}`},
		map[string]map[string]string{"c1": {"config.json": "abc123def456"}},
		map[string]string{"main": "c1"})
	s, _ := Scan(root)
	b := find(t, s, "abc123def456")
	if b.Digest != "" {
		t.Fatalf("etag blob got digest %q", b.Digest)
	}
	if b.Class != inventory.ClassLive {
		t.Fatalf("class = %s", b.Class)
	}
}

func TestScanOrphanBlobNothingLinksTo(t *testing.T) {
	root := t.TempDir()
	kept, orphan := hashHex("kept"), hashHex("orphan")
	repo(t, root, "models--org--m",
		map[string]string{kept: "kept", orphan: "orphan"},
		map[string]map[string]string{"c1": {"w.bin": kept}},
		map[string]string{"main": "c1"})
	s, _ := Scan(root)
	b := find(t, s, orphan)
	if b.Class != inventory.ClassOrphan {
		t.Fatalf("class = %s", b.Class)
	}
	if len(b.Refs) != 0 {
		t.Fatalf("orphan has refs %v", b.Refs)
	}
}

func TestScanStaleBlobOnlyDetachedRevisionLinks(t *testing.T) {
	// The classic "downloaded the model twice, the old revision is
	// still on disk" case that `hf cache scan` reports as prunable.
	root := t.TempDir()
	oldW, newW := hashHex("old-weights"), hashHex("new-weights")
	repo(t, root, "models--org--m",
		map[string]string{oldW: "old-weights", newW: "new-weights"},
		map[string]map[string]string{
			"oldcommit000": {"w.bin": oldW},
			"newcommit111": {"w.bin": newW},
		},
		map[string]string{"main": "newcommit111"})
	s, _ := Scan(root)
	if b := find(t, s, oldW); b.Class != inventory.ClassStale {
		t.Fatalf("old blob class = %s", b.Class)
	}
	if b := find(t, s, newW); b.Class != inventory.ClassLive {
		t.Fatalf("new blob class = %s", b.Class)
	}
	// The detached snapshot skeleton is reported as a prunable extra.
	if len(s.Extras) != 1 {
		t.Fatalf("extras = %+v", s.Extras)
	}
	e := s.Extras[0]
	if e.Kind != "stale-revision" || !e.IsDir {
		t.Fatalf("extra = %+v", e)
	}
	if !strings.HasSuffix(e.Path, filepath.Join("snapshots", "oldcommit000")) {
		t.Fatalf("extra path = %s", e.Path)
	}
	if !strings.Contains(e.Note, "oldcommit000"[:12]) {
		t.Fatalf("note = %q", e.Note)
	}
}

func TestScanBlobSharedByLiveAndStaleRevisionStaysLive(t *testing.T) {
	// A tokenizer that did not change between revisions is linked from
	// both; it must never be flagged stale while a ref still uses it.
	root := t.TempDir()
	tok := hashHex("tokenizer")
	oldW, newW := hashHex("old"), hashHex("new")
	repo(t, root, "models--org--m",
		map[string]string{tok: "tokenizer", oldW: "old", newW: "new"},
		map[string]map[string]string{
			"oldcommit000": {"w.bin": oldW, "tok.json": tok},
			"newcommit111": {"w.bin": newW, "tok.json": tok},
		},
		map[string]string{"main": "newcommit111"})
	s, _ := Scan(root)
	if b := find(t, s, tok); b.Class != inventory.ClassLive {
		t.Fatalf("shared blob class = %s", b.Class)
	}
}

func TestScanIncompleteDownloadIsPartial(t *testing.T) {
	root := t.TempDir()
	w := hashHex("w")
	repo(t, root, "models--org--m",
		map[string]string{
			w:                 "w",
			w + ".incomplete": "half-downloaded",
		},
		map[string]map[string]string{"c1": {"w.bin": w}},
		map[string]string{"main": "c1"})
	s, _ := Scan(root)
	if b := find(t, s, ".incomplete"); b.Class != inventory.ClassPartial {
		t.Fatalf("class = %s", b.Class)
	}
}

func TestScanLeftoverLockFileIsPartial(t *testing.T) {
	// Stray .lock files in blobs/ are prunable clutter like interrupted
	// downloads, but their note must not claim they are downloads.
	root := t.TempDir()
	w := hashHex("w")
	repo(t, root, "models--org--m",
		map[string]string{
			w:           "w",
			w + ".lock": "",
		},
		map[string]map[string]string{"c1": {"w.bin": w}},
		map[string]string{"main": "c1"})
	s, _ := Scan(root)
	b := find(t, s, ".lock")
	if b.Class != inventory.ClassPartial {
		t.Fatalf("class = %s", b.Class)
	}
	if b.Note != "leftover download lock" {
		t.Fatalf("note = %q", b.Note)
	}
}

func TestScanDanglingSnapshotLinkWarns(t *testing.T) {
	root := t.TempDir()
	w := hashHex("w")
	repo(t, root, "models--org--m",
		map[string]string{w: "w"},
		map[string]map[string]string{"c1": {"w.bin": w}},
		map[string]string{"main": "c1"})
	// Add a link whose blob is gone.
	link := filepath.Join(root, "models--org--m", "snapshots", "c1", "gone.bin")
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "deadbeef"), link); err != nil {
		t.Fatal(err)
	}
	s, _ := Scan(root)
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "dangling") {
		t.Fatalf("warnings = %v", s.Warnings)
	}
}

func TestScanNestedRefsLikePRs(t *testing.T) {
	root := t.TempDir()
	w := hashHex("pr-weights")
	repo(t, root, "models--org--m",
		map[string]string{w: "pr-weights"},
		map[string]map[string]string{"prcommit0000": {"w.bin": w}},
		map[string]string{filepath.Join("pr", "1"): "prcommit0000"})
	s, _ := Scan(root)
	b := find(t, s, w)
	if b.Class != inventory.ClassLive {
		t.Fatalf("class = %s", b.Class)
	}
	if b.Refs[0] != "org/m@pr/1" {
		t.Fatalf("refs = %v", b.Refs)
	}
}

func TestScanDatasetRepoKeepsKindPrefix(t *testing.T) {
	root := t.TempDir()
	d := hashHex("data")
	repo(t, root, "datasets--org--corpus",
		map[string]string{d: "data"},
		map[string]map[string]string{"c1": {"train.parquet": d}},
		map[string]string{"main": "c1"})
	s, _ := Scan(root)
	b := find(t, s, d)
	if b.Refs[0] != "datasets/org/corpus@main" {
		t.Fatalf("refs = %v", b.Refs)
	}
}

func TestScanEmptyRepoShellReported(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "models--org--husk"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := Scan(root)
	if len(s.Extras) != 1 || s.Extras[0].Kind != "empty-repo" {
		t.Fatalf("extras = %+v", s.Extras)
	}
}

func TestScanIgnoresNonRepoDirectories(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{".locks", "tmp", "version.txt-dir"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Models != 0 || len(s.Blobs) != 0 || len(s.Extras) != 0 {
		t.Fatalf("non-repo dirs leaked into scan: %+v", s)
	}
}

func TestScanMultipleRefsToSameCommit(t *testing.T) {
	root := t.TempDir()
	w := hashHex("w")
	repo(t, root, "models--org--m",
		map[string]string{w: "w"},
		map[string]map[string]string{"c1": {"w.bin": w}},
		map[string]string{"main": "c1", "v1.0": "c1"})
	s, _ := Scan(root)
	b := find(t, s, w)
	if len(b.Refs) != 2 || b.Refs[0] != "org/m@main" || b.Refs[1] != "org/m@v1.0" {
		t.Fatalf("refs = %v", b.Refs)
	}
}

func TestRepoDisplayNames(t *testing.T) {
	cases := map[string]string{
		"models--org--name":        "org/name",
		"models--org--multi--part": "org/multi/part",
		"datasets--org--d":         "datasets/org/d",
		"spaces--org--s":           "spaces/org/s",
		"random-dir":               "",
		"models--":                 "",
	}
	for in, want := range cases {
		if got := repoDisplay(in); got != want {
			t.Errorf("repoDisplay(%q) = %q, want %q", in, got, want)
		}
	}
}
