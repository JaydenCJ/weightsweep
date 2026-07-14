// Tests for cross-store correlation: the size-collision prefilter must
// hash exactly what it needs to and nothing else, and duplicate groups
// must be deterministic.
package correlate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// blobFile writes content to disk and returns a live blob for it,
// optionally pre-filling the digest as if the store named it by hash.
func blobFile(t *testing.T, dir, name, content string, store inventory.Store, named bool) *inventory.Blob {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &inventory.Blob{Store: store, Path: p, Size: int64(len(content)), Class: inventory.ClassLive}
	if named {
		b.Digest = hashOf(content)
		b.DigestFromName = true
	}
	return b
}

func TestFillDigestsHashesOnlySizeCollisions(t *testing.T) {
	dir := t.TempDir()
	// Same length ("seven77" vs "eight88" are both 7 bytes) → both hashed.
	a := blobFile(t, dir, "a", "seven77", inventory.LMStudio, false)
	b := blobFile(t, dir, "b", "eight88", inventory.LMStudio, false)
	// Unique size → must be skipped, not read.
	c := blobFile(t, dir, "c", "this-length-is-unique-here", inventory.LMStudio, false)
	stats := FillDigests([]*inventory.Blob{a, b, c}, false)
	if a.Digest == "" || b.Digest == "" {
		t.Fatalf("colliding sizes not hashed: %+v %+v", a, b)
	}
	if c.Digest != "" {
		t.Fatalf("unique size was hashed: %+v", c)
	}
	if stats.Hashed != 2 || stats.SkippedCheap != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestFillDigestsCollisionWithNamedBlobTriggersHash(t *testing.T) {
	// An Ollama blob already knows its hash; an LM Studio file of the
	// same size must be hashed so the pair can be compared.
	dir := t.TempDir()
	named := blobFile(t, dir, "sha", "same-bytes", inventory.Ollama, true)
	anon := blobFile(t, dir, "w.gguf", "same-bytes", inventory.LMStudio, false)
	FillDigests([]*inventory.Blob{named, anon}, false)
	if anon.Digest != named.Digest {
		t.Fatalf("digests differ: %q vs %q", anon.Digest, named.Digest)
	}
	if anon.DigestFromName {
		t.Fatal("computed digest mislabelled as from-name")
	}
}

func TestFillDigestsHashAllIgnoresPrefilter(t *testing.T) {
	dir := t.TempDir()
	c := blobFile(t, dir, "c", "totally-unique-size-string!", inventory.LMStudio, false)
	stats := FillDigests([]*inventory.Blob{c}, true)
	if c.Digest != hashOf("totally-unique-size-string!") {
		t.Fatalf("digest = %q", c.Digest)
	}
	if stats.Hashed != 1 || stats.HashedBytes != c.Size {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestFillDigestsIgnoresPartialsCompletely(t *testing.T) {
	// A half-downloaded file is garbage bytes: it is never hashed, and
	// its current size must not force a real blob into a "collision"
	// that gets that blob hashed either.
	dir := t.TempDir()
	a := blobFile(t, dir, "a", "same-size", inventory.LMStudio, false)
	p := blobFile(t, dir, "p", "same-size", inventory.LMStudio, false)
	p.Class = inventory.ClassPartial
	stats := FillDigests([]*inventory.Blob{a, p}, false)
	if a.Digest != "" || stats.Hashed != 0 {
		t.Fatalf("partial size collision hashed a blob: %+v stats=%+v", a, stats)
	}
	FillDigests([]*inventory.Blob{a, p}, true)
	if p.Digest != "" {
		t.Fatalf("partial was hashed under hashAll: %+v", p)
	}
}

func TestFillDigestsUnreadableFileGetsNoteNotError(t *testing.T) {
	dir := t.TempDir()
	a := blobFile(t, dir, "a", "same-size", inventory.LMStudio, false)
	b := blobFile(t, dir, "b", "same-size", inventory.LMStudio, false)
	if err := os.Remove(b.Path); err != nil {
		t.Fatal(err)
	}
	FillDigests([]*inventory.Blob{a, b}, false)
	if b.Digest != "" || !strings.Contains(b.Note, "unreadable") {
		t.Fatalf("blob = %+v", b)
	}
	if a.Digest == "" {
		t.Fatal("readable sibling should still be hashed")
	}
}

func TestGroupsFindsCrossStoreDuplicates(t *testing.T) {
	dir := t.TempDir()
	hf := blobFile(t, dir, "hf", "weights", inventory.HuggingFace, true)
	ol := blobFile(t, dir, "ol", "weights", inventory.Ollama, true)
	lm := blobFile(t, dir, "lm", "weights", inventory.LMStudio, false)
	lm.Digest = hashOf("weights")
	solo := blobFile(t, dir, "solo", "different", inventory.Ollama, true)
	groups := Groups([]*inventory.Blob{hf, ol, lm, solo}, 0)
	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	g := groups[0]
	if len(g.Blobs) != 3 || g.Wasted != 2*int64(len("weights")) {
		t.Fatalf("group = %+v", g)
	}
	want := []inventory.Store{inventory.HuggingFace, inventory.Ollama, inventory.LMStudio}
	for i, st := range g.Stores {
		if st != want[i] {
			t.Fatalf("stores = %v", g.Stores)
		}
	}
	// Members come back path-sorted for stable output.
	for i := 1; i < len(g.Blobs); i++ {
		if g.Blobs[i-1].Path >= g.Blobs[i].Path {
			t.Fatalf("members not path-sorted: %+v", g.Blobs)
		}
	}
}

func TestGroupsSameStoreDuplicatesCount(t *testing.T) {
	// Two LM Studio quant folders holding identical bytes is still
	// wasted space even though only one store is involved.
	dir := t.TempDir()
	a := blobFile(t, dir, "a", "same", inventory.LMStudio, false)
	b := blobFile(t, dir, "b", "same", inventory.LMStudio, false)
	a.Digest, b.Digest = hashOf("same"), hashOf("same")
	groups := Groups([]*inventory.Blob{a, b}, 0)
	if len(groups) != 1 || len(groups[0].Stores) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestGroupsIgnoresDigestlessAndPartials(t *testing.T) {
	dir := t.TempDir()
	a := blobFile(t, dir, "a", "same", inventory.LMStudio, false) // no digest
	b := blobFile(t, dir, "b", "same", inventory.LMStudio, false)
	b.Digest = hashOf("same")
	p := blobFile(t, dir, "p", "same", inventory.Ollama, true)
	p.Class = inventory.ClassPartial
	if groups := Groups([]*inventory.Blob{a, b, p}, 0); len(groups) != 0 {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestGroupsMinSizeFilters(t *testing.T) {
	dir := t.TempDir()
	a := blobFile(t, dir, "a", "tiny", inventory.Ollama, true)
	b := blobFile(t, dir, "b", "tiny", inventory.HuggingFace, true)
	if groups := Groups([]*inventory.Blob{a, b}, 1024); len(groups) != 0 {
		t.Fatalf("min-size ignored: %+v", groups)
	}
	if groups := Groups([]*inventory.Blob{a, b}, 4); len(groups) != 1 {
		t.Fatalf("boundary size excluded: %+v", groups)
	}
}

func TestGroupsSortedByWastedThenDigest(t *testing.T) {
	dir := t.TempDir()
	big1 := blobFile(t, dir, "b1", "large-content-here", inventory.Ollama, true)
	big2 := blobFile(t, dir, "b2", "large-content-here", inventory.HuggingFace, true)
	small1 := blobFile(t, dir, "s1", "small", inventory.Ollama, true)
	small2 := blobFile(t, dir, "s2", "small", inventory.LMStudio, true)
	groups := Groups([]*inventory.Blob{small1, big1, small2, big2}, 0)
	if len(groups) != 2 || groups[0].Wasted < groups[1].Wasted {
		t.Fatalf("groups out of order: %+v", groups)
	}
	if TotalWasted(groups) != groups[0].Wasted+groups[1].Wasted {
		t.Fatal("TotalWasted mismatch")
	}
}

func TestVerifyDetectsCorruptedBlob(t *testing.T) {
	dir := t.TempDir()
	good := blobFile(t, dir, "good", "intact", inventory.Ollama, true)
	bad := blobFile(t, dir, "bad", "original", inventory.Ollama, true)
	// Corrupt the file after it was "named" by its original hash.
	if err := os.WriteFile(bad.Path, []byte("bitrot!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	mismatches, err := Verify([]*inventory.Blob{good, bad})
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %+v", mismatches)
	}
	if mismatches[0].Actual != hashOf("bitrot!!") {
		t.Fatalf("actual = %q", mismatches[0].Actual)
	}
	// The blob is corrected in place so it cannot join a wrong group.
	if bad.Digest != hashOf("bitrot!!") || bad.DigestFromName {
		t.Fatalf("blob not corrected: %+v", bad)
	}
	if good.Digest != hashOf("intact") {
		t.Fatalf("good blob disturbed: %+v", good)
	}
}

func TestVerifySkipsComputedDigests(t *testing.T) {
	dir := t.TempDir()
	b := blobFile(t, dir, "b", "content", inventory.LMStudio, false)
	b.Digest = hashOf("content") // computed, not from filename
	if err := os.Remove(b.Path); err != nil {
		t.Fatal(err)
	}
	// Verify must not try to read it: computed digests are already real.
	if _, err := Verify([]*inventory.Blob{b}); err != nil {
		t.Fatalf("verify read a computed-digest blob: %v", err)
	}
}
