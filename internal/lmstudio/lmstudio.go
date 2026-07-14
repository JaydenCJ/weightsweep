// Package lmstudio scans an LM Studio models directory (usually
// ~/.lmstudio/models, formerly ~/.cache/lm-studio/models). Layout recap:
//
//	models/
//	└── <publisher>/<model>/model-file.gguf     plain files, no blob store
//	                        model-00001-of-00002.gguf  (splits allowed)
//
// LM Studio has no content-addressed layer, so files carry no digest in
// their name — weightsweep computes sha256 lazily, and only for files
// whose size collides with another blob (see the correlate package).
// Temp/interrupted downloads (".partial", ".download", ".crdownload",
// ".tmp") are classified partial; hidden files are ignored.
package lmstudio

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

var partialSuffixes = []string{".partial", ".download", ".crdownload", ".tmp"}

// Scan walks an LM Studio models root. Every regular file is a blob
// owned by its "publisher/model" directory pair; there is no orphan
// concept in this store beyond leftover partial downloads.
// A missing root is not an error: Found is false and the scan is empty.
// The root is absolutized so plans built from the scan survive a change
// of working directory.
func Scan(root string) (*inventory.StoreScan, error) {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	s := &inventory.StoreScan{Store: inventory.LMStudio, Root: root}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	s.Found = true

	models := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || d.Type()&os.ModeSymlink != 0 {
			return nil // .DS_Store and friends; links are not owned bytes
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		owner := ownerOf(filepath.ToSlash(rel))
		b := inventory.Blob{
			Store: inventory.LMStudio,
			Path:  p,
			Size:  info.Size(),
		}
		if isPartial(d.Name()) {
			b.Class = inventory.ClassPartial
			b.Note = "interrupted download"
		} else {
			b.Class = inventory.ClassLive
			if owner != "" {
				b.Refs = []string{owner}
				models[owner] = true
			} else {
				// A file sitting directly under the root (or one level
				// deep) is not how LM Studio lays models out; keep it
				// visible but flag it.
				b.Note = "outside publisher/model layout"
			}
		}
		s.Blobs = append(s.Blobs, b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Models = len(models)
	s.Sort()
	return s, nil
}

// ownerOf extracts "publisher/model" from a slash-relative path, or ""
// when the file is not at least two directories deep.
func ownerOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func isPartial(name string) bool {
	for _, suf := range partialSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}
