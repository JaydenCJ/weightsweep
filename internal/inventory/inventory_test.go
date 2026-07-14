// Unit tests for the store-neutral inventory model: digest parsing is
// strict (it gates what joins duplicate groups), and the byte formatter
// feeds every human-facing table.
package inventory

import (
	"strings"
	"testing"
)

const goodHex = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestDigestFromHexAcceptsLowercaseSha256(t *testing.T) {
	if got := DigestFromHex(goodHex); got != "sha256:"+goodHex {
		t.Fatalf("got %q", got)
	}
}

func TestDigestFromHexRejectsMalformedNames(t *testing.T) {
	// HF and Ollama both write lowercase 64-hex blob names; anything
	// else was not minted by either store and must not be trusted as a
	// content hash — a fabricated digest fabricates duplicate matches.
	for _, bad := range []string{
		strings.ToUpper(goodHex), // uppercase
		goodHex[:63],             // too short
		goodHex + "0",            // too long
		"z" + goodHex[1:],        // non-hex
		"", "abc123",
	} {
		if got := DigestFromHex(bad); got != "" {
			t.Fatalf("malformed %q accepted as %q", bad, got)
		}
	}
}

func TestIsDigestRoundTrip(t *testing.T) {
	if !IsDigest("sha256:" + goodHex) {
		t.Fatal("well-formed digest rejected")
	}
	for _, bad := range []string{goodHex, "md5:" + goodHex, "sha256:" + goodHex[:10]} {
		if IsDigest(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestShortDigestAbbreviates(t *testing.T) {
	if got := ShortDigest("sha256:" + goodHex); got != goodHex[:12] {
		t.Fatalf("got %q", got)
	}
}

func TestShortDigestPassesThroughUnknown(t *testing.T) {
	// Values without the sha256: prefix are shown verbatim so nothing
	// masquerades as a hash abbreviation.
	if got := ShortDigest("not-a-digest"); got != "not-a-digest" {
		t.Fatalf("got %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{5 << 20, "5.0 MiB"},
		{int64(3.5 * float64(1<<30)), "3.5 GiB"},
		{1 << 40, "1.0 TiB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func scanFixture() *StoreScan {
	return &StoreScan{
		Store: HuggingFace,
		Blobs: []Blob{
			{Path: "/x/b", Size: 100, Class: ClassLive},
			{Path: "/x/a", Size: 40, Class: ClassOrphan},
			{Path: "/x/c", Size: 7, Class: ClassOrphan},
			{Path: "/x/d", Size: 3, Class: ClassPartial},
		},
	}
}

func TestStoreScanTotals(t *testing.T) {
	s := scanFixture()
	if got := s.TotalBytes(); got != 150 {
		t.Fatalf("TotalBytes = %d", got)
	}
	if got := s.BytesByClass(ClassOrphan); got != 47 {
		t.Fatalf("orphan bytes = %d", got)
	}
	if got := s.CountByClass(ClassOrphan); got != 2 {
		t.Fatalf("orphan count = %d", got)
	}
	if got := s.CountByClass(ClassStale); got != 0 {
		t.Fatalf("stale count = %d", got)
	}
}

func TestStoreScanSortIsByPath(t *testing.T) {
	s := scanFixture()
	s.Warnings = []string{"b", "a"}
	s.Sort()
	want := []string{"/x/a", "/x/b", "/x/c", "/x/d"}
	for i, b := range s.Blobs {
		if b.Path != want[i] {
			t.Fatalf("blob %d = %s, want %s", i, b.Path, want[i])
		}
	}
	if s.Warnings[0] != "a" {
		t.Fatalf("warnings not sorted: %v", s.Warnings)
	}
}
