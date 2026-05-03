package worktree

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// CuratorRepoDirName is the per-pinned-repo directory name inside a
// project's working directory. Exposed so the curator runtime + tests
// agree on the layout — the agent's "the source for sky-ai-eng/sky
// is at ./repos/sky-ai-eng-sky" mental model is encoded here.
//
// Slash-replacement (not '/') is intentional: a literal owner/repo
// path would create a nested ./repos/sky-ai-eng/sky/ layout that
// makes `cd repos/<TAB>` ambiguous and breaks the simple
// "one entry per pinned repo" contract.
func CuratorRepoDirName(owner, repo string) string {
	return owner + "-" + repo
}

// EnsureCuratorWorktree materializes a per-project worktree of a
// pinned repo at <projectDir>/repos/<owner>-<repo>/, refreshing it
// to upstream HEAD on every call. Used by the curator's dispatch
// loop so each chat turn sees current code without the user having
// to manage refresh state.
//
// Behavior:
//
//   - If the bare clone is missing, returns an error. SKY-214's
//     bootstrap is the producer; calling this on an unconfigured repo
//     should never happen because validatePinnedRepos guards POST/
//     PATCH at the API layer.
//   - If the worktree directory doesn't exist yet, creates it via
//     `git worktree add` checked out to `branch`.
//   - If it already exists, runs `git fetch origin <branch>` then
//     `git reset --hard origin/<branch>`. Always-reset-hard is the
//     intended contract: the curator's working tree is "current state
//     of upstream," not a place to accumulate agent edits. If the
//     agent wrote to a tracked file, that change is treated as
//     ephemeral and dropped on the next dispatch.
//
// Returns the absolute worktree path. Holds the per-repo lock
// throughout so concurrent curator dispatches that pin the same repo
// queue rather than race on git state.
func EnsureCuratorWorktree(ctx context.Context, owner, repo, branch, projectDir string) (string, error) {
	if owner == "" || repo == "" {
		return "", fmt.Errorf("ensure curator worktree: owner/repo required")
	}
	if branch == "" {
		return "", fmt.Errorf("ensure curator worktree: branch required (caller resolves base_branch || default_branch)")
	}
	if projectDir == "" {
		return "", fmt.Errorf("ensure curator worktree: projectDir required")
	}

	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}
	if _, err := os.Stat(bareDir); err != nil {
		// Bare missing — bootstrap hasn't run for this repo yet, or
		// it failed. Don't lazy-clone here: the curator runtime calls
		// us per dispatch, and a missing bare for a pinned repo is a
		// configuration problem the user needs to see.
		if os.IsNotExist(err) {
			return "", fmt.Errorf("bare clone for %s/%s missing — repo profiling has not run yet", owner, repo)
		}
		return "", fmt.Errorf("stat bare: %w", err)
	}

	wtDir := filepath.Join(projectDir, "repos", CuratorRepoDirName(owner, repo))
	remoteRef := "refs/remotes/origin/" + branch

	// Fetch into the remote-tracking ref rather than the local
	// branch ref. The curator's per-project worktree keeps the local
	// branch checked out across dispatches; a `+refs/heads/<b>:refs/heads/<b>`
	// refspec would try to destructively overwrite the checked-out
	// branch and git refuses with "fatal: refusing to fetch into
	// branch ... checked out at <path>". Fetching into
	// refs/remotes/origin/<b> avoids that, and the subsequent reset
	// (or worktree add) points the local branch at the same SHA.
	branchRefspec := fmt.Sprintf("+refs/heads/%s:%s", branch, remoteRef)
	start := time.Now()
	if err := gitRunCtx(ctx, bareDir, "fetch", "origin", branchRefspec); err != nil {
		return "", fmt.Errorf("fetch %s/%s %s: %w", owner, repo, branch, err)
	}
	log.Printf("[worktree] curator fetch %s/%s %s in %s", owner, repo, branch, time.Since(start).Round(time.Millisecond))

	// Worktree already there → refresh in place. Reset hard to the
	// just-fetched remote-tracking ref makes the curator's view of
	// the world match upstream HEAD; without it, a previous
	// dispatch's agent edit to a tracked file would persist into
	// the next dispatch.
	if _, err := os.Stat(wtDir); err == nil {
		if err := gitRunCtx(ctx, wtDir, "reset", "--hard", remoteRef); err != nil {
			return "", fmt.Errorf("reset --hard %s: %w", branch, err)
		}
		// Also nuke untracked files so a previous dispatch's
		// scratch output doesn't leak into the next agent's view.
		if err := gitRunCtx(ctx, wtDir, "clean", "-fdx"); err != nil {
			// Non-fatal: the agent will still see a current tracked
			// state. Log and continue.
			log.Printf("[worktree] curator clean %s/%s: %v", owner, repo, err)
		}
		return wtDir, nil
	}

	// First materialization for this (project, repo) pair. -B
	// creates-or-resets the local branch from the remote-tracking
	// ref we just fetched, so the worktree has a real local branch
	// to commit on (even though the curator's contract is read-only,
	// having a proper local branch ref makes git operations like
	// `git log <branch>` inside the worktree work without dancing
	// around remote-tracking refs).
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent: %w", err)
	}
	if err := gitRunCtx(ctx, bareDir, "worktree", "add", "-B", branch, wtDir, remoteRef); err != nil {
		return "", fmt.Errorf("worktree add %s/%s %s: %w", owner, repo, branch, err)
	}
	log.Printf("[worktree] curator worktree %s @ %s/%s %s", wtDir, owner, repo, branch)
	return wtDir, nil
}

// PruneCuratorBare deregisters worktrees from a bare clone that no
// longer exist on disk. Called from the curator's project-delete
// hook AFTER the project's directory has been RemoveAll'd, so the
// bare's `worktrees/` registration list doesn't accumulate stale
// entries (which would block re-creating the same name in a future
// project).
//
// Best-effort: a prune failure is logged but doesn't bubble. The
// stale entries are recoverable via `git worktree prune` at any time
// and don't otherwise affect correctness.
func PruneCuratorBare(owner, repo string) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := repoDir(owner, repo)
	if err != nil {
		log.Printf("[worktree] curator prune resolve %s/%s: %v", owner, repo, err)
		return
	}
	if _, err := os.Stat(bareDir); err != nil {
		// Bare gone too — nothing to prune.
		return
	}
	if err := gitRun(bareDir, "worktree", "prune"); err != nil {
		log.Printf("[worktree] curator prune %s/%s: %v", owner, repo, err)
	}
}
