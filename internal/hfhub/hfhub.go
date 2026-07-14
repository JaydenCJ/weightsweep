// Package hfhub scans a Hugging Face hub cache (the directory usually at
// ~/.cache/huggingface/hub). Layout recap:
//
//	hub/
//	├── models--org--name/
//	│   ├── blobs/<etag>            actual bytes; LFS blobs are named by
//	│   │                           their sha256, small files by a short etag
//	│   ├── snapshots/<commit>/…    symlink farms into ../../blobs
//	│   └── refs/<refname>          text file holding a commit hash
//	└── datasets--org--name/ …      same shape for datasets and spaces
//
// A blob is live when a snapshot that a ref points at links to it, stale
// when only detached snapshots (no ref) link to it, and orphaned when no
// snapshot links to it at all. `*.incomplete` files in blobs/ are
// interrupted downloads.
package hfhub

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

var repoPrefixes = []string{"models--", "datasets--", "spaces--"}

// Scan walks a hub cache root and classifies every blob it holds.
// A missing root is not an error: Found is false and the scan is empty.
// The root is absolutized so plans built from the scan survive a change
// of working directory.
func Scan(root string) (*inventory.StoreScan, error) {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	s := &inventory.StoreScan{Store: inventory.HuggingFace, Root: root}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	s.Found = true
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if repoDisplay(e.Name()) == "" {
			continue // not a models--/datasets--/spaces-- directory
		}
		s.Models++
		if err := scanRepo(s, filepath.Join(root, e.Name()), repoDisplay(e.Name())); err != nil {
			return nil, err
		}
	}
	s.Sort()
	return s, nil
}

// repoDisplay turns "models--org--name" into "org/name" (models keep no
// prefix; datasets and spaces keep one so the kind stays visible).
// Returns "" when the directory is not a cache repo at all.
func repoDisplay(dir string) string {
	for _, p := range repoPrefixes {
		if strings.HasPrefix(dir, p) {
			name := strings.ReplaceAll(strings.TrimPrefix(dir, p), "--", "/")
			if name == "" {
				return ""
			}
			if p == "models--" {
				return name
			}
			return strings.TrimSuffix(p, "--") + "/" + name
		}
	}
	return ""
}

func scanRepo(s *inventory.StoreScan, repoDir, display string) error {
	refs, err := readRefs(filepath.Join(repoDir, "refs"))
	if err != nil {
		return err
	}

	// Map blob path -> referencing revisions, split live/stale.
	liveRefs := map[string][]string{}  // blob path -> "display@refname"
	staleRefs := map[string][]string{} // blob path -> "display@commit12"
	snapDir := filepath.Join(repoDir, "snapshots")
	revs, _ := os.ReadDir(snapDir)
	liveRevs := 0
	for _, rev := range revs {
		if !rev.IsDir() {
			continue
		}
		commit := rev.Name()
		names := refs[commit] // refnames pointing at this commit
		revDir := filepath.Join(snapDir, commit)
		linked, err := snapshotBlobs(revDir)
		if err != nil {
			return err
		}
		for _, l := range linked {
			if l.dangling {
				s.Warnings = append(s.Warnings,
					"huggingface: dangling snapshot link "+l.linkPath+" -> missing blob")
				continue
			}
			if len(names) > 0 {
				for _, n := range names {
					liveRefs[l.blobPath] = append(liveRefs[l.blobPath], display+"@"+n)
				}
			} else {
				staleRefs[l.blobPath] = append(staleRefs[l.blobPath], display+"@"+short(commit))
			}
		}
		if len(names) > 0 {
			liveRevs++
		} else {
			size, _ := dirOwnSize(revDir)
			s.Extras = append(s.Extras, inventory.Extra{
				Store: inventory.HuggingFace,
				Path:  revDir,
				Size:  size,
				IsDir: true,
				Kind:  "stale-revision",
				Note:  display + " revision " + short(commit) + " (no ref points here)",
			})
		}
	}

	// Every file in blobs/ becomes a Blob with a classification.
	blobDir := filepath.Join(repoDir, "blobs")
	blobEntries, _ := os.ReadDir(blobDir)
	for _, be := range blobEntries {
		if be.IsDir() {
			continue
		}
		p := filepath.Join(blobDir, be.Name())
		info, err := be.Info()
		if err != nil {
			continue // raced away; nothing to report
		}
		b := inventory.Blob{
			Store: inventory.HuggingFace,
			Path:  p,
			Size:  info.Size(),
		}
		switch {
		case strings.HasSuffix(be.Name(), ".incomplete"):
			b.Class = inventory.ClassPartial
			b.Note = "interrupted download"
		case strings.HasSuffix(be.Name(), ".lock"):
			b.Class = inventory.ClassPartial
			b.Note = "leftover download lock"
		case len(liveRefs[p]) > 0:
			b.Class = inventory.ClassLive
			b.Refs = dedupeSorted(liveRefs[p])
		case len(staleRefs[p]) > 0:
			b.Class = inventory.ClassStale
			b.Refs = dedupeSorted(staleRefs[p])
			b.Note = "reachable only from detached revisions"
		default:
			b.Class = inventory.ClassOrphan
			b.Note = "no snapshot links to this blob"
		}
		if d := inventory.DigestFromHex(be.Name()); d != "" {
			b.Digest = d
			b.DigestFromName = true
		}
		s.Blobs = append(s.Blobs, b)
	}

	// A repo shell with refs/snapshots gone but the directory left
	// behind is itself clutter worth reporting.
	if len(blobEntries) == 0 && len(revs) == 0 && len(refs) == 0 {
		size, _ := dirOwnSize(repoDir)
		s.Extras = append(s.Extras, inventory.Extra{
			Store: inventory.HuggingFace,
			Path:  repoDir,
			Size:  size,
			IsDir: true,
			Kind:  "empty-repo",
			Note:  display + " has no blobs, snapshots or refs",
		})
	}
	return nil
}

// readRefs returns commit hash -> ref names. Refs may nest
// (refs/pr/1), so we walk instead of listing.
func readRefs(dir string) (map[string][]string, error) {
	refs := map[string][]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		commit := strings.TrimSpace(string(data))
		if commit == "" {
			return nil
		}
		name, _ := filepath.Rel(dir, p)
		refs[commit] = append(refs[commit], filepath.ToSlash(name))
		return nil
	})
	if err != nil {
		return nil, err
	}
	for c := range refs {
		sort.Strings(refs[c])
	}
	return refs, nil
}

type snapLink struct {
	linkPath string
	blobPath string
	dangling bool
}

// snapshotBlobs resolves every entry under one snapshot directory to the
// blob it references. Symlinks are resolved relative to their own
// directory (HF writes ../../blobs/<etag>); regular files — which some
// older clients copied instead of linking — reference no blob and are
// ignored here, since their bytes live in the snapshot, not in blobs/.
func snapshotBlobs(revDir string) ([]snapLink, error) {
	var out []snapLink
	err := filepath.WalkDir(revDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		target = filepath.Clean(target)
		l := snapLink{linkPath: p, blobPath: target}
		if _, err := os.Stat(target); err != nil {
			l.dangling = true
		}
		out = append(out, l)
		return nil
	})
	return out, err
}

// dirOwnSize sums the apparent size of everything under dir except the
// blobs symlinks point at — i.e. the cost of the directory skeleton
// itself. Symlinks count as zero, which mirrors what deleting the
// directory (and not its targets) would free.
func dirOwnSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		n += info.Size()
		return nil
	})
	return n, err
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
