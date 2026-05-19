package agentproc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// shouldSandbox decides whether the current Run invocation routes
// through the gVisor sandbox. Both conditions must hold:
//
//   - runmode.ModeMulti: local-mode users are trusted with their own
//     creds; sandboxing them is friction without isolation benefit
//     (single tenant). The local→multi-style sandbox toggle could
//     land later as defense-in-depth but isn't a v1 concern.
//   - runtime.GOOS == "linux": gVisor only works on Linux. Multi mode
//     on macOS isn't a supported config (the production runner image
//     is alpine Linux per SKY-256).
func shouldSandbox() bool {
	return runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"
}

// buildSandboxEnv constructs the *complete* env exposed to the
// sandboxed agent. PROPERTY B INVARIANT: this slice contains
// NO credential-shaped entries. The agent's process.env / FDs /
// memory contain only the keys below; a jailbroken agent dumping
// its own state into a tool result / commit message / model
// response leaks nothing usable.
//
// SKY-335 will append ANTHROPIC_BASE_URL + a placeholder
// ANTHROPIC_API_KEY to the returned slice so the agent can reach
// an in-host proxy that holds the real key. Until then, the agent
// will ENOAUTH against api.anthropic.com — that's the intended
// interim state (multi mode isn't shipped to users yet).
func buildSandboxEnv(extraEnv []string) []string {
	// Floor: just enough for Node to find its binaries + cache dirs.
	// Deliberately minimal; the sandbox's filesystem layout fills in
	// most of what HOME would normally point at.
	base := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/work",
		"TERM=xterm",
	}
	out := make([]string, 0, len(base)+len(extraEnv))
	out = append(out, base...)
	// ExtraEnv carries non-credential run-scoped metadata
	// (TRIAGE_FACTORY_RUN_ID etc). Callers that pass credential
	// env vars in ExtraEnv would violate Property B — that's a
	// caller bug, but the package's existing contract for ExtraEnv
	// is "run-scoped non-credential variables" so we trust it.
	out = append(out, extraEnv...)
	return out
}

// chownWorktreeForSandbox recursively chowns the worktree to the
// uid/gid the sandboxed agent runs as. Without this, the agent
// can't write to its own worktree (EACCES). Idempotent — chowning
// already-correctly-owned files is a no-op at the kernel level.
//
// On non-Linux this is a no-op; the sandbox path isn't reachable
// off Linux per shouldSandbox.
func chownWorktreeForSandbox(worktree string) error {
	if worktree == "" {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	return filepath.Walk(worktree, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if cerr := os.Chown(path, sandbox.WorktreeUID, sandbox.WorktreeGID); cerr != nil {
			return fmt.Errorf("chown %s: %w", path, cerr)
		}
		return nil
	})
}
