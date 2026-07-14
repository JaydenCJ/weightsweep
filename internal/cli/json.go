package cli

import (
	"encoding/json"
	"io"
)

// writeJSON renders v as indented JSON with a trailing newline. Every
// --json code path funnels through here so output shape stays uniform.
func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
