package workspace

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// runAdd materializes a worktree for the run identified by
// $TRIAGE_FACTORY_RUN_ID. Prints the absolute worktree path on stdout
// (no decoration) so the agent can `cd "$(... workspace add owner/repo)"`.
//
// Idempotent: if `add` was already called for this (run, repo), the
// existing worktree's path is printed and exit is 0 — no second
// `git worktree add`, no duplicate row.
func runAdd(database *db.DB, args []string) {
	if len(args) == 0 {
		exitErr("workspace add: missing argument; expected owner/repo")
	}
	ownerRepo := strings.TrimSpace(args[0])
	owner, repo, ok := splitOwnerRepo(ownerRepo)
	if !ok {
		exitErr("workspace add: invalid owner/repo: " + args[0])
	}
	repoID := owner + "/" + repo

	runID := os.Getenv("TRIAGE_FACTORY_RUN_ID")
	if runID == "" {
		exitErr("workspace add: TRIAGE_FACTORY_RUN_ID not set; this command must be invoked by the delegated agent")
	}

	// Validate the run exists and is a Jira run. GitHub PR runs have a
	// pre-materialized worktree managed by the spawner; allowing a
	// `workspace add` against them would create a parallel worktree the
	// spawner doesn't know to clean up.
	run, err := db.GetAgentRun(database.Conn, runID)
	if err != nil {
		exitErr("workspace add: load run: " + err.Error())
	}
	if run == nil {
		exitErr("workspace add: run " + runID + " not found")
	}
	task, err := db.GetTask(database.Conn, run.TaskID)
	if err != nil {
		exitErr("workspace add: load task: " + err.Error())
	}
	if task == nil {
		exitErr("workspace add: task " + run.TaskID + " not found")
	}
	if task.EntitySource != "jira" {
		exitErr("workspace add: only supported for Jira runs (this run's task source is " + task.EntitySource + "); GitHub PR runs have an eagerly-materialized worktree")
	}

	// Idempotency: if already materialized for this (run, repo), reuse.
	existing, err := db.GetRunWorktreeByRepo(database.Conn, runID, repoID)
	if err != nil {
		exitErr("workspace add: lookup existing worktree: " + err.Error())
	}
	if existing != nil {
		fmt.Println(existing.Path)
		return
	}

	profile, err := db.GetRepoProfile(database.Conn, repoID)
	if err != nil {
		exitErr("workspace add: load repo profile: " + err.Error())
	}
	if profile == nil {
		exitErr("workspace add: repo " + repoID + " is not configured in Triage Factory; add it on the Settings page first")
	}
	if profile.CloneURL == "" {
		exitErr("workspace add: repo " + repoID + " has no clone URL on its profile; try re-profiling from the Settings page")
	}

	baseBranch := profile.BaseBranch
	if baseBranch == "" {
		baseBranch = profile.DefaultBranch
	}
	featureBranch := "feature/" + task.EntitySourceID
	runRoot := worktree.RunRoot(runID)

	wtPath, err := worktree.CreateForBranchInRoot(
		context.Background(),
		profile.Owner, profile.Repo,
		profile.CloneURL,
		baseBranch, featureBranch,
		runID, runRoot,
	)
	if err != nil {
		exitErr("workspace add: create worktree: " + err.Error())
	}

	inserted, winningPath, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID:         runID,
		RepoID:        repoID,
		Path:          wtPath,
		FeatureBranch: featureBranch,
	})
	if err != nil {
		// Failed to record the row — the worktree is on disk but the
		// spawner's cleanup defer (which lists run_worktrees) won't
		// reap it. Roll back the on-disk worktree so the run terminates
		// cleanly, surface the error, and let the agent retry.
		if rmErr := worktree.RemoveAt(wtPath, runID); rmErr != nil {
			fmt.Fprintf(os.Stderr, "workspace add: rollback after insert failure: %v\n", rmErr)
		}
		exitErr("workspace add: record worktree: " + err.Error())
	}
	if !inserted {
		// Race-loss: another concurrent `workspace add` for the same
		// (run, repo) won. Their on-disk worktree is registered in the
		// row that won; ours is a duplicate at the same path that we
		// must remove to avoid an orphan registration. Same target
		// path, but RemoveAt + pruneAll on the loser still resets the
		// bare's tracking cleanly.
		if rmErr := worktree.RemoveAt(wtPath, runID); rmErr != nil {
			fmt.Fprintf(os.Stderr, "workspace add: cleanup loser worktree after race: %v\n", rmErr)
		}
		fmt.Println(winningPath)
		return
	}

	fmt.Println(wtPath)
}

// splitOwnerRepo splits "owner/repo" once. Both halves must be non-empty.
func splitOwnerRepo(s string) (owner, repo string, ok bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
