// Tests for the Ollama store scanner, using synthetic manifests/ +
// blobs/ trees that mirror what `ollama pull` writes.
package ollama

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// writeBlob stores content under its real sha256 name and returns the
// digest, so manifests and blobs agree exactly like in a real store.
func writeBlob(t *testing.T, root, content string) string {
	t.Helper()
	h := hashHex(content)
	dir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sha256-"+h), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + h
}

// writeManifest writes an OCI-style manifest at manifests/<relPath>
// pinning config plus layers.
func writeManifest(t *testing.T, root, relPath, config string, layers ...string) {
	t.Helper()
	type ref struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	}
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config":        ref{MediaType: "application/vnd.docker.container.image.v1+json", Digest: config},
	}
	var ls []ref
	for _, l := range layers {
		ls = append(ls, ref{MediaType: "application/vnd.ollama.image.model", Digest: l})
	}
	m["layers"] = ls
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "manifests", filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func find(t *testing.T, s *inventory.StoreScan, digest string) inventory.Blob {
	t.Helper()
	for _, b := range s.Blobs {
		if b.Digest == digest {
			return b
		}
	}
	t.Fatalf("no blob with digest %s", digest)
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

func TestScanLiveBlobReferencedByManifest(t *testing.T) {
	root := t.TempDir()
	cfg := writeBlob(t, root, `{"model_format":"gguf"}`)
	layer := writeBlob(t, root, "layer-bytes")
	writeManifest(t, root, "registry.ollama.ai/library/gemma3/4b", cfg, layer)
	s, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Models != 1 {
		t.Fatalf("models = %d", s.Models)
	}
	b := find(t, s, layer)
	if b.Class != inventory.ClassLive || b.Refs[0] != "gemma3:4b" {
		t.Fatalf("blob = %+v", b)
	}
	// Blob filenames embed the digest — no file reads needed.
	if !b.DigestFromName {
		t.Fatal("digest should come from the filename, not a read")
	}
}

func TestScanConfigBlobIsAlsoLive(t *testing.T) {
	root := t.TempDir()
	cfg := writeBlob(t, root, `{"cfg":true}`)
	layer := writeBlob(t, root, "layer")
	writeManifest(t, root, "registry.ollama.ai/library/m/latest", cfg, layer)
	s, _ := Scan(root)
	if b := find(t, s, cfg); b.Class != inventory.ClassLive {
		t.Fatalf("config blob class = %s", b.Class)
	}
}

func TestScanOrphanBlobNoManifest(t *testing.T) {
	// The classic Ollama leak: `ollama rm` a model whose layers were
	// shared, or an upgrade left layers behind.
	root := t.TempDir()
	orphan := writeBlob(t, root, "abandoned-layer")
	s, _ := Scan(root)
	b := find(t, s, orphan)
	if b.Class != inventory.ClassOrphan {
		t.Fatalf("class = %s", b.Class)
	}
}

func TestScanPartialPullClassified(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "sha256-" + hashHex("x") + "-partial-0"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := Scan(root)
	if len(s.Blobs) != 1 || s.Blobs[0].Class != inventory.ClassPartial {
		t.Fatalf("blobs = %+v", s.Blobs)
	}
	if s.Blobs[0].Digest != "" {
		t.Fatalf("partial must not carry a digest: %q", s.Blobs[0].Digest)
	}
}

func TestScanSharedLayerListsBothModels(t *testing.T) {
	// Two tags of one model share the big weights layer — deleting the
	// blob because one manifest went away would break the other.
	root := t.TempDir()
	cfg1 := writeBlob(t, root, "cfg1")
	cfg2 := writeBlob(t, root, "cfg2")
	weights := writeBlob(t, root, "shared-weights")
	writeManifest(t, root, "registry.ollama.ai/library/m/4b", cfg1, weights)
	writeManifest(t, root, "registry.ollama.ai/library/m/4b-instruct", cfg2, weights)
	s, _ := Scan(root)
	b := find(t, s, weights)
	if len(b.Refs) != 2 || b.Refs[0] != "m:4b" || b.Refs[1] != "m:4b-instruct" {
		t.Fatalf("refs = %v", b.Refs)
	}
}

func TestScanMissingBlobWarns(t *testing.T) {
	root := t.TempDir()
	cfg := writeBlob(t, root, "cfg")
	ghost := "sha256:" + hashHex("never-downloaded")
	writeManifest(t, root, "registry.ollama.ai/library/broken/latest", cfg, ghost)
	s, _ := Scan(root)
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "missing blob") {
		t.Fatalf("warnings = %v", s.Warnings)
	}
	if !strings.Contains(s.Warnings[0], "broken:latest") {
		t.Fatalf("warning does not name the model: %q", s.Warnings[0])
	}
}

func TestScanUnparsableManifestWarnsAndContinues(t *testing.T) {
	root := t.TempDir()
	good := writeBlob(t, root, "good-layer")
	cfg := writeBlob(t, root, "cfg")
	writeManifest(t, root, "registry.ollama.ai/library/ok/latest", cfg, good)
	bad := filepath.Join(root, "manifests", "registry.ollama.ai", "library", "bad", "latest")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "unparsable manifest") {
		t.Fatalf("warnings = %v", s.Warnings)
	}
	if b := find(t, s, good); b.Class != inventory.ClassLive {
		t.Fatalf("good model poisoned by bad manifest: %+v", b)
	}
}

func TestModelDisplayShapes(t *testing.T) {
	cases := map[string]string{
		"registry.ollama.ai/library/gemma3/4b":  "gemma3:4b",
		"registry.ollama.ai/jayden/tool/v2":     "jayden/tool:v2",
		"example.test/team/model/latest":        "example.test/team/model:latest",
		"registry.ollama.ai/library/a/b/latest": "a/b:latest", // nested model name
		"short/path":                            "short/path", // unexpected: shown raw
	}
	for in, want := range cases {
		if got := modelDisplay(in); got != want {
			t.Errorf("modelDisplay(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScanDeterministicOrderAndDirHygiene(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeBlob(t, root, fmt.Sprintf("layer-%d", i))
	}
	// A stray directory inside blobs/ must not be counted as a blob.
	if err := os.MkdirAll(filepath.Join(root, "blobs", "not-a-blob-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	s1, _ := Scan(root)
	s2, _ := Scan(root)
	if len(s1.Blobs) != 5 {
		t.Fatalf("directory counted as blob: %d entries", len(s1.Blobs))
	}
	for i := range s1.Blobs {
		if s1.Blobs[i].Path != s2.Blobs[i].Path {
			t.Fatalf("order differs at %d", i)
		}
	}
	for i := 1; i < len(s1.Blobs); i++ {
		if s1.Blobs[i-1].Path >= s1.Blobs[i].Path {
			t.Fatalf("not sorted: %s >= %s", s1.Blobs[i-1].Path, s1.Blobs[i].Path)
		}
	}
}
