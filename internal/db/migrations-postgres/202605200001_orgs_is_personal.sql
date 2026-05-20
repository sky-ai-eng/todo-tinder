-- +goose Up
-- SKY-345: distinguish a user's personal org (auto-provisioned on first
-- signup) from explicit enterprise orgs. Future UI keys off this — "you
-- can't delete your personal org", "personal orgs don't appear in the
-- enterprise org picker for billing" — so paying the one-bit cost now
-- avoids a backfill later.
--
-- Race safety: we deliberately do NOT add a UNIQUE constraint on
-- (owner_user_id, is_personal). The data model allows a user to own a
-- personal org and also create another org (rare but not forbidden);
-- the signup-callback provisioning is race-protected by an in-tx
-- pg_advisory_xact_lock keyed by the user UUID, plus an inside-tx
-- "user has zero memberships" check — that's the right enforcement
-- point, not a schema-level uniqueness rule.

ALTER TABLE public.orgs ADD COLUMN is_personal boolean NOT NULL DEFAULT false;

-- +goose Down
SELECT 'down not supported';
