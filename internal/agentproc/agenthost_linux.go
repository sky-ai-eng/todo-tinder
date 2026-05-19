//go:build linux

package agentproc

import (
	"strings"
)

// rewriteAllowedToolsForSandbox swaps the host TF binary path in the
// allowlist string for the canonical in-sandbox path. The allowlist
// is a comma-separated list of patterns; BuildAllowedTools emits
// exactly one pattern matching the TF binary
// (`Bash(<selfBin> exec *)`), so a single string replace is precise.
//
// Why this matters: Claude Code applies a per-tool path check against
// the literal pattern before exec. If the pattern names a host path
// the sandbox can't reach, every `triagefactory exec` invocation
// from inside the sandbox is denied — even though the bind-mount at
// /usr/local/bin/triagefactory makes the binary reachable. The
// allowlist must agree with reality.
//
// Empty hostSelfBin (os.Executable failed) returns the allowlist
// unchanged; the caller surfaces the original os.Executable error
// elsewhere. Same for a hostSelfBin already equal to the sandbox
// target (production multi-mode container installs at /usr/local/bin)
// — the replace would no-op but skipping spares the allocation.
func rewriteAllowedToolsForSandbox(allowedTools, hostSelfBin string) string {
	if hostSelfBin == "" || hostSelfBin == sandboxTFBinary {
		return allowedTools
	}
	return strings.ReplaceAll(allowedTools, hostSelfBin, sandboxTFBinary)
}
