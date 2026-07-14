// Tests for plan building (safety triage, keeper choice, dedup of
// actions) and plan execution (dry-run default, guards against escaped
// paths, changed files and stale plans).
package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JaydenCJ/weightsweep/internal/correlate"
	"github.com/JaydenCJ/weightsweep/internal/inventory"
)

const dg = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func scanWith(store inventory.Store, root string, blobs ...inventory.Blob) *inventory.StoreScan {
	return &inventory.StoreScan{Store: store, Root: root, Found: true, Blobs: blobs}
}

func actionByPath(t *testing.T, p *Plan, path string) Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.Path == path {
			return a
		}
	}
	t.Fatalf("no action for %s in %+v", path, p.Actions)
	return Action{}
}

func TestBuildOrphanIsSafe(t *testing.T) {
	s := scanWith(inventory.Ollama, "/r",
		inventory.Blob{Store: inventory.Ollama, Path: "/r/blobs/x", Size: 100, Class: inventory.ClassOrphan})
	p := Build([]*inventory.StoreScan{s}, nil, 0)
	a := actionByPath(t, p, "/r/blobs/x")
	if a.Safety != SafetySafe || a.Reason != "orphan-blob" {
		t.Fatalf("action = %+v", a)
	}
	if p.SafeBytes != 100 || p.TotalBytes != 100 || p.ReviewBytes != 0 {
		t.Fatalf("byte totals wrong: %+v", p)
	}
}

func TestBuildStaleBlobNeedsReview(t *testing.T) {
	s := scanWith(inventory.HuggingFace, "/r",
		inventory.Blob{Store: inventory.HuggingFace, Path: "/r/b/old", Size: 50,
			Class: inventory.ClassStale, Refs: []string{"org/m@oldcommit000"}})
	p := Build([]*inventory.StoreScan{s}, nil, 0)
	a := actionByPath(t, p, "/r/b/old")
	if a.Safety != SafetyReview || a.Reason != "stale-blob" {
		t.Fatalf("action = %+v", a)
	}
	if !strings.Contains(a.Detail, "org/m@oldcommit000") {
		t.Fatalf("detail = %q", a.Detail)
	}
}

func TestBuildLiveBlobNeverPlanned(t *testing.T) {
	s := scanWith(inventory.Ollama, "/r",
		inventory.Blob{Store: inventory.Ollama, Path: "/r/blobs/live", Size: 10,
			Class: inventory.ClassLive, Refs: []string{"m:latest"}})
	p := Build([]*inventory.StoreScan{s}, nil, 0)
	if len(p.Actions) != 0 {
		t.Fatalf("live blob planned for deletion: %+v", p.Actions)
	}
}

func TestBuildStaleRevisionExtraIsDirAction(t *testing.T) {
	s := scanWith(inventory.HuggingFace, "/r")
	s.Extras = []inventory.Extra{{
		Store: inventory.HuggingFace, Path: "/r/models--o--m/snapshots/old",
		Size: 12, IsDir: true, Kind: "stale-revision", Note: "o/m revision old",
	}}
	p := Build([]*inventory.StoreScan{s}, nil, 0)
	a := actionByPath(t, p, "/r/models--o--m/snapshots/old")
	if !a.IsDir || a.Reason != "stale-revision" || a.Safety != SafetySafe {
		t.Fatalf("action = %+v", a)
	}
}

func dupGroup(size int64, blobs ...inventory.Blob) correlate.Group {
	g := correlate.Group{Digest: dg, Size: size, Blobs: blobs}
	g.Wasted = size * int64(len(blobs)-1)
	return g
}

func TestBuildDuplicateKeepsMostReferencedCopy(t *testing.T) {
	hfCopy := inventory.Blob{Store: inventory.HuggingFace, Path: "/hf/b/x", Size: 100,
		Class: inventory.ClassLive, Digest: dg, Refs: []string{"org/m@main"}}
	olCopy := inventory.Blob{Store: inventory.Ollama, Path: "/ol/blobs/x", Size: 100,
		Class: inventory.ClassLive, Digest: dg, Refs: []string{"m:4b", "m:4b-instruct"}}
	scans := []*inventory.StoreScan{
		scanWith(inventory.HuggingFace, "/hf", hfCopy),
		scanWith(inventory.Ollama, "/ol", olCopy),
	}
	p := Build(scans, []correlate.Group{dupGroup(100, hfCopy, olCopy)}, 0)
	if len(p.Actions) != 1 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	a := p.Actions[0]
	// The Ollama copy feeds two tags; the HF copy is the one to drop.
	if a.Path != "/hf/b/x" || a.Safety != SafetyReview || a.Reason != "duplicate" {
		t.Fatalf("action = %+v", a)
	}
	if !strings.Contains(a.Detail, "/ol/blobs/x") || !strings.Contains(a.Detail, "re-download") {
		t.Fatalf("detail = %q", a.Detail)
	}
}

func TestBuildDuplicateTieBreaksByStoreOrder(t *testing.T) {
	hfCopy := inventory.Blob{Store: inventory.HuggingFace, Path: "/hf/b/x", Size: 10,
		Class: inventory.ClassLive, Digest: dg, Refs: []string{"a"}}
	lmCopy := inventory.Blob{Store: inventory.LMStudio, Path: "/lm/p/m/x.gguf", Size: 10,
		Class: inventory.ClassLive, Digest: dg, Refs: []string{"p/m"}}
	scans := []*inventory.StoreScan{
		scanWith(inventory.HuggingFace, "/hf", hfCopy),
		scanWith(inventory.LMStudio, "/lm", lmCopy),
	}
	p := Build(scans, []correlate.Group{dupGroup(10, hfCopy, lmCopy)}, 0)
	// Equal ref counts: canonical store order keeps huggingface.
	if len(p.Actions) != 1 || p.Actions[0].Path != "/lm/p/m/x.gguf" {
		t.Fatalf("actions = %+v", p.Actions)
	}
}

func TestBuildOrphanedDuplicateNotDoubleActioned(t *testing.T) {
	// A blob that is both an orphan and a duplicate copy gets exactly
	// one action — the safe orphan one.
	orphan := inventory.Blob{Store: inventory.Ollama, Path: "/ol/blobs/x", Size: 10,
		Class: inventory.ClassOrphan, Digest: dg}
	live := inventory.Blob{Store: inventory.HuggingFace, Path: "/hf/b/x", Size: 10,
		Class: inventory.ClassLive, Digest: dg, Refs: []string{"org/m@main"}}
	scans := []*inventory.StoreScan{
		scanWith(inventory.Ollama, "/ol", orphan),
		scanWith(inventory.HuggingFace, "/hf", live),
	}
	p := Build(scans, []correlate.Group{dupGroup(10, orphan, live)}, 0)
	if len(p.Actions) != 1 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	if a := p.Actions[0]; a.Path != "/ol/blobs/x" || a.Reason != "orphan-blob" {
		t.Fatalf("action = %+v", a)
	}
}

func TestBuildMinSizeSuppressesSmallOrphansButNeverPartials(t *testing.T) {
	// Partial downloads are garbage at any size; min-size must not
	// hide them, only small-but-real orphans.
	s := scanWith(inventory.Ollama, "/r",
		inventory.Blob{Store: inventory.Ollama, Path: "/r/blobs/small", Size: 10, Class: inventory.ClassOrphan},
		inventory.Blob{Store: inventory.Ollama, Path: "/r/blobs/big", Size: 10000, Class: inventory.ClassOrphan},
		inventory.Blob{Store: inventory.Ollama, Path: "/r/blobs/x-partial", Size: 3, Class: inventory.ClassPartial})
	p := Build([]*inventory.StoreScan{s}, nil, 1000)
	if len(p.Actions) != 2 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	actionByPath(t, p, "/r/blobs/big")
	if a := actionByPath(t, p, "/r/blobs/x-partial"); a.Reason != "partial-download" {
		t.Fatalf("action = %+v", a)
	}
}

func TestBuildActionsSortedSafeFirstThenBytes(t *testing.T) {
	s := scanWith(inventory.HuggingFace, "/r",
		inventory.Blob{Store: inventory.HuggingFace, Path: "/r/b/stale", Size: 999, Class: inventory.ClassStale, Refs: []string{"x"}},
		inventory.Blob{Store: inventory.HuggingFace, Path: "/r/b/orphan-small", Size: 5, Class: inventory.ClassOrphan},
		inventory.Blob{Store: inventory.HuggingFace, Path: "/r/b/orphan-big", Size: 500, Class: inventory.ClassOrphan})
	p := Build([]*inventory.StoreScan{s}, nil, 0)
	if len(p.Actions) != 3 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	if p.Actions[0].Path != "/r/b/orphan-big" || p.Actions[1].Path != "/r/b/orphan-small" ||
		p.Actions[2].Path != "/r/b/stale" {
		t.Fatalf("order = %s, %s, %s", p.Actions[0].Path, p.Actions[1].Path, p.Actions[2].Path)
	}
	if p.Actions[0].ID != "ws-0001" || p.Actions[2].ID != "ws-0003" {
		t.Fatalf("ids = %s..%s", p.Actions[0].ID, p.Actions[2].ID)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	s := scanWith(inventory.Ollama, "/r",
		inventory.Blob{Store: inventory.Ollama, Path: "/r/blobs/x", Size: 100, Class: inventory.ClassOrphan})
	p := Build([]*inventory.StoreScan{s}, nil, 0)
	data, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tool != p.Tool || len(got.Actions) != 1 || got.Actions[0].Path != "/r/blobs/x" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestDecodeRejectsInvalidPlans(t *testing.T) {
	// A plan is a deletion list; a half-understood one must never run.
	if _, err := Decode([]byte("not json")); err == nil {
		t.Fatal("garbage accepted")
	}
	future, _ := json.Marshal(map[string]any{
		"format_version": 99,
		"roots":          map[string]string{"ollama": "/r"},
	})
	if _, err := Decode(future); err == nil || !strings.Contains(err.Error(), "format_version") {
		t.Fatalf("future version: err = %v", err)
	}
	rootless, _ := json.Marshal(map[string]any{"format_version": 1})
	if _, err := Decode(rootless); err == nil || !strings.Contains(err.Error(), "roots") {
		t.Fatalf("missing roots: err = %v", err)
	}
}

// planFor builds a real on-disk orphan and a matching plan.
func planFor(t *testing.T) (*Plan, string) {
	t.Helper()
	root := t.TempDir()
	blobs := filepath.Join(root, "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(blobs, "sha256-dead")
	if err := os.WriteFile(orphan, []byte("orphan-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := scanWith(inventory.Ollama, root,
		inventory.Blob{Store: inventory.Ollama, Path: orphan, Size: 12, Class: inventory.ClassOrphan})
	return Build([]*inventory.StoreScan{s}, nil, 0), orphan
}

func TestExecuteDryRunThenApply(t *testing.T) {
	p, orphan := planFor(t)
	// Dry run reports what it would do and touches nothing.
	res := Execute(p, ExecOpts{})
	if res.Removed != 1 || res.FreedBytes != 12 {
		t.Fatalf("result = %+v", res)
	}
	if res.Outcomes[0].Status != "would-remove" {
		t.Fatalf("status = %s", res.Outcomes[0].Status)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("dry run deleted the file")
	}
	// Apply actually deletes.
	res = Execute(p, ExecOpts{Apply: true})
	if res.Removed != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("file survived apply")
	}
}

func TestExecuteReviewNeedsExplicitConsent(t *testing.T) {
	// Review actions are skipped by default even under --apply…
	p, orphan := planFor(t)
	p.Actions[0].Safety = SafetyReview
	res := Execute(p, ExecOpts{Apply: true})
	if res.Removed != 0 || res.Skipped != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("review action executed without consent")
	}
	if !strings.Contains(res.Outcomes[0].Message, "--include-review") {
		t.Fatalf("message = %q", res.Outcomes[0].Message)
	}
	// …and executed once IncludeReview grants consent.
	res = Execute(p, ExecOpts{Apply: true, IncludeReview: true})
	if res.Removed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("file survived")
	}
}

func TestExecuteOnlySelectsAndOverridesReviewGate(t *testing.T) {
	// Explicitly naming a review action's ID is consent for that one.
	p, orphan := planFor(t)
	p.Actions[0].Safety = SafetyReview
	res := Execute(p, ExecOpts{Apply: true, Only: []string{p.Actions[0].ID}})
	if res.Removed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("file survived")
	}
	// An unknown ID selects nothing and touches nothing.
	p2, orphan2 := planFor(t)
	res2 := Execute(p2, ExecOpts{Apply: true, Only: []string{"ws-9999"}})
	if res2.Removed != 0 || len(res2.Outcomes) != 0 {
		t.Fatalf("result = %+v", res2)
	}
	if _, err := os.Stat(orphan2); err != nil {
		t.Fatal("unrelated file deleted")
	}
}

func TestExecuteRefusesPathOutsideRoot(t *testing.T) {
	p, _ := planFor(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("precious-12b"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Actions[0].Path = victim // tampered plan
	res := Execute(p, ExecOpts{Apply: true})
	if res.Failed != 1 || res.Removed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Outcomes[0].Message, "escapes") {
		t.Fatalf("message = %q", res.Outcomes[0].Message)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file outside root was deleted")
	}
}

func TestExecuteRefusesTamperedPlanFields(t *testing.T) {
	// Relative paths and unknown store roots both mean the plan cannot
	// be trusted; each is a hard failure, never a deletion.
	p, _ := planFor(t)
	p.Actions[0].Path = "blobs/sha256-dead"
	res := Execute(p, ExecOpts{Apply: true})
	if res.Failed != 1 || !strings.Contains(res.Outcomes[0].Message, "not absolute") {
		t.Fatalf("relative path: result = %+v", res)
	}
	p2, _ := planFor(t)
	p2.Actions[0].Store = "mystery"
	res2 := Execute(p2, ExecOpts{Apply: true})
	if res2.Failed != 1 || !strings.Contains(res2.Outcomes[0].Message, "no root") {
		t.Fatalf("unknown store: result = %+v", res2)
	}
}

func TestExecuteSkipsWhenSizeChanged(t *testing.T) {
	// The blob was re-downloaded (or is being written) since planning;
	// deleting it now would destroy fresh data.
	p, orphan := planFor(t)
	if err := os.WriteFile(orphan, []byte("brand-new-longer-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Execute(p, ExecOpts{Apply: true})
	if res.Skipped != 1 || res.Removed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Outcomes[0].Message, "size changed") {
		t.Fatalf("message = %q", res.Outcomes[0].Message)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("changed file deleted")
	}
}

func TestExecuteRefusesKindMismatchButSkipsAlreadyGone(t *testing.T) {
	// A vanished entry is a benign skip (prune stays idempotent)…
	p, orphan := planFor(t)
	if err := os.Remove(orphan); err != nil {
		t.Fatal(err)
	}
	res0 := Execute(p, ExecOpts{Apply: true})
	if res0.Skipped != 1 || res0.Failed != 0 {
		t.Fatalf("prune not idempotent: %+v", res0)
	}
	// …but a directory where the plan recorded a file means the world
	// changed too much to trust the plan.
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	res := Execute(p, ExecOpts{Apply: true})
	if res.Failed != 1 || !strings.Contains(res.Outcomes[0].Message, "kind changed") {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecuteRemovesStaleRevisionDirectory(t *testing.T) {
	root := t.TempDir()
	revDir := filepath.Join(root, "models--o--m", "snapshots", "old")
	if err := os.MkdirAll(revDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(revDir, "w.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := scanWith(inventory.HuggingFace, root)
	s.Extras = []inventory.Extra{{Store: inventory.HuggingFace, Path: revDir, Size: 1,
		IsDir: true, Kind: "stale-revision"}}
	p := Build([]*inventory.StoreScan{s}, nil, 0)
	res := Execute(p, ExecOpts{Apply: true})
	if res.Removed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(revDir); !os.IsNotExist(err) {
		t.Fatal("directory survived")
	}
}
