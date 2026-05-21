package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraStatusRulesStore is the Postgres impl of db.JiraStatusRulesStore.
// Holds both pools — see the JiraStatusRulesStore interface comment
// for the pool-split rationale.
//
//   - admin: ListForTeamSystem. Boot-time pollers/scorer without a
//     JWT-claims context.
//   - app: ListForTeam, ReplaceForTeam. Request-handler reads/writes
//     gated by jira_rules_select / jira_rules_insert /
//     jira_rules_update / jira_rules_delete (team membership / team
//     admin).
type jiraStatusRulesStore struct {
	app   queryer
	admin queryer
}

func newJiraStatusRulesStore(app, admin queryer) db.JiraStatusRulesStore {
	return &jiraStatusRulesStore{app: app, admin: admin}
}

var _ db.JiraStatusRulesStore = (*jiraStatusRulesStore)(nil)

func (s *jiraStatusRulesStore) ListForTeam(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.app, teamID)
}

func (s *jiraStatusRulesStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.admin, teamID)
}

func listJiraStatusRules(ctx context.Context, q queryer, teamID string) ([]domain.JiraProjectStatusRules, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT project_key,
		       pickup_members, in_progress_members, in_progress_canonical,
		       done_members, done_canonical
		FROM jira_project_status_rules
		WHERE team_id = $1
		ORDER BY project_key ASC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read jira_project_status_rules: %w", err)
	}
	defer rows.Close()
	out := []domain.JiraProjectStatusRules{}
	for rows.Next() {
		var (
			r                          domain.JiraProjectStatusRules
			pickup, inProgress, done   []string
			inProgressCanon, doneCanon sql.NullString
		)
		if err := rows.Scan(&r.ProjectKey, &pickup, &inProgress, &inProgressCanon, &done, &doneCanon); err != nil {
			return nil, fmt.Errorf("scan jira_project_status_rules: %w", err)
		}
		r.PickupMembers = orEmptyStrings(pickup)
		r.InProgressMembers = orEmptyStrings(inProgress)
		r.InProgressCanonical = inProgressCanon.String
		r.DoneMembers = orEmptyStrings(done)
		r.DoneCanonical = doneCanon.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *jiraStatusRulesStore) ReplaceForTeam(ctx context.Context, teamID string, rules []domain.JiraProjectStatusRules) error {
	return inTx(ctx, s.app, func(tx queryer) error {
		for _, r := range rules {
			if r.ProjectKey == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jira_project_status_rules (
					team_id, project_key,
					pickup_members, in_progress_members, in_progress_canonical,
					done_members, done_canonical, updated_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, now())
				ON CONFLICT (team_id, project_key) DO UPDATE SET
					pickup_members = EXCLUDED.pickup_members,
					in_progress_members = EXCLUDED.in_progress_members,
					in_progress_canonical = EXCLUDED.in_progress_canonical,
					done_members = EXCLUDED.done_members,
					done_canonical = EXCLUDED.done_canonical,
					updated_at = now()
			`,
				teamID, r.ProjectKey,
				orEmptyStrings(r.PickupMembers),
				orEmptyStrings(r.InProgressMembers), nullString(r.InProgressCanonical),
				orEmptyStrings(r.DoneMembers), nullString(r.DoneCanonical),
			); err != nil {
				return fmt.Errorf("upsert jira_project_status_rules[%s]: %w", r.ProjectKey, err)
			}
		}

		// Prune rows for project keys no longer in the input. NOT IN
		// against an empty list trivially keeps every row, so the
		// "no keys left" case takes the simpler unconditional DELETE.
		keys := projectKeys(rules)
		if len(keys) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM jira_project_status_rules WHERE team_id = $1`,
				teamID,
			); err != nil {
				return fmt.Errorf("clear jira_project_status_rules: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM jira_project_status_rules
			 WHERE team_id = $1 AND project_key <> ALL($2)`,
			teamID, keys,
		); err != nil {
			return fmt.Errorf("prune jira_project_status_rules: %w", err)
		}
		return nil
	})
}

func projectKeys(rules []domain.JiraProjectStatusRules) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		if r.ProjectKey != "" {
			out = append(out, r.ProjectKey)
		}
	}
	return out
}

func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
