// Package ollama scans an Ollama model store (the directory usually at
// ~/.ollama/models). Layout recap:
//
//	models/
//	├── blobs/sha256-<64 hex>        content-addressed layer files
//	│         sha256-<hex>-partial…  interrupted pulls
//	└── manifests/<host>/<namespace>/<model>/<tag>
//	                                 OCI-style JSON manifest listing a
//	                                 config digest and layer digests
//
// A blob is live when at least one manifest references its digest and
// orphaned otherwise. Manifests referencing missing blobs produce
// warnings — that model is broken and will re-pull.
package ollama

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

const defaultHost = "registry.ollama.ai"
const defaultNamespace = "library"

// manifest is the subset of the OCI image manifest Ollama writes that
// weightsweep needs: which digests this model pins.
type manifest struct {
	SchemaVersion int `json:"schemaVersion"`
	Config        struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// Scan walks an Ollama models root and classifies every blob it holds.
// A missing root is not an error: Found is false and the scan is empty.
// The root is absolutized so plans built from the scan survive a change
// of working directory.
func Scan(root string) (*inventory.StoreScan, error) {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	s := &inventory.StoreScan{Store: inventory.Ollama, Root: root}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	s.Found = true

	// digest -> model names referencing it.
	refs := map[string][]string{}
	manifestDir := filepath.Join(root, "manifests")
	err := filepath.WalkDir(manifestDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(manifestDir, p)
		name := modelDisplay(filepath.ToSlash(rel))
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			s.Warnings = append(s.Warnings, "ollama: unparsable manifest "+p+": "+err.Error())
			return nil
		}
		s.Models++
		digests := []string{m.Config.Digest}
		for _, l := range m.Layers {
			digests = append(digests, l.Digest)
		}
		for _, dg := range digests {
			if !inventory.IsDigest(dg) {
				continue
			}
			refs[dg] = append(refs[dg], name)
			if _, err := os.Stat(blobPath(root, dg)); err != nil {
				s.Warnings = append(s.Warnings,
					"ollama: "+name+" references missing blob "+inventory.ShortDigest(dg))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	blobDir := filepath.Join(root, "blobs")
	entries, _ := os.ReadDir(blobDir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		b := inventory.Blob{
			Store: inventory.Ollama,
			Path:  filepath.Join(blobDir, e.Name()),
			Size:  info.Size(),
		}
		name := e.Name()
		switch {
		case strings.Contains(name, "-partial"):
			b.Class = inventory.ClassPartial
			b.Note = "interrupted pull"
		default:
			if d := digestFromBlobName(name); d != "" {
				b.Digest = d
				b.DigestFromName = true
			}
			if owners := refs[b.Digest]; len(owners) > 0 {
				b.Class = inventory.ClassLive
				b.Refs = dedupeSorted(owners)
			} else {
				b.Class = inventory.ClassOrphan
				b.Note = "no manifest references this blob"
			}
		}
		s.Blobs = append(s.Blobs, b)
	}
	s.Sort()
	return s, nil
}

// blobPath maps a "sha256:<hex>" digest to the on-disk blob filename
// Ollama uses ("sha256-<hex>").
func blobPath(root, digest string) string {
	return filepath.Join(root, "blobs", strings.Replace(digest, ":", "-", 1))
}

// digestFromBlobName parses "sha256-<64 hex>" blob filenames.
func digestFromBlobName(name string) string {
	if !strings.HasPrefix(name, "sha256-") {
		return ""
	}
	return inventory.DigestFromHex(strings.TrimPrefix(name, "sha256-"))
}

// modelDisplay collapses a manifest path to the name users type:
// "registry.ollama.ai/library/gemma3/4b" -> "gemma3:4b",
// "registry.ollama.ai/jayden/tool/v2"    -> "jayden/tool:v2",
// "example.test/team/m/latest"           -> "example.test/team/m:latest".
func modelDisplay(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) < 4 {
		return rel // unexpected shape; show as-is rather than guess
	}
	host, ns := parts[0], parts[1]
	model := strings.Join(parts[2:len(parts)-1], "/")
	tag := parts[len(parts)-1]
	switch {
	case host == defaultHost && ns == defaultNamespace:
		return model + ":" + tag
	case host == defaultHost:
		return ns + "/" + model + ":" + tag
	default:
		return host + "/" + ns + "/" + model + ":" + tag
	}
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := make([]string, 0, len(in))
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
