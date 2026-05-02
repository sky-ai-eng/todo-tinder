-- Projects: top-level concept that segments work items by *concept*
-- rather than by repo (SKY-211 / SKY-215). Pure data layer here —
-- the Curator runtime that populates `designer_session_id` and the
-- summary regenerator that fills `summary_md` land in later tickets
-- (SKY-216, SKY-220). UI is decoupled from this migration.
--
-- pinned_repos is JSON because the natural shape is a small list of
-- "owner/repo" slugs and modeling it as a separate junction table
-- buys nothing here: there are no per-pinning attributes, no FK
-- target (repos aren't first-class entities yet), and the read
-- pattern is "give me all pins for this project" — a single JSON
-- column round-trip vs a join. If a forge other than GitHub ever
-- shows up, the slug shape will need migration; flagged in the
-- ticket as not blocking v1.
CREATE TABLE IF NOT EXISTS projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    -- summary_md is the ~100-line distilled project context the
    -- classifier ticket (SKY-220) regenerates from the
    -- knowledge-base files. Nullable until that runtime ships.
    summary_md TEXT,
    -- summary_stale flips true when knowledge-base files change so
    -- the regenerator knows there's work to do. The regenerator
    -- flips it back on success.
    summary_stale BOOLEAN NOT NULL DEFAULT 0,
    -- designer_session_id is the long-lived Claude Code session id
    -- the Curator resumes against. Populated by SKY-216 once the
    -- Curator session lifecycle is wired into the spawner.
    designer_session_id TEXT,
    -- pinned_repos is a JSON array of "owner/repo" slugs. Stored as
    -- TEXT (json1 functions accept it) — empty array '[]' is the
    -- semantic default; NULL would mean "unset" which we don't
    -- want as an option for this field.
    pinned_repos TEXT NOT NULL DEFAULT '[]',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Add project_id to entities so an entity can be assigned to a
-- project. Nullable: most entities won't have a project until the
-- classifier (SKY-220) tags them. ON DELETE SET NULL because
-- deleting a project shouldn't lose the work items it covered;
-- they just become un-tagged again.
--
-- ALTER TABLE ADD COLUMN with a REFERENCES clause is supported by
-- SQLite ≥ 3.6.19 and the FK fires on subsequent writes; existing
-- rows are NULL by default so the FK has nothing to validate
-- against on the way in. PRAGMA foreign_keys=ON in main.go gates
-- enforcement at runtime.
ALTER TABLE entities ADD COLUMN project_id TEXT REFERENCES projects(id) ON DELETE SET NULL;

-- Partial index on project_id: skips the un-assigned rows (NULL)
-- so the footprint stays proportional to assigned-entity volume.
-- Drives the future "list entities in project X" query that the
-- projects page will run.
CREATE INDEX IF NOT EXISTS idx_entities_project_id ON entities(project_id)
    WHERE project_id IS NOT NULL;
