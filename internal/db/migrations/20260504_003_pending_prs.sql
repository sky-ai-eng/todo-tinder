-- Pending-PR approval flow (SKY-???).
--
-- Mirrors the existing pending-reviews shape: the agent calls
-- `triagefactory exec gh pr create` from inside its worktree to QUEUE
-- a draft PR; the run flips to `pending_approval`; the user reviews
-- title/body/diff and either approves (server actually opens the PR
-- on GitHub with a TF footer) or eventually rejects (SKY-235 follow-
-- up). The agent must `git push` upstream BEFORE calling pr create
-- so the diff source (bare-clone fetch from origin) and GitHub's
-- CreatePR API both see the head ref.
--
-- Schema notes:
--   run_id UNIQUE       — at most one pending PR per run, parallel to
--                         how reviews work today (one review per run).
--                         FK CASCADE so deleting the run reaps the row.
--   head_sha            — captured at queue time so the UI can flag
--                         drift if the agent pushes a fixup mid-
--                         approval ("queued at abc123, now def456").
--   original_title      — snapshot for the human-feedback diff
--   original_body         that lands in run_memory.human_content
--                         after the user submits with edits. Mirror
--                         of pending_reviews' write-once originals
--                         pattern. Nullable so legacy rows stay nil.
--   locked              — agent-side anti-retry sentinel. Set by
--                         LockPendingPR's WHERE locked = 0 gate.
--                         Cleaner than reviews' accidental
--                         (review_event != '') sentinel.
--   submitted_at        — concurrent-submit guard. Two browser tabs
--                         clicking Submit can't both call CreatePR
--                         on GitHub; the UPDATE ... WHERE
--                         submitted_at IS NULL only matches once.
--                         Reviews don't have this today; porting
--                         back is a separate cleanup.
CREATE TABLE IF NOT EXISTS pending_prs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
    owner TEXT NOT NULL,
    repo TEXT NOT NULL,
    head_branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT,
    original_title TEXT,
    original_body TEXT,
    locked INTEGER NOT NULL DEFAULT 0,
    submitted_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_pending_prs_run ON pending_prs(run_id);
