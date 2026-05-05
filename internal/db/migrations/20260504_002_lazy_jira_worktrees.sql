-- Lazy worktree materialization for Jira delegations (SKY-233).
--
-- Replaces the AI scorer's repo-matching path. The old contract: scorer
-- guesses which repo a Jira ticket belongs to, writes tasks.matched_repos,
-- spawner reads it. The 0-match path (no repo guessed) launched the agent
-- against an empty temp dir with the implementation prompt — it could
-- read the ticket but had no codebase, no git, no GitHub tools, and burned
-- turns until MaxTurns hit. Empirical "agent spins until failing" symptom.
--
-- New contract: spawner does NOT pre-clone for Jira. The delegated agent
-- reads the ticket, decides which repo(s) it needs, and materializes them
-- via `triagefactory exec workspace add <owner/repo>`. Each call lands a
-- worktree at /tmp/triagefactory-runs/{run-id}/{owner}/{repo}/. This
-- table is the source of truth for which worktrees exist per run; it
-- drives cleanup at run terminal and across-restart orphan recovery.
--
-- run_id is the FK; ON DELETE CASCADE so deleting a run row reaps its
-- worktree records (the on-disk worktree is reaped separately via the
-- spawner's defer / startup orphan sweep). Composite PK (run_id, repo_id)
-- enforces idempotency: concurrent `workspace add owner/repo` invocations
-- for the same run resolve to the same row, second insert no-ops.
CREATE TABLE IF NOT EXISTS run_worktrees (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    repo_id TEXT NOT NULL,           -- "owner/repo"
    path TEXT NOT NULL,              -- absolute worktree path on disk
    feature_branch TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, repo_id)
);
CREATE INDEX IF NOT EXISTS idx_run_worktrees_run ON run_worktrees(run_id);

-- Drop the AI scorer's repo-matching columns. SQLite 3.35+ supports
-- ALTER TABLE ... DROP COLUMN. The columns were:
--   matched_repos  — JSON array of repo IDs the scorer guessed
--   blocked_reason — "multi_repo" | "no_repo_match" | NULL
-- Replaced by agent-driven workspace materialization (table above).
ALTER TABLE tasks DROP COLUMN matched_repos;
ALTER TABLE tasks DROP COLUMN blocked_reason;
