// Package inventory defines the store-neutral model that every cache
// scanner (Hugging Face hub, Ollama, LM Studio) produces: blobs with a
// size, an optional content digest, references that keep them alive, and
// a classification that says whether they are live, stale, orphaned or a
// leftover partial download. Everything downstream — duplicate
// correlation, orphan reports, prune plans — works on this model only.
package inventory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Store identifies which cache a blob was found in.
type Store string

const (
	HuggingFace Store = "huggingface"
	Ollama      Store = "ollama"
	LMStudio    Store = "lmstudio"
)

// AllStores lists the supported stores in canonical display order.
var AllStores = []Store{HuggingFace, Ollama, LMStudio}

// Class is the liveness classification of a cached file.
type Class string

const (
	// ClassLive means at least one live reference (a ref'd revision, a
	// tag manifest, or a model directory) points at the file.
	ClassLive Class = "live"
	// ClassStale means the file is reachable only through a detached
	// Hugging Face revision — a snapshot no ref points at anymore.
	ClassStale Class = "stale"
	// ClassOrphan means nothing references the file at all.
	ClassOrphan Class = "orphan"
	// ClassPartial means the file is an interrupted or temporary
	// download (".incomplete", "-partial", ".tmp", …).
	ClassPartial Class = "partial"
)

// Blob is one content-bearing file inside a store.
type Blob struct {
	Store Store  `json:"store"`
	Path  string `json:"path"` // absolute path on disk
	Size  int64  `json:"size"`
	// Digest is "sha256:<64 lowercase hex>" when known, "" otherwise.
	Digest string `json:"digest,omitempty"`
	// DigestFromName is true when the digest was read off the blob's
	// filename (HF LFS blobs, Ollama blobs) rather than computed.
	DigestFromName bool  `json:"digest_from_name,omitempty"`
	Class          Class `json:"class"`
	// Refs are the human-readable owners keeping the blob alive,
	// e.g. "org/model@main", "gemma3:4b", "publisher/model".
	Refs []string `json:"refs,omitempty"`
	Note string   `json:"note,omitempty"`
}

// Extra is a prunable non-blob item: a stale snapshot directory, a lock
// file, an empty repo shell. Extras carry their own on-disk size (often
// tiny — symlinks), while the bytes they strand are counted on blobs.
type Extra struct {
	Store Store  `json:"store"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir,omitempty"`
	Kind  string `json:"kind"` // "stale-revision", "empty-repo", …
	Note  string `json:"note,omitempty"`
}

// StoreScan is the result of scanning one store root.
type StoreScan struct {
	Store  Store   `json:"store"`
	Root   string  `json:"root"`
	Found  bool    `json:"found"` // root existed and looked like this store
	Models int     `json:"models"`
	Blobs  []Blob  `json:"blobs"`
	Extras []Extra `json:"extras,omitempty"`
	// Warnings are non-fatal oddities: unparsable manifests, manifests
	// pointing at missing blobs, dangling snapshot symlinks.
	Warnings []string `json:"warnings,omitempty"`
}

// TotalBytes sums the size of every blob in the scan.
func (s *StoreScan) TotalBytes() int64 {
	var n int64
	for _, b := range s.Blobs {
		n += b.Size
	}
	return n
}

// BytesByClass sums blob sizes for one classification.
func (s *StoreScan) BytesByClass(c Class) int64 {
	var n int64
	for _, b := range s.Blobs {
		if b.Class == c {
			n += b.Size
		}
	}
	return n
}

// CountByClass counts blobs with one classification.
func (s *StoreScan) CountByClass(c Class) int {
	var n int
	for _, b := range s.Blobs {
		if b.Class == c {
			n++
		}
	}
	return n
}

// Sort orders blobs by path and extras by path, making scanner output
// deterministic regardless of directory read order.
func (s *StoreScan) Sort() {
	sort.Slice(s.Blobs, func(i, j int) bool { return s.Blobs[i].Path < s.Blobs[j].Path })
	sort.Slice(s.Extras, func(i, j int) bool { return s.Extras[i].Path < s.Extras[j].Path })
	sort.Strings(s.Warnings)
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DigestFromHex returns "sha256:<hex>" if hex is a 64-char lowercase
// sha256 hex string, "" otherwise. Uppercase input is rejected on
// purpose: both HF and Ollama write lowercase names, and anything else
// is not a name this tool minted.
func DigestFromHex(hex string) string {
	if hex64.MatchString(hex) {
		return "sha256:" + hex
	}
	return ""
}

// IsDigest reports whether s is a well-formed "sha256:<64 hex>" digest.
func IsDigest(s string) bool {
	return strings.HasPrefix(s, "sha256:") && hex64.MatchString(strings.TrimPrefix(s, "sha256:"))
}

// ShortDigest abbreviates "sha256:abcdef…" to "abcdef012345" (12 hex
// chars) for human-facing tables. Unknown digests come back unchanged.
func ShortDigest(d string) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) >= 12 && h != d {
		return h[:12]
	}
	return d
}

// HumanBytes renders a byte count using binary units, one decimal for
// values ≥ 1 KiB ("1.5 MiB", "912 B"). Deterministic and locale-free.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	val := float64(n)
	for _, u := range units {
		val /= unit
		if val < unit || u == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", val, u)
		}
	}
	return fmt.Sprintf("%d B", n) // unreachable
}
