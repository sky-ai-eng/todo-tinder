-- Per-project tracker links (SKY-217). A project can be associated
-- with a Jira project, a Linear project, both, or neither. The two
-- columns are independent — there's no discriminator and no both-or-
-- neither constraint, because a team running both Jira and Linear
-- legitimately wants to see both surfaces in one place.
--
-- Validation lives at the API layer (server/projects.go): the value,
-- when non-empty, must match a project key already declared in the
-- corresponding integration's config (config.Jira.Projects today,
-- config.Linear.* once that integration ships). Storing free-form
-- strings here keeps the DB layer integration-agnostic — the migration
-- doesn't need to know the shape of either tracker.
--
-- NULL semantics: NULL = "not linked." Empty string is normalized to
-- NULL by the handler so the column always reads cleanly. This matches
-- how summary_md and curator_session_id already handle "unset" on this
-- table.

ALTER TABLE projects ADD COLUMN jira_project_key TEXT;
ALTER TABLE projects ADD COLUMN linear_project_key TEXT;
