//go:build linux

package agentproc

import (
	"strings"
	"testing"
)

// TestRewriteAllowedToolsForSandbox pins the load-bearing behavior of
// the allowlist rewrite: the host TF binary's exec rule becomes the
// in-sandbox canonical path so Claude Code's per-tool path check
// matches what the agent can actually exec. Without the rewrite,
// every `triagefactory exec` invocation from inside the sandbox is
// denied by Claude Code before it even reaches the kernel exec.
func TestRewriteAllowedToolsForSandbox(t *testing.T) {
	hostBin := "/home/user/triage-factory/triagefactory"
	allowlist := BuildAllowedTools(hostBin)

	got := rewriteAllowedToolsForSandbox(allowlist, hostBin)
	if strings.Contains(got, hostBin) {
		t.Errorf("rewrite still contains host bin %q:\n%s", hostBin, got)
	}
	wantPattern := "Bash(" + sandboxTFBinary + " exec *)"
	if !strings.Contains(got, wantPattern) {
		t.Errorf("rewrite missing in-sandbox bin pattern %q:\n%s", wantPattern, got)
	}
}

// TestRewriteAllowedToolsForSandbox_EmptyHostBin pins the
// "os.Executable failed" path: an empty hostSelfBin must NOT trigger
// the rewrite (it would corrupt the allowlist by replacing every
// empty substring). Caller surfaces the original os.Executable error
// elsewhere.
func TestRewriteAllowedToolsForSandbox_EmptyHostBin(t *testing.T) {
	hostBin := "/anywhere/triagefactory"
	allowlist := BuildAllowedTools(hostBin)

	got := rewriteAllowedToolsForSandbox(allowlist, "")
	if got != allowlist {
		t.Errorf("empty host bin should pass through unchanged")
	}
}

// TestRewriteAllowedToolsForSandbox_AlreadyCanonical pins the
// production multi-mode shape: the container image installs the TF
// binary at /usr/local/bin/triagefactory, so os.Executable() returns
// exactly the in-sandbox path. The rewrite is a no-op in this case
// but the test pins it so a future refactor can't silently break the
// short-circuit.
func TestRewriteAllowedToolsForSandbox_AlreadyCanonical(t *testing.T) {
	allowlist := BuildAllowedTools(sandboxTFBinary)
	got := rewriteAllowedToolsForSandbox(allowlist, sandboxTFBinary)
	if got != allowlist {
		t.Errorf("already-canonical host bin should pass through unchanged")
	}
}
