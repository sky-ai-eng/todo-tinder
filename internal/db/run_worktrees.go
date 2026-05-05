package db

import (
	"database/sql"
	"fmt"
	"time"
)

// RunWorktree records a worktree materialized for a given run by the
// agent's `triagefactory exec workspace add` invocation. The composite
// PK (run_id, repo_id) makes the insert idempotent: a second `workspace
// add` for the same repo within the same run hits ON CONFLICT and the
// caller looks up the existing path.
//
// Path is the absolute on-disk worktree location, typically
// /tmp/triagefactory-runs/{run_id}/{owner}/{repo}/. FeatureBranch is
// the branch name `git worktree add` checked out (e.g. "feature/SKY-220").
//
// FK to runs cascades on delete so wiping a run cleans up its worktree
// records here. The on-disk worktrees themselves are reaped by the
// spawner's runAgent cleanup defer (which lists this table at run
// terminal) plus the startup orphan sweep at worktree.CleanupWithOptions.
type RunWorktree struct {
	RunID         string
	RepoID        string
	Path          string
	FeatureBranch string
	CreatedAt     time.Time
}

// InsertRunWorktree records a freshly-materialized worktree. Caller must
// have already created the worktree on disk at w.Path.
//
// On PK conflict (concurrent CLI invocations for the same (run_id,
// repo_id)) the existing row's path is returned with inserted=false so
// the caller can clean up its just-created loser worktree without
// confusing future readers about which path actually maps to the row.
//
// Race shape: two `workspace add owner/repo` processes can both pass
// the GetRunWorktreeByRepo "not found" check, both call
// CreateForBranchInRoot, and arrive here racing. The per-repo lock
// inside the worktree library serializes the bare-side git operations,
// but it can't prevent two distinct on-disk worktree paths if the
// two processes computed different paths — same here, both computed
// the same `{runRoot}/{owner}/{repo}` path. The second `git worktree
// add` will fail because the dir is in use; that's caught at the
// call site. If by some path the second process did land a worktree,
// the loser-cleanup path here makes the side-table state authoritative.
func InsertRunWorktree(database *sql.DB, w RunWorktree) (inserted bool, winningPath string, err error) {
	res, err := database.Exec(`
		INSERT OR IGNORE INTO run_worktrees (run_id, repo_id, path, feature_branch)
		VALUES (?, ?, ?, ?)
	`, w.RunID, w.RepoID, w.Path, w.FeatureBranch)
	if err != nil {
		return false, "", fmt.Errorf("insert run_worktree: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, "", fmt.Errorf("rows affected: %w", err)
	}
	if rows == 1 {
		return true, w.Path, nil
	}
	// Row already existed — look up the winning path.
	existing, err := GetRunWorktreeByRepo(database, w.RunID, w.RepoID)
	if err != nil {
		return false, "", fmt.Errorf("read existing run_worktree after conflict: %w", err)
	}
	if existing == nil {
		// Theoretically impossible: INSERT OR IGNORE matched a row that
		// then vanished. Surface as an error rather than papering over
		// a real DB weirdness.
		return false, "", fmt.Errorf("run_worktree row vanished after INSERT OR IGNORE conflict (run_id=%s, repo_id=%s)", w.RunID, w.RepoID)
	}
	return false, existing.Path, nil
}

// GetRunWorktreeByRepo fetches the worktree row for a (run_id, repo_id)
// pair, or (nil, nil) if none exists. Used by the CLI to short-circuit
// the create+insert path when the agent re-invokes `workspace add`
// against an already-materialized repo.
func GetRunWorktreeByRepo(database *sql.DB, runID, repoID string) (*RunWorktree, error) {
	row := database.QueryRow(`
		SELECT run_id, repo_id, path, feature_branch, created_at
		  FROM run_worktrees
		 WHERE run_id = ? AND repo_id = ?
	`, runID, repoID)
	var w RunWorktree
	if err := row.Scan(&w.RunID, &w.RepoID, &w.Path, &w.FeatureBranch, &w.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

// GetRunWorktrees returns every worktree materialized for a run, in
// insertion order. The spawner's cleanup defer iterates this list and
// calls worktree.RemoveAt on each path before nuking the run-root.
func GetRunWorktrees(database *sql.DB, runID string) ([]RunWorktree, error) {
	rows, err := database.Query(`
		SELECT run_id, repo_id, path, feature_branch, created_at
		  FROM run_worktrees
		 WHERE run_id = ?
		 ORDER BY created_at ASC, repo_id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RunWorktree{}
	for rows.Next() {
		var w RunWorktree
		if err := rows.Scan(&w.RunID, &w.RepoID, &w.Path, &w.FeatureBranch, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
