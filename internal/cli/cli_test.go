// End-to-end tests for the CLI: a synthetic three-store cache tree is
// scanned, planned and pruned entirely through Run(), asserting on real
// stdout/stderr text, JSON shapes and exit codes.
package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hashHex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// demoTree builds a three-store fixture with every interesting case:
//   - HF: a live model whose big weights blob duplicates the Ollama
//     layer and the LM Studio file, plus an orphaned blob.
//   - Ollama: one manifest, its config + weights layer, plus a
//     leftover -partial download.
//   - LM Studio: one model whose GGUF holds the same bytes as the HF
//     and Ollama weights (found via size-collision hashing).
func demoTree(t *testing.T) (hf, ol, lm string) {
	t.Helper()
	base := t.TempDir()
	hf = filepath.Join(base, "hf")
	ol = filepath.Join(base, "ollama")
	lm = filepath.Join(base, "lmstudio")

	weights := "shared-model-weights-0123456789"
	wHash := hashHex(weights)

	// Hugging Face: live weights + orphan.
	repoDir := filepath.Join(hf, "models--acme--coder")
	write(t, filepath.Join(repoDir, "blobs", wHash), weights)
	write(t, filepath.Join(repoDir, "blobs", hashHex("abandoned")), "abandoned")
	write(t, filepath.Join(repoDir, "refs", "main"), "commit111\n")
	link := filepath.Join(repoDir, "snapshots", "commit111", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", wHash), link); err != nil {
		t.Fatal(err)
	}

	// Ollama: manifest + config + same weights layer + partial.
	cfg := `{"model_format":"gguf"}`
	write(t, filepath.Join(ol, "blobs", "sha256-"+hashHex(cfg)), cfg)
	write(t, filepath.Join(ol, "blobs", "sha256-"+wHash), weights)
	write(t, filepath.Join(ol, "blobs", "sha256-"+hashHex("half")+"-partial"), "hal")
	manifest := `{"schemaVersion":2,"config":{"digest":"sha256:` + hashHex(cfg) + `"},` +
		`"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:` + wHash + `"}]}`
	write(t, filepath.Join(ol, "manifests", "registry.ollama.ai", "library", "coder", "latest"), manifest)

	// LM Studio: same weights again, as a plain file.
	write(t, filepath.Join(lm, "acme", "coder", "coder.gguf"), weights)
	return hf, ol, lm
}

// run invokes the CLI against the demo roots and returns exit, stdout,
// stderr.
func run(t *testing.T, hf, ol, lm string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--hf", hf, "--ollama", ol, "--lmstudio", lm}, args...)
	var out, errb bytes.Buffer
	code := Run(full, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"--version"}, &out, &out); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(out.String()) != "weightsweep 0.1.0" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestHelpMentionsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"--help"}, &out, &out); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, cmd := range []string{"scan", "dupes", "orphans", "plan", "prune"} {
		if !strings.Contains(out.String(), cmd) {
			t.Fatalf("help missing %q", cmd)
		}
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	cases := [][]string{
		{},                              // no command
		{"frobnicate"},                  // unknown command
		{"--bogus", "scan"},             // unknown flag
		{"scan", "--hf"},                // missing value
		{"scan", "--hash", "maybe"},     // bad hash mode
		{"scan", "--min-size", "a lot"}, // bad size
		{"scan", "extra-arg"},           // trailing argument
	}
	for _, argv := range cases {
		var out, errb bytes.Buffer
		if code := Run(argv, &out, &errb); code != ExitUsage {
			t.Errorf("Run(%v) exit = %d, want %d (stderr: %s)", argv, code, ExitUsage, errb.String())
		}
	}
}

func TestScanHumanOutput(t *testing.T) {
	hf, ol, lm := demoTree(t)
	code, out, errb := run(t, hf, ol, lm, "scan")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errb)
	}
	for _, want := range []string{
		"huggingface", "ollama", "lmstudio", "total",
		"duplicates: 1 group(s)",
		"orphans:    1 file(s)",
		"partial: 1 file(s)",
		"hashed 1 file(s)", // only the LM Studio file needed hashing
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("scan output missing %q:\n%s", want, out)
		}
	}
}

func TestScanJSONShape(t *testing.T) {
	hf, ol, lm := demoTree(t)
	code, out, _ := run(t, hf, ol, lm, "--json", "scan")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		Stores []struct {
			Store string `json:"store"`
			Blobs []struct {
				Class  string `json:"class"`
				Digest string `json:"digest"`
			} `json:"blobs"`
		} `json:"stores"`
		DupeGroups []struct {
			Blobs []struct{ Store string } `json:"blobs"`
		} `json:"dupe_groups"`
		DupeWasted  int64  `json:"dupe_wasted"`
		ToolVersion string `json:"tool_version"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(doc.Stores) != 3 || doc.ToolVersion != "0.1.0" {
		t.Fatalf("doc = %+v", doc)
	}
	if len(doc.DupeGroups) != 1 || len(doc.DupeGroups[0].Blobs) != 3 {
		t.Fatalf("dupes = %+v", doc.DupeGroups)
	}
	if doc.DupeWasted != 2*int64(len("shared-model-weights-0123456789")) {
		t.Fatalf("wasted = %d", doc.DupeWasted)
	}
}

func TestScanMissingRootsMarkedNotFound(t *testing.T) {
	base := t.TempDir()
	code, out, _ := run(t, filepath.Join(base, "a"), filepath.Join(base, "b"),
		filepath.Join(base, "c"), "scan")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Count(out, "(not found)") != 3 {
		t.Fatalf("missing roots not marked:\n%s", out)
	}
}

func TestScanHashNeverSkipsCorrelation(t *testing.T) {
	// Without hashing, the LM Studio copy has no digest, so the dupe
	// group shrinks to the two name-addressed copies.
	hf, ol, lm := demoTree(t)
	code, out, _ := run(t, hf, ol, lm, "--json", "scan", "--hash", "never")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		DupeGroups []struct {
			Blobs []struct{ Store string } `json:"blobs"`
		} `json:"dupe_groups"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.DupeGroups) != 1 || len(doc.DupeGroups[0].Blobs) != 2 {
		t.Fatalf("dupes = %+v", doc.DupeGroups)
	}
}

func TestScanVerifyFlagsCorruption(t *testing.T) {
	hf, ol, lm := demoTree(t)
	// Corrupt the Ollama weights: the name promises different bytes.
	weights := "shared-model-weights-0123456789"
	corrupt := filepath.Join(ol, "blobs", "sha256-"+hashHex(weights))
	if err := os.WriteFile(corrupt, []byte("bitrot-bitrot-bitrot-bitrot-bit"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, hf, ol, lm, "scan", "--verify")
	if code != ExitDirty {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitDirty, out)
	}
	if !strings.Contains(out, "CORRUPT: "+corrupt) {
		t.Fatalf("corruption not reported:\n%s", out)
	}
}

func TestDupesHumanAndJSON(t *testing.T) {
	hf, ol, lm := demoTree(t)
	code, out, _ := run(t, hf, ol, lm, "dupes")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "1 duplicate group(s)") ||
		!strings.Contains(out, "huggingface+ollama+lmstudio") ||
		!strings.Contains(out, "coder.gguf") {
		t.Fatalf("dupes output:\n%s", out)
	}
	_, jout, _ := run(t, hf, ol, lm, "--json", "dupes")
	var doc struct {
		Groups []struct{ Wasted int64 } `json:"groups"`
		Wasted int64                    `json:"wasted"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Groups) != 1 || doc.Wasted != doc.Groups[0].Wasted {
		t.Fatalf("doc = %+v", doc)
	}
	// --min-size above the fixture's blob size empties the report.
	code, out, _ = run(t, hf, ol, lm, "dupes", "--min-size", "1KiB")
	if code != ExitOK || !strings.Contains(out, "no duplicate blobs found") {
		t.Fatalf("min-size ignored (exit %d):\n%s", code, out)
	}
}

func TestOrphansListsAllReclaimableClasses(t *testing.T) {
	hf, ol, lm := demoTree(t)
	code, out, _ := run(t, hf, ol, lm, "orphans")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "orphan") || !strings.Contains(out, "partial") {
		t.Fatalf("orphans output:\n%s", out)
	}
	if !strings.Contains(out, "2 file(s)") {
		t.Fatalf("summary line wrong:\n%s", out)
	}
	// Empty stores → friendly message, not an empty table.
	base := t.TempDir()
	_, out2, _ := run(t, filepath.Join(base, "a"), filepath.Join(base, "b"),
		filepath.Join(base, "c"), "orphans")
	if !strings.Contains(out2, "no orphaned, stale or partial files found") {
		t.Fatalf("empty message missing:\n%s", out2)
	}
}

func TestPlanWritesFileWithSummary(t *testing.T) {
	hf, ol, lm := demoTree(t)
	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, out, _ := run(t, hf, ol, lm, "plan", "--out", planPath)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "wrote "+planPath) || !strings.Contains(out, "safe:") {
		t.Fatalf("plan summary:\n%s", out)
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		FormatVersion int `json:"format_version"`
		Actions       []struct {
			Safety string `json:"safety"`
			Reason string `json:"reason"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	// orphan + partial (safe) and the two redundant duplicate copies
	// (review) — the keeper copy gets no action.
	if doc.FormatVersion != 1 || len(doc.Actions) != 4 {
		t.Fatalf("plan = %+v", doc)
	}
	safe, review := 0, 0
	for _, a := range doc.Actions {
		if a.Safety == "safe" {
			safe++
		} else {
			review++
		}
	}
	if safe != 2 || review != 2 {
		t.Fatalf("safe=%d review=%d", safe, review)
	}
	// Without --out the JSON goes to stdout instead.
	code, out, _ = run(t, hf, ol, lm, "plan")
	if code != ExitOK || !strings.Contains(out, `"format_version": 1`) {
		t.Fatalf("stdout plan missing (exit %d):\n%s", code, out)
	}
}

func TestPruneRejectsMissingOrCorruptPlan(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"prune"}, &out, &errb); code != ExitUsage {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errb.String(), "--plan") {
		t.Fatalf("stderr = %q", errb.String())
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	write(t, planPath, "{broken")
	errb.Reset()
	if code := Run([]string{"prune", "--plan", planPath}, &out, &errb); code != ExitUsage {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errb.String(), "not a weightsweep plan") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestPruneDryRunThenApply(t *testing.T) {
	hf, ol, lm := demoTree(t)
	planPath := filepath.Join(t.TempDir(), "plan.json")
	run(t, hf, ol, lm, "plan", "--out", planPath)

	orphan := filepath.Join(hf, "models--acme--coder", "blobs", hashHex("abandoned"))
	partial := filepath.Join(ol, "blobs", "sha256-"+hashHex("half")+"-partial")
	liveWeights := filepath.Join(ol, "blobs", "sha256-"+hashHex("shared-model-weights-0123456789"))

	// Dry run: reports, deletes nothing.
	code, out, _ := run(t, hf, ol, lm, "prune", "--plan", planPath)
	if code != ExitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "would free") {
		t.Fatalf("dry-run output:\n%s", out)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("dry run deleted the orphan")
	}

	// Apply: safe actions go, review (duplicate) stays.
	code, out, _ = run(t, hf, ol, lm, "prune", "--plan", planPath, "--apply")
	if code != ExitOK {
		t.Fatalf("apply exit = %d\n%s", code, out)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan survived apply")
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal("partial survived apply")
	}
	if _, err := os.Stat(liveWeights); err != nil {
		t.Fatal("live blob deleted by safe apply")
	}
	if !strings.Contains(out, "skipped") {
		t.Fatalf("review skip not reported:\n%s", out)
	}

	// Re-apply is idempotent: everything already gone is a skip.
	code, _, _ = run(t, hf, ol, lm, "prune", "--plan", planPath, "--apply")
	if code != ExitOK {
		t.Fatalf("re-apply exit = %d", code)
	}
}

func TestPruneIncludeReviewRemovesDuplicateCopy(t *testing.T) {
	hf, ol, lm := demoTree(t)
	planPath := filepath.Join(t.TempDir(), "plan.json")
	run(t, hf, ol, lm, "plan", "--out", planPath)
	code, _, _ := run(t, hf, ol, lm, "prune", "--plan", planPath, "--apply", "--include-review")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	// The keeper is the HF copy (canonical store order on a ref tie);
	// exactly one of the three copies must have been dropped.
	weights := "shared-model-weights-0123456789"
	hfCopy := filepath.Join(hf, "models--acme--coder", "blobs", hashHex(weights))
	lmCopy := filepath.Join(lm, "acme", "coder", "coder.gguf")
	if _, err := os.Stat(hfCopy); err != nil {
		t.Fatal("keeper copy deleted")
	}
	_, olErr := os.Stat(filepath.Join(ol, "blobs", "sha256-"+hashHex(weights)))
	_, lmErr := os.Stat(lmCopy)
	gone := 0
	if os.IsNotExist(olErr) {
		gone++
	}
	if os.IsNotExist(lmErr) {
		gone++
	}
	if gone != 2 {
		t.Fatalf("expected both redundant copies gone, got %d (ol:%v lm:%v)", gone, olErr, lmErr)
	}
}

func TestPruneJSONOutcomes(t *testing.T) {
	hf, ol, lm := demoTree(t)
	planPath := filepath.Join(t.TempDir(), "plan.json")
	run(t, hf, ol, lm, "plan", "--out", planPath)
	code, out, _ := run(t, hf, ol, lm, "--json", "prune", "--plan", planPath)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		Outcomes []struct {
			Status string `json:"status"`
		} `json:"outcomes"`
		FreedBytes int64 `json:"freed_bytes"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Outcomes) == 0 || doc.FreedBytes == 0 {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestInlineFlagSyntax(t *testing.T) {
	hf, ol, lm := demoTree(t)
	var out, errb bytes.Buffer
	code := Run([]string{"--hf=" + hf, "--ollama=" + ol, "--lmstudio=" + lm, "scan"}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if strings.Contains(out.String(), "(not found)") {
		t.Fatalf("inline flags not honored:\n%s", out.String())
	}
}

func TestRelativeRootsYieldAbsolutePlanPaths(t *testing.T) {
	// Regression: a plan built from `--hf ./demo` must still be
	// executable — prune's guard rejects relative paths outright.
	hf, ol, lm := demoTree(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Dir(hf)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, _, errb := run(t, filepath.Base(hf), filepath.Base(ol), filepath.Base(lm),
		"plan", "--out", planPath)
	if code != ExitOK {
		t.Fatalf("plan exit = %d, stderr = %s", code, errb)
	}
	code, out, _ := run(t, filepath.Base(hf), filepath.Base(ol), filepath.Base(lm),
		"prune", "--plan", planPath, "--apply")
	if code != ExitOK {
		t.Fatalf("prune exit = %d\n%s", code, out)
	}
	if strings.Contains(out, "not absolute") {
		t.Fatalf("plan carried relative paths:\n%s", out)
	}
	orphan := filepath.Join(hf, "models--acme--coder", "blobs", hashHex("abandoned"))
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan survived relative-root prune")
	}
}

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"1048576": 1048576,
		"100KB":   100_000,
		"100KiB":  102400,
		"1.5GiB":  int64(1.5 * float64(1<<30)),
		"2 MiB":   2 << 20,
		"7b":      7,
		"0":       0,
		"3tb":     3_000_000_000_000,
	}
	for in, want := range good {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GiB", "-5MB", "ten", "1.2.3KB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
}
