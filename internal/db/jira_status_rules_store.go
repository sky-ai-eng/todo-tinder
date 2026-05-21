package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// JiraStatusRulesStore owns the jira_project_status_rules table —
// one row per (team_id, project_key) carrying the team's per-project
// pickup/in_progress/done status configuration. Separate from
// TeamsStore because the table is multi-row per team with bulk-replace
// semantics (config.Save's clone-then-prune flow); folding it into
// the single-row TeamSettings struct would muddy the per-scope read
// shape.
//
// # Pool split (Postgres)
//
//   - ListForTeam, ReplaceForTeam run on the app pool. The
//     jira_rules_select / jira_rules_insert / jira_rules_update /
//     jira_rules_delete RLS policies gate reads by team membership
//     and writes by team admin; the request-handler caller has set
//     the JWT claims via the TxRunner.
//   - ListForTeamSystem runs on the admin pool. The poller manager
//     and scorer read team rules at boot/poll-tick without a JWT-
//     claims context.
//
// SQLite collapses the pool split to one connection; the `...System`
// variant delegates to its non-System counterpart.
type JiraStatusRulesStore interface {
	// ListForTeam returns the team's per-project rules in
	// project_key ascending order. Empty slice with nil error when
	// the team has no rules configured. Postgres routes through the
	// app pool.
	ListForTeam(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error)

	// ListForTeamSystem mirrors ListForTeam but routes through the
	// admin pool in Postgres for callers without a JWT-claims context.
	ListForTeamSystem(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error)

	// ReplaceForTeam upserts one row per entry in rules and deletes
	// rows whose project_key is no longer in the input — matches the
	// bulk-replace semantics of the existing config.Save() flow. The
	// whole replace runs inside a single transaction so the table
	// can't observe a partial mid-sync state. Passing an empty rules
	// slice deletes every row for the team. Postgres routes through
	// the app pool (jira_rules_insert / _update / _delete RLS gate
	// writes by team admin).
	ReplaceForTeam(ctx context.Context, teamID string, rules []domain.JiraProjectStatusRules) error
}
