-- Curator pending context (SKY-224). Per-project, per-session queue of
-- "things changed since the agent last saw the world" — pinned repos
-- changed, tracker key changed. Goroutine drains these on the next
-- user message and prepends them as a hidden [system note] block so
-- the agent always sees current ground truth without us depending on
-- Claude Code's resume-time --append-system-prompt mutability.
--
-- Coalescing: at most one PENDING (unconsumed) row per
-- (project_id, curator_session_id, change_type). The handler upserts
-- with ON CONFLICT DO NOTHING so repeated PATCHes between user messages
-- preserve the *earliest* baseline_value (the snapshot before the first
-- unconsumed change). At consume time the goroutine renders the diff
-- between baseline_value and the project row's current state, which
-- naturally folds A→B→A round-trips into "no actual change" without us
-- having to track each intermediate edit.
--
-- Lifecycle is a two-phase consume so a failed dispatch doesn't lose
-- the user's deltas:
--   1. Goroutine claims rows by setting consumed_at + consumed_by_request_id
--      atomically before agentproc.Run.
--   2. On terminal `done`, FinalizePendingContext deletes the consumed rows.
--   3. On `cancelled` or `failed`, RevertPendingContext flips consumed_at
--      back to NULL so the next user message picks them up. New rows that
--      landed mid-dispatch (state 2 → state 2+1) are merged into the
--      reverted ones — the older row's baseline is the truer "earliest
--      unconsumed snapshot" so the newer is dropped.
--
-- Session-reset cleanup: when curator_session_id is cleared on a project
-- (orphan-cleanup, future user-driven reset), pending rows for the dead
-- session are deleted. The new session's static envelope renders fresh
-- values so describing a transition the new agent never saw is just
-- noise. The FK cascade on projects(id) covers project deletion.

CREATE TABLE IF NOT EXISTS curator_pending_context (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    curator_session_id TEXT NOT NULL,
    change_type TEXT NOT NULL,
    -- baseline_value is JSON: arrays for pinned_repos, scalar string
    -- (or null) wrapped in JSON for tracker keys. Stored as the value
    -- right before the first unconsumed PATCH applied; renderer diffs
    -- it against the project row's current state at consume time.
    baseline_value TEXT NOT NULL,
    consumed_at DATETIME,
    consumed_by_request_id TEXT REFERENCES curator_requests(id) ON DELETE SET NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- "At most one pending row per (session, change_type)" — the partial
-- predicate excludes consumed rows, so a NEW PATCH that lands while
-- the goroutine is still dispatching the previous batch does NOT
-- conflict with the in-flight consumed row. Revert merges the two.
CREATE UNIQUE INDEX IF NOT EXISTS idx_curator_pending_context_one_pending_per_type
    ON curator_pending_context(project_id, curator_session_id, change_type)
    WHERE consumed_at IS NULL;

-- Drives finalize/revert by request id.
CREATE INDEX IF NOT EXISTS idx_curator_pending_context_consumer
    ON curator_pending_context(consumed_by_request_id)
    WHERE consumed_by_request_id IS NOT NULL;
