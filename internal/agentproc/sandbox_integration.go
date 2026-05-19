package agentproc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

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
//
// SECURITY: uses os.Lchown (not os.Chown) so a symlink inside the
// repo can't redirect the chown to a host file outside the worktree.
// filepath.Walk does not follow symlinks during the walk itself, so
// the recursion stays inside the worktree; the per-entry Lchown
// ensures we change the link's own owner rather than the target's.
// Without this, a repo containing `link -> /etc/passwd` would chown
// the host's passwd file when this runs as root in multi mode.
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
		if cerr := os.Lchown(path, sandbox.WorktreeUID, sandbox.WorktreeGID); cerr != nil {
			return fmt.Errorf("lchown %s: %w", path, cerr)
		}
		return nil
	})
}

// translateAddDirsForSandbox rewrites the host paths in opts.AddDirs
// into their sandbox-side equivalents under /work. The agent's tool
// permission checks consume these via `--add-dir` flags; if we
// leave them as host paths (e.g. /data/worktrees/abc/knowledge-base)
// the agent's path checks reject every write attempt because no such
// path exists inside the sandbox rootfs.
//
// Paths that aren't under cwd are dropped — they're not reachable
// from inside the sandbox, so there's nothing useful to do with
// them. Empty entries are dropped too (matches BuildArgs's own
// behavior).
//
// Returns nil for nil input, an empty slice for an empty/all-dropped
// input, so the caller can distinguish "not set" from "set to nothing
// after filtering."
func translateAddDirsForSandbox(addDirs []string, cwd string) []string {
	if len(addDirs) == 0 {
		return nil
	}
	if cwd == "" {
		// Without cwd we can't compute relative paths; safest to
		// drop everything rather than pass through host paths that
		// don't exist in the sandbox.
		return []string{}
	}
	out := make([]string, 0, len(addDirs))
	for _, dir := range addDirs {
		if dir == "" {
			continue
		}
		// filepath.Rel handles both absolute paths under cwd and
		// already-relative paths. Anything that comes back with
		// ".." prefix is outside cwd; drop it.
		rel, err := filepath.Rel(cwd, dir)
		if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
			continue
		}
		// "/work" + relative path. Use filepath.Join to handle the
		// rel == "." case (cwd itself), which becomes "/work".
		out = append(out, filepath.Join("/work", rel))
	}
	return out
}
