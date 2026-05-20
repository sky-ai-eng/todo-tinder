-- +goose Up
-- SKY-345 (SQLite mirror). Matches the Postgres migration's column
-- addition; SQLite's local-mode bootstrap only ever creates the
-- single LocalDefaultOrg, which stays is_personal=false. The column
-- is read by multi-mode-only code in the auth callback; local mode
-- never branches on it. We mirror the schema anyway so the per-store
-- query surface is identical across backends — every SELECT against
-- orgs can reference the column without dialect-specific branching.

ALTER TABLE orgs ADD COLUMN is_personal INTEGER NOT NULL DEFAULT 0;

-- +goose Down
SELECT 'down not supported';
