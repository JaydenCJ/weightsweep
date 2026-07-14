// Package correlate joins blobs from all three stores by content digest
// and finds duplicates. The expensive part — hashing multi-gigabyte
// GGUF files — is avoided wherever possible:
//
//   - HF LFS blobs and Ollama blobs carry their sha256 in the filename,
//     so they cost a readdir, not a read.
//   - Files without a name digest (all of LM Studio, small HF etag
//     blobs) are hashed only when their exact byte size collides with
//     another blob's — a file whose size is unique in the whole
//     inventory cannot be a duplicate of anything.
//
// The same sha256 is what `ollama pull` verifies and what the HF hub
// stores for LFS files, so a cross-store match is a byte-identical file.
package correlate

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"

	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

// Group is one set of byte-identical blobs found in ≥2 places.
type Group struct {
	Digest string           `json:"digest"`
	Size   int64            `json:"size"`
	Blobs  []inventory.Blob `json:"blobs"`
	// Wasted is Size × (len(Blobs)-1): the bytes you would free by
	// keeping exactly one copy.
	Wasted int64 `json:"wasted"`
	// Stores lists the distinct stores involved, in canonical order.
	Stores []inventory.Store `json:"stores"`
}

// HashStats reports what FillDigests actually did, for --json output
// and for honest progress messages.
type HashStats struct {
	Hashed       int   `json:"hashed"`
	HashedBytes  int64 `json:"hashed_bytes"`
	SkippedCheap int   `json:"skipped_unique_size"`
}

// FillDigests computes sha256 for blobs that have no digest yet, using
// the size-collision prefilter described in the package comment. With
// hashAll set, every digest-less blob is hashed regardless (used by
// --hash always). Partial downloads are never hashed — their bytes are
// garbage by definition. Unreadable files get a warning-style note
// instead of failing the whole scan.
func FillDigests(blobs []*inventory.Blob, hashAll bool) HashStats {
	var stats HashStats
	bySize := map[int64]int{}
	for _, b := range blobs {
		if b.Class != inventory.ClassPartial {
			bySize[b.Size]++
		}
	}
	for _, b := range blobs {
		if b.Digest != "" || b.Class == inventory.ClassPartial {
			continue
		}
		if !hashAll && bySize[b.Size] < 2 {
			stats.SkippedCheap++
			continue
		}
		sum, err := hashFile(b.Path)
		if err != nil {
			b.Note = joinNote(b.Note, "unreadable, digest unknown: "+err.Error())
			continue
		}
		b.Digest = "sha256:" + sum
		b.DigestFromName = false
		stats.Hashed++
		stats.HashedBytes += b.Size
	}
	return stats
}

// Mismatch is a blob whose filename claims one digest but whose bytes
// hash to another — a corrupted or tampered cache entry.
type Mismatch struct {
	Blob   inventory.Blob `json:"blob"`
	Actual string         `json:"actual"`
}

// Verify re-hashes every blob whose digest came from its filename and
// returns the ones that do not match. Expensive by design; only run for
// --verify. Blobs are corrected in place to their actual digest so a
// corrupted copy never joins a duplicate group it does not belong to.
func Verify(blobs []*inventory.Blob) ([]Mismatch, error) {
	var out []Mismatch
	for _, b := range blobs {
		if !b.DigestFromName {
			continue
		}
		sum, err := hashFile(b.Path)
		if err != nil {
			return nil, err
		}
		actual := "sha256:" + sum
		if actual != b.Digest {
			out = append(out, Mismatch{Blob: *b, Actual: actual})
			b.Digest = actual
			b.DigestFromName = false
			b.Note = joinNote(b.Note, "digest mismatch: filename claims different content")
		}
	}
	return out, nil
}

// Groups clusters blobs by digest and returns every cluster with two or
// more members and size ≥ minSize, sorted by wasted bytes descending
// (ties broken by digest for determinism). Partial and digest-less
// blobs never join a group.
func Groups(blobs []*inventory.Blob, minSize int64) []Group {
	byDigest := map[string][]inventory.Blob{}
	for _, b := range blobs {
		if b.Digest == "" || b.Class == inventory.ClassPartial || b.Size < minSize {
			continue
		}
		byDigest[b.Digest] = append(byDigest[b.Digest], *b)
	}
	var groups []Group
	for d, members := range byDigest {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })
		g := Group{Digest: d, Size: members[0].Size, Blobs: members}
		g.Wasted = g.Size * int64(len(members)-1)
		seen := map[inventory.Store]bool{}
		for _, m := range members {
			seen[m.Store] = true
		}
		for _, st := range inventory.AllStores {
			if seen[st] {
				g.Stores = append(g.Stores, st)
			}
		}
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Wasted != groups[j].Wasted {
			return groups[i].Wasted > groups[j].Wasted
		}
		return groups[i].Digest < groups[j].Digest
	})
	return groups
}

// TotalWasted sums wasted bytes across groups.
func TotalWasted(groups []Group) int64 {
	var n int64
	for _, g := range groups {
		n += g.Wasted
	}
	return n
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
