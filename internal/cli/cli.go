// Package cli wires the scanners, correlator and planner into the
// weightsweep command line. All I/O flows through the injected writers
// so the whole surface is unit-testable without a process boundary.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/JaydenCJ/weightsweep/internal/correlate"
	"github.com/JaydenCJ/weightsweep/internal/hfhub"
	"github.com/JaydenCJ/weightsweep/internal/inventory"
	"github.com/JaydenCJ/weightsweep/internal/lmstudio"
	"github.com/JaydenCJ/weightsweep/internal/ollama"
	"github.com/JaydenCJ/weightsweep/internal/plan"
	"github.com/JaydenCJ/weightsweep/internal/version"
)

// Exit codes, documented in the README.
const (
	ExitOK    = 0
	ExitDirty = 1 // findings that need attention: verify mismatches, prune failures
	ExitUsage = 2 // bad flags, unreadable plan, IO errors
)

const usage = `weightsweep — disk analyzer for Hugging Face, Ollama and LM Studio caches

Usage:
  weightsweep [global flags] <command> [command flags]

Commands:
  scan      inventory every store: sizes, duplicate and orphan totals
  dupes     list byte-identical blobs found in more than one place
  orphans   list orphaned, stale and partial files per store
  plan      write a prune plan (JSON) proposing safe deletions
  prune     execute a plan (dry run unless --apply)

Global flags:
  --hf DIR        Hugging Face hub cache
                  (default $HF_HUB_CACHE, $HF_HOME/hub, ~/.cache/huggingface/hub)
  --ollama DIR    Ollama models directory (default $OLLAMA_MODELS, ~/.ollama/models)
  --lmstudio DIR  LM Studio models directory
                  (default ~/.lmstudio/models, ~/.cache/lm-studio/models)
  --json          machine-readable output
  --version       print version and exit
  --help          this text

Command flags:
  scan     --verify           re-hash name-addressed blobs, report corruption
  scan|dupes|plan
           --hash MODE        auto (default: hash only size collisions),
                              always, never
           --min-size SIZE    drop blobs smaller than SIZE (e.g. 100MiB)
                              from duplicate groups and plan actions
  plan     --out FILE         write plan JSON here (default: stdout)
  prune    --plan FILE        plan to execute (required)
           --apply            actually delete (default: dry run)
           --include-review   also execute review-level actions
           --only ID[,ID]     restrict to specific action IDs

Exit codes: 0 ok · 1 findings need attention (corruption, prune failures) · 2 usage error
`

// options carries parsed global + command flags.
type options struct {
	hf, ollamaDir, lmstudio string
	jsonOut                 bool
	verify                  bool
	hashMode                string // auto | always | never
	minSize                 int64
	planOut                 string
	planFile                string
	apply                   bool
	includeReview           bool
	only                    []string
}

// Run is the whole CLI. argv excludes the program name.
func Run(argv []string, stdout, stderr io.Writer) int {
	opts := options{hashMode: "auto", planOut: "-"}
	cmd := ""
	i := 0
	next := func(flagName string) (string, bool) {
		if i+1 >= len(argv) {
			fmt.Fprintf(stderr, "weightsweep: %s needs a value\n", flagName)
			return "", false
		}
		i++
		return argv[i], true
	}
	for ; i < len(argv); i++ {
		arg := argv[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		val := func(flagName string) (string, bool) {
			if hasInline {
				return inline, true
			}
			return next(flagName)
		}
		switch name {
		case "--hf":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			opts.hf = v
		case "--ollama":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			opts.ollamaDir = v
		case "--lmstudio":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			opts.lmstudio = v
		case "--json":
			opts.jsonOut = true
		case "--verify":
			opts.verify = true
		case "--hash":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			if v != "auto" && v != "always" && v != "never" {
				fmt.Fprintf(stderr, "weightsweep: --hash must be auto, always or never (got %q)\n", v)
				return ExitUsage
			}
			opts.hashMode = v
		case "--min-size":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			n, err := ParseSize(v)
			if err != nil {
				fmt.Fprintf(stderr, "weightsweep: %v\n", err)
				return ExitUsage
			}
			opts.minSize = n
		case "--out":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			opts.planOut = v
		case "--plan":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			opts.planFile = v
		case "--apply":
			opts.apply = true
		case "--include-review":
			opts.includeReview = true
		case "--only":
			v, ok := val(name)
			if !ok {
				return ExitUsage
			}
			for _, id := range strings.Split(v, ",") {
				if id = strings.TrimSpace(id); id != "" {
					opts.only = append(opts.only, id)
				}
			}
		case "--version":
			fmt.Fprintf(stdout, "weightsweep %s\n", version.Version)
			return ExitOK
		case "--help", "-h":
			fmt.Fprint(stdout, usage)
			return ExitOK
		default:
			if strings.HasPrefix(name, "-") {
				fmt.Fprintf(stderr, "weightsweep: unknown flag %s\n", name)
				return ExitUsage
			}
			if cmd != "" {
				fmt.Fprintf(stderr, "weightsweep: unexpected argument %q\n", arg)
				return ExitUsage
			}
			cmd = name
		}
	}

	switch cmd {
	case "":
		fmt.Fprint(stderr, usage)
		return ExitUsage
	case "scan":
		return cmdScan(&opts, stdout, stderr)
	case "dupes":
		return cmdDupes(&opts, stdout, stderr)
	case "orphans":
		return cmdOrphans(&opts, stdout, stderr)
	case "plan":
		return cmdPlan(&opts, stdout, stderr)
	case "prune":
		return cmdPrune(&opts, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "weightsweep: unknown command %q (try --help)\n", cmd)
		return ExitUsage
	}
}

// roots resolves the three store roots from flags, environment, then
// conventional home locations, in that order.
func (o *options) roots() (hf, ol, lm string) {
	home, _ := os.UserHomeDir()
	hf = o.hf
	if hf == "" {
		hf = os.Getenv("HF_HUB_CACHE")
	}
	if hf == "" {
		if h := os.Getenv("HF_HOME"); h != "" {
			hf = filepath.Join(h, "hub")
		}
	}
	if hf == "" && home != "" {
		hf = filepath.Join(home, ".cache", "huggingface", "hub")
	}
	ol = o.ollamaDir
	if ol == "" {
		ol = os.Getenv("OLLAMA_MODELS")
	}
	if ol == "" && home != "" {
		ol = filepath.Join(home, ".ollama", "models")
	}
	lm = o.lmstudio
	if lm == "" && home != "" {
		modern := filepath.Join(home, ".lmstudio", "models")
		legacy := filepath.Join(home, ".cache", "lm-studio", "models")
		lm = modern
		if _, err := os.Stat(modern); os.IsNotExist(err) {
			if _, err := os.Stat(legacy); err == nil {
				lm = legacy
			}
		}
	}
	return hf, ol, lm
}

// gather runs all three scanners and, unless hashMode is "never", fills
// in digests for correlation.
func gather(o *options, stderr io.Writer) ([]*inventory.StoreScan, correlate.HashStats, error) {
	hfRoot, olRoot, lmRoot := o.roots()
	var scans []*inventory.StoreScan
	hs, err := hfhub.Scan(hfRoot)
	if err != nil {
		return nil, correlate.HashStats{}, fmt.Errorf("huggingface scan: %w", err)
	}
	os_, err := ollama.Scan(olRoot)
	if err != nil {
		return nil, correlate.HashStats{}, fmt.Errorf("ollama scan: %w", err)
	}
	ls, err := lmstudio.Scan(lmRoot)
	if err != nil {
		return nil, correlate.HashStats{}, fmt.Errorf("lmstudio scan: %w", err)
	}
	scans = []*inventory.StoreScan{hs, os_, ls}
	var stats correlate.HashStats
	if o.hashMode != "never" {
		stats = correlate.FillDigests(allBlobs(scans), o.hashMode == "always")
	}
	for _, s := range scans {
		for _, w := range s.Warnings {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
	}
	return scans, stats, nil
}

func allBlobs(scans []*inventory.StoreScan) []*inventory.Blob {
	var out []*inventory.Blob
	for _, s := range scans {
		for i := range s.Blobs {
			out = append(out, &s.Blobs[i])
		}
	}
	return out
}

// reclaimable is the per-store "you could get this back" figure:
// orphaned + stale + partial bytes.
func reclaimable(s *inventory.StoreScan) int64 {
	return s.BytesByClass(inventory.ClassOrphan) +
		s.BytesByClass(inventory.ClassStale) +
		s.BytesByClass(inventory.ClassPartial)
}

func cmdScan(o *options, stdout, stderr io.Writer) int {
	scans, stats, err := gather(o, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	var mismatches []correlate.Mismatch
	if o.verify {
		mismatches, err = correlate.Verify(allBlobs(scans))
		if err != nil {
			fmt.Fprintf(stderr, "weightsweep: verify: %v\n", err)
			return ExitUsage
		}
	}
	groups := correlate.Groups(allBlobs(scans), o.minSize)

	if o.jsonOut {
		writeJSON(stdout, map[string]any{
			"stores":       scans,
			"dupe_groups":  groups,
			"dupe_wasted":  correlate.TotalWasted(groups),
			"hash_stats":   stats,
			"verify_bad":   mismatches,
			"tool_version": version.Version,
		})
		if len(mismatches) > 0 {
			return ExitDirty
		}
		return ExitOK
	}

	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STORE\tROOT\tMODELS\tFILES\tSIZE\tRECLAIMABLE")
	var tModels, tFiles int
	var tSize, tRec int64
	for _, s := range scans {
		root := s.Root
		if !s.Found {
			root += " (not found)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n",
			s.Store, root, s.Models, len(s.Blobs),
			inventory.HumanBytes(s.TotalBytes()), inventory.HumanBytes(reclaimable(s)))
		tModels += s.Models
		tFiles += len(s.Blobs)
		tSize += s.TotalBytes()
		tRec += reclaimable(s)
	}
	fmt.Fprintf(tw, "total\t\t%d\t%d\t%s\t%s\n",
		tModels, tFiles, inventory.HumanBytes(tSize), inventory.HumanBytes(tRec))
	tw.Flush()

	redundant := 0
	for _, g := range groups {
		redundant += len(g.Blobs) - 1
	}
	fmt.Fprintf(stdout, "\nduplicates: %d group(s), %d redundant cop%s, %s reclaimable by deduping\n",
		len(groups), redundant, plural(redundant, "y", "ies"), inventory.HumanBytes(correlate.TotalWasted(groups)))
	orphanFiles, orphanBytes := classTotal(scans, inventory.ClassOrphan)
	staleFiles, staleBytes := classTotal(scans, inventory.ClassStale)
	partFiles, partBytes := classTotal(scans, inventory.ClassPartial)
	fmt.Fprintf(stdout, "orphans:    %d file(s), %s · stale: %d file(s), %s · partial: %d file(s), %s\n",
		orphanFiles, inventory.HumanBytes(orphanBytes),
		staleFiles, inventory.HumanBytes(staleBytes),
		partFiles, inventory.HumanBytes(partBytes))
	if stats.Hashed > 0 {
		fmt.Fprintf(stdout, "hashed %d file(s) (%s) to correlate size collisions\n",
			stats.Hashed, inventory.HumanBytes(stats.HashedBytes))
	}
	for _, m := range mismatches {
		fmt.Fprintf(stdout, "CORRUPT: %s claims %s but hashes to %s\n",
			m.Blob.Path, inventory.ShortDigest(m.Blob.Digest), inventory.ShortDigest(m.Actual))
	}
	fmt.Fprintf(stdout, "next: weightsweep plan --out plan.json && weightsweep prune --plan plan.json\n")
	if len(mismatches) > 0 {
		return ExitDirty
	}
	return ExitOK
}

func cmdDupes(o *options, stdout, stderr io.Writer) int {
	scans, _, err := gather(o, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	groups := correlate.Groups(allBlobs(scans), o.minSize)
	if o.jsonOut {
		writeJSON(stdout, map[string]any{
			"groups": groups,
			"wasted": correlate.TotalWasted(groups),
		})
		return ExitOK
	}
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "no duplicate blobs found")
		return ExitOK
	}
	fmt.Fprintf(stdout, "%d duplicate group(s), %s wasted\n",
		len(groups), inventory.HumanBytes(correlate.TotalWasted(groups)))
	for _, g := range groups {
		stores := make([]string, len(g.Stores))
		for i, st := range g.Stores {
			stores[i] = string(st)
		}
		fmt.Fprintf(stdout, "\n%s  %s × %d copies (wasted %s)  stores: %s\n",
			inventory.ShortDigest(g.Digest), inventory.HumanBytes(g.Size),
			len(g.Blobs), inventory.HumanBytes(g.Wasted), strings.Join(stores, "+"))
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		for _, b := range g.Blobs {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				b.Class, b.Store, b.Path, strings.Join(b.Refs, ", "))
		}
		tw.Flush()
	}
	return ExitOK
}

func cmdOrphans(o *options, stdout, stderr io.Writer) int {
	scans, _, err := gather(o, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	type row struct {
		b inventory.Blob
	}
	var rows []row
	for _, s := range scans {
		for _, b := range s.Blobs {
			if b.Class == inventory.ClassOrphan || b.Class == inventory.ClassStale ||
				b.Class == inventory.ClassPartial {
				rows = append(rows, row{b})
			}
		}
	}
	if o.jsonOut {
		blobs := make([]inventory.Blob, len(rows))
		var total int64
		for i, r := range rows {
			blobs[i] = r.b
			total += r.b.Size
		}
		writeJSON(stdout, map[string]any{"orphans": blobs, "total_bytes": total})
		return ExitOK
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no orphaned, stale or partial files found")
		return ExitOK
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CLASS\tSTORE\tSIZE\tPATH\tNOTE")
	var total int64
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.b.Class, r.b.Store, inventory.HumanBytes(r.b.Size), r.b.Path, r.b.Note)
		total += r.b.Size
	}
	tw.Flush()
	fmt.Fprintf(stdout, "%d file(s), %s\n", len(rows), inventory.HumanBytes(total))
	return ExitOK
}

func cmdPlan(o *options, stdout, stderr io.Writer) int {
	scans, _, err := gather(o, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	groups := correlate.Groups(allBlobs(scans), o.minSize)
	p := plan.Build(scans, groups, o.minSize)
	data, err := p.Encode()
	if err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	if o.planOut == "-" {
		stdout.Write(data)
		return ExitOK
	}
	if err := os.WriteFile(o.planOut, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	safeN, reviewN := 0, 0
	for _, a := range p.Actions {
		if a.Safety == plan.SafetySafe {
			safeN++
		} else {
			reviewN++
		}
	}
	fmt.Fprintf(stdout, "plan: %d action(s), %s total\n",
		len(p.Actions), inventory.HumanBytes(p.TotalBytes))
	fmt.Fprintf(stdout, "  safe:   %d action(s), %s (orphans, partials, stale skeletons)\n",
		safeN, inventory.HumanBytes(p.SafeBytes))
	fmt.Fprintf(stdout, "  review: %d action(s), %s (stale blobs, duplicate copies)\n",
		reviewN, inventory.HumanBytes(p.ReviewBytes))
	fmt.Fprintf(stdout, "wrote %s\n", o.planOut)
	return ExitOK
}

func cmdPrune(o *options, stdout, stderr io.Writer) int {
	if o.planFile == "" {
		fmt.Fprintln(stderr, "weightsweep: prune needs --plan FILE (generate one with `weightsweep plan`)")
		return ExitUsage
	}
	data, err := os.ReadFile(o.planFile)
	if err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	p, err := plan.Decode(data)
	if err != nil {
		fmt.Fprintf(stderr, "weightsweep: %v\n", err)
		return ExitUsage
	}
	res := plan.Execute(p, plan.ExecOpts{
		Apply:         o.apply,
		IncludeReview: o.includeReview,
		Only:          o.only,
	})
	if o.jsonOut {
		writeJSON(stdout, res)
	} else {
		if !o.apply {
			fmt.Fprintln(stdout, "DRY RUN — nothing deleted (pass --apply to delete)")
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		for _, out := range res.Outcomes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				out.Status, out.Action.ID, out.Action.Safety, out.Action.Reason,
				inventory.HumanBytes(out.Action.Bytes), out.Action.Path)
			if out.Message != "" {
				fmt.Fprintf(tw, "\t\t\t\t\t↳ %s\n", out.Message)
			}
		}
		tw.Flush()
		verb := "freed"
		if !o.apply {
			verb = "would free"
		}
		fmt.Fprintf(stdout, "%s %s across %d action(s); %d skipped, %d failed\n",
			verb, inventory.HumanBytes(res.FreedBytes), res.Removed, res.Skipped, res.Failed)
	}
	if res.Failed > 0 {
		return ExitDirty
	}
	return ExitOK
}

func classTotal(scans []*inventory.StoreScan, c inventory.Class) (int, int64) {
	var files int
	var bytes int64
	for _, s := range scans {
		files += s.CountByClass(c)
		bytes += s.BytesByClass(c)
	}
	return files, bytes
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ParseSize parses human-friendly sizes: "1048576", "100KB", "1.5GiB",
// "2 MiB". Decimal suffixes (KB, MB, …) are powers of 1000; binary
// suffixes (KiB, MiB, …) are powers of 1024.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	upper := strings.ToUpper(t)
	units := []struct {
		suffix string
		mult   float64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
	}
	mult := float64(1)
	num := t
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mult
			num = strings.TrimSpace(t[:len(t)-len(u.suffix)])
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q (want e.g. 1048576, 100MB, 1.5GiB)", s)
	}
	return int64(v * mult), nil
}
