// Package plan turns an inventory into a reviewable, executable prune
// plan, and executes plans with hard safety rails.
//
// Every action carries a safety level:
//
//   - "safe": deleting it cannot break any installed model — orphaned
//     blobs nothing references, interrupted partial downloads, stale
//     snapshot skeletons, empty repo shells.
//   - "review": deleting it frees real space but changes what is
//     available — blobs kept alive only by detached HF revisions, and
//     redundant copies inside duplicate groups (the tool losing its
//     copy would have to re-download to use that model again).
//
// Execution is dry-run by default, applies only "safe" actions unless
// asked otherwise, and refuses to delete anything the plan's recorded
// store roots do not contain or whose size changed since planning.
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JaydenCJ/weightsweep/internal/correlate"
	"github.com/JaydenCJ/weightsweep/internal/inventory"
	"github.com/JaydenCJ/weightsweep/internal/version"
)

// FormatVersion is bumped on any incompatible change to the plan JSON.
const FormatVersion = 1

const (
	SafetySafe   = "safe"
	SafetyReview = "review"
)

// Action is one deletion the plan proposes.
type Action struct {
	ID     string `json:"id"` // "ws-0001", stable within a plan
	Safety string `json:"safety"`
	Reason string `json:"reason"` // "orphan-blob", "partial-download", …
	Store  string `json:"store"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"` // bytes freed (0 for symlink skeletons)
	IsDir  bool   `json:"is_dir,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Plan is the serializable prune proposal.
type Plan struct {
	FormatVersion int               `json:"format_version"`
	Tool          string            `json:"tool"`
	Roots         map[string]string `json:"roots"`
	Actions       []Action          `json:"actions"`
	SafeBytes     int64             `json:"safe_bytes"`
	ReviewBytes   int64             `json:"review_bytes"`
	TotalBytes    int64             `json:"total_bytes"`
}

// Build assembles a plan from per-store scans and the duplicate groups
// found across them. minSize suppresses trivially small actions
// (default 0 keeps everything).
func Build(scans []*inventory.StoreScan, groups []correlate.Group, minSize int64) *Plan {
	p := &Plan{
		FormatVersion: FormatVersion,
		Tool:          "weightsweep " + version.Version,
		Roots:         map[string]string{},
	}
	// Paths already claimed by an action, so a blob is never proposed
	// for deletion twice (e.g. orphan AND duplicate copy).
	claimed := map[string]bool{}

	for _, s := range scans {
		p.Roots[string(s.Store)] = s.Root
		for _, b := range s.Blobs {
			if b.Size < minSize && b.Class != inventory.ClassPartial {
				continue
			}
			switch b.Class {
			case inventory.ClassOrphan:
				p.add(claimed, Action{
					Safety: SafetySafe, Reason: "orphan-blob",
					Store: string(b.Store), Path: b.Path, Bytes: b.Size,
					Detail: b.Note,
				})
			case inventory.ClassPartial:
				p.add(claimed, Action{
					Safety: SafetySafe, Reason: "partial-download",
					Store: string(b.Store), Path: b.Path, Bytes: b.Size,
					Detail: b.Note,
				})
			case inventory.ClassStale:
				p.add(claimed, Action{
					Safety: SafetyReview, Reason: "stale-blob",
					Store: string(b.Store), Path: b.Path, Bytes: b.Size,
					Detail: "only detached revisions use this: " + strings.Join(b.Refs, ", "),
				})
			}
		}
		for _, e := range s.Extras {
			safety, reason := SafetySafe, "empty-repo"
			if e.Kind == "stale-revision" {
				reason = "stale-revision"
			}
			p.add(claimed, Action{
				Safety: safety, Reason: reason,
				Store: string(e.Store), Path: e.Path, Bytes: e.Size,
				IsDir: e.IsDir, Detail: e.Note,
			})
		}
	}

	for _, g := range groups {
		keep := keeper(g)
		for _, b := range g.Blobs {
			if b.Path == keep.Path || claimed[b.Path] || b.Class != inventory.ClassLive {
				continue // orphan/stale copies already have an action
			}
			p.add(claimed, Action{
				Safety: SafetyReview, Reason: "duplicate",
				Store: string(b.Store), Path: b.Path, Bytes: b.Size,
				Detail: fmt.Sprintf("same bytes as %s (%s); %s would re-download %s on next use",
					keep.Path, inventory.ShortDigest(g.Digest), b.Store, strings.Join(b.Refs, ", ")),
			})
		}
	}

	sort.Slice(p.Actions, func(i, j int) bool {
		if p.Actions[i].Safety != p.Actions[j].Safety { // safe first
			return p.Actions[i].Safety == SafetySafe
		}
		if p.Actions[i].Bytes != p.Actions[j].Bytes {
			return p.Actions[i].Bytes > p.Actions[j].Bytes
		}
		return p.Actions[i].Path < p.Actions[j].Path
	})
	for i := range p.Actions {
		p.Actions[i].ID = fmt.Sprintf("ws-%04d", i+1)
	}
	return p
}

// keeper picks which copy of a duplicate group survives: prefer a live
// copy with the most referents (the most would break by deleting it),
// then canonical store order, then path — fully deterministic.
func keeper(g correlate.Group) inventory.Blob {
	storeRank := map[inventory.Store]int{}
	for i, s := range inventory.AllStores {
		storeRank[s] = i
	}
	best := g.Blobs[0]
	for _, b := range g.Blobs[1:] {
		bLive, bestLive := b.Class == inventory.ClassLive, best.Class == inventory.ClassLive
		switch {
		case bLive != bestLive:
			if bLive {
				best = b
			}
		case len(b.Refs) != len(best.Refs):
			if len(b.Refs) > len(best.Refs) {
				best = b
			}
		case storeRank[b.Store] != storeRank[best.Store]:
			if storeRank[b.Store] < storeRank[best.Store] {
				best = b
			}
		case b.Path < best.Path:
			best = b
		}
	}
	return best
}

func (p *Plan) add(claimed map[string]bool, a Action) {
	if claimed[a.Path] {
		return
	}
	claimed[a.Path] = true
	p.Actions = append(p.Actions, a)
	p.TotalBytes += a.Bytes
	if a.Safety == SafetySafe {
		p.SafeBytes += a.Bytes
	} else {
		p.ReviewBytes += a.Bytes
	}
}

// Encode renders the plan as stable, indented JSON.
func (p *Plan) Encode() ([]byte, error) {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Decode parses and validates a plan file. Unknown future format
// versions are rejected rather than half-executed.
func Decode(data []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("not a weightsweep plan: %w", err)
	}
	if p.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("plan format_version %d not supported (this build understands %d)",
			p.FormatVersion, FormatVersion)
	}
	if len(p.Roots) == 0 {
		return nil, fmt.Errorf("plan has no store roots recorded; refusing to delete anything")
	}
	return &p, nil
}

// ExecOpts controls Execute.
type ExecOpts struct {
	Apply         bool     // false = dry run, print only
	IncludeReview bool     // also execute "review" actions
	Only          []string // if non-empty, restrict to these action IDs
}

// Outcome describes what happened to one action.
type Outcome struct {
	Action  Action `json:"action"`
	Status  string `json:"status"` // "removed", "would-remove", "skipped"
	Message string `json:"message,omitempty"`
}

// Result is the aggregate of an Execute run.
type Result struct {
	Outcomes   []Outcome `json:"outcomes"`
	Removed    int       `json:"removed"`
	FreedBytes int64     `json:"freed_bytes"`
	Skipped    int       `json:"skipped"`
	Failed     int       `json:"failed"`
}

// Execute applies (or dry-runs) a plan. Per-action guards, in order:
//
//  1. the action's store root must be recorded in the plan and the
//     path must resolve inside it (no "..", no absolute surprises);
//  2. the entry must still exist — vanished entries are skips, not
//     failures, so prune stays idempotent;
//  3. a file's current size must equal the planned size, so a blob
//     that was re-downloaded or appended to since planning survives;
//  4. directories are only removed for actions that recorded IsDir.
//
// Failures never abort the run: every action gets an outcome.
func Execute(p *Plan, opts ExecOpts) Result {
	var res Result
	only := map[string]bool{}
	for _, id := range opts.Only {
		only[id] = true
	}
	for _, a := range p.Actions {
		if len(only) > 0 && !only[a.ID] {
			continue
		}
		if a.Safety != SafetySafe && !opts.IncludeReview && !only[a.ID] {
			res.Outcomes = append(res.Outcomes, Outcome{Action: a, Status: "skipped",
				Message: "review action; pass --include-review or --only " + a.ID})
			res.Skipped++
			continue
		}
		if msg := guard(p, a); msg != "" {
			status := "skipped"
			if strings.HasPrefix(msg, "unsafe") {
				status = "failed"
				res.Failed++
			} else {
				res.Skipped++
			}
			res.Outcomes = append(res.Outcomes, Outcome{Action: a, Status: status, Message: msg})
			continue
		}
		if !opts.Apply {
			res.Outcomes = append(res.Outcomes, Outcome{Action: a, Status: "would-remove"})
			res.Removed++
			res.FreedBytes += a.Bytes
			continue
		}
		var err error
		if a.IsDir {
			err = os.RemoveAll(a.Path)
		} else {
			err = os.Remove(a.Path)
		}
		if err != nil {
			res.Outcomes = append(res.Outcomes, Outcome{Action: a, Status: "failed", Message: err.Error()})
			res.Failed++
			continue
		}
		res.Outcomes = append(res.Outcomes, Outcome{Action: a, Status: "removed"})
		res.Removed++
		res.FreedBytes += a.Bytes
	}
	return res
}

// guard returns "" when the action is safe to perform, a "skipped: …"
// reason for benign staleness, or an "unsafe: …" reason for anything
// that suggests the plan does not match this machine.
func guard(p *Plan, a Action) string {
	root, ok := p.Roots[a.Store]
	if !ok || root == "" {
		return "unsafe: plan records no root for store " + a.Store
	}
	if !filepath.IsAbs(a.Path) {
		return "unsafe: action path is not absolute"
	}
	rel, err := filepath.Rel(root, a.Path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "unsafe: path escapes the recorded store root " + root
	}
	info, err := os.Lstat(a.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "skipped: already gone"
		}
		return "unsafe: cannot stat: " + err.Error()
	}
	if info.IsDir() != a.IsDir {
		return "unsafe: entry kind changed since the plan was written"
	}
	if !a.IsDir && info.Mode().IsRegular() && info.Size() != a.Bytes {
		return fmt.Sprintf("skipped: size changed since planning (%d -> %d), re-run plan",
			a.Bytes, info.Size())
	}
	return ""
}
