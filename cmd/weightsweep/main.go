// Command weightsweep analyzes Hugging Face, Ollama and LM Studio model
// caches on one machine: it correlates blobs across the three stores by
// content hash, reports duplicates and orphans, and produces reviewable
// prune plans. Everything runs offline against the local filesystem.
package main

import (
	"os"

	"github.com/JaydenCJ/weightsweep/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
