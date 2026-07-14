// Tests for the LM Studio scanner: plain publisher/model/file trees,
// temp-download suffixes, hidden-file hygiene.
package lmstudio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func find(t *testing.T, s *inventory.StoreScan, suffix string) inventory.Blob {
	t.Helper()
	for _, b := range s.Blobs {
		if strings.HasSuffix(b.Path, suffix) {
			return b
		}
	}
	t.Fatalf("no blob ending in %q", suffix)
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

func TestScanFileOwnedByPublisherModelPair(t *testing.T) {
	root := t.TempDir()
	write(t, root, "acme/coder-7b/coder-7b-q4.gguf", "gguf-bytes")
	s, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Models != 1 {
		t.Fatalf("models = %d", s.Models)
	}
	b := find(t, s, "coder-7b-q4.gguf")
	if b.Class != inventory.ClassLive || b.Refs[0] != "acme/coder-7b" {
		t.Fatalf("blob = %+v", b)
	}
}

func TestScanSplitFilesShareOneOwner(t *testing.T) {
	// Multi-part GGUFs live in a subdirectory but still belong to the
	// same publisher/model pair — they must not inflate the model count.
	root := t.TempDir()
	write(t, root, "acme/big/parts/big-00001-of-00002.gguf", "p1")
	write(t, root, "acme/big/parts/big-00002-of-00002.gguf", "p2")
	s, _ := Scan(root)
	if s.Models != 1 {
		t.Fatalf("models = %d", s.Models)
	}
	if b := find(t, s, "big-00001-of-00002.gguf"); b.Refs[0] != "acme/big" {
		t.Fatalf("refs = %v", b.Refs)
	}
}

func TestScanPartialSuffixesClassified(t *testing.T) {
	root := t.TempDir()
	for _, suf := range []string{".partial", ".download", ".crdownload", ".tmp"} {
		write(t, root, "acme/m/w.gguf"+suf, "half")
	}
	write(t, root, "acme/m/w.gguf", "whole")
	s, _ := Scan(root)
	partials := 0
	for _, b := range s.Blobs {
		if b.Class == inventory.ClassPartial {
			partials++
		}
	}
	if partials != 4 {
		t.Fatalf("partials = %d", partials)
	}
	if b := find(t, s, "m/w.gguf"); b.Class != inventory.ClassLive {
		t.Fatalf("whole file class = %s", b.Class)
	}
}

func TestScanHiddenFilesIgnored(t *testing.T) {
	root := t.TempDir()
	write(t, root, "acme/m/w.gguf", "w")
	write(t, root, "acme/m/.DS_Store", "junk")
	write(t, root, ".internal/state.json", "junk")
	s, _ := Scan(root)
	if len(s.Blobs) != 1 {
		t.Fatalf("hidden files leaked: %+v", s.Blobs)
	}
}

func TestScanShallowFileFlaggedNotOwned(t *testing.T) {
	// A file dropped straight into the models dir is not how LM Studio
	// stores anything; keep it visible but clearly out of layout.
	root := t.TempDir()
	write(t, root, "stray.gguf", "stray")
	s, _ := Scan(root)
	b := find(t, s, "stray.gguf")
	if len(b.Refs) != 0 || !strings.Contains(b.Note, "outside publisher/model layout") {
		t.Fatalf("blob = %+v", b)
	}
	if s.Models != 0 {
		t.Fatalf("models = %d", s.Models)
	}
}

func TestScanNoDigestWithoutHashing(t *testing.T) {
	// LM Studio filenames carry no hash; the digest stays empty until
	// the correlate package decides the file is worth hashing.
	root := t.TempDir()
	write(t, root, "acme/m/w.gguf", "bytes")
	s, _ := Scan(root)
	if b := find(t, s, "w.gguf"); b.Digest != "" || b.DigestFromName {
		t.Fatalf("unexpected digest: %+v", b)
	}
}

func TestScanSymlinksNotCountedAsOwnedBytes(t *testing.T) {
	root := t.TempDir()
	target := write(t, root, "acme/m/w.gguf", "real-bytes")
	link := filepath.Join(root, "acme", "m", "alias.gguf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s, _ := Scan(root)
	if len(s.Blobs) != 1 {
		t.Fatalf("symlink double-counted: %+v", s.Blobs)
	}
}

func TestScanCountsDistinctModels(t *testing.T) {
	root := t.TempDir()
	write(t, root, "acme/a/a.gguf", "a")
	write(t, root, "acme/b/b.gguf", "b")
	write(t, root, "other/c/c.gguf", "c")
	s, _ := Scan(root)
	if s.Models != 3 || len(s.Blobs) != 3 {
		t.Fatalf("models=%d blobs=%d", s.Models, len(s.Blobs))
	}
}
