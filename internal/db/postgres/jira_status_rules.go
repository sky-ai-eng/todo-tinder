package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// array_to_json(...)::text round-trips text[] as a JSON literal.
	// database/sql + pgx stdlib doesn't ship a scanner for *[]string,
	// so the JSON detour is the portable shape.
	rows, err := q.QueryContext(ctx, `
		SELECT project_key,
		       array_to_json(pickup_members)::text,
		       array_to_json(in_progress_members)::text,
		       in_progress_canonical,
		       array_to_json(done_members)::text,
		       done_canonical
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
			r                                    domain.JiraProjectStatusRules
			pickupJSON, inProgressJSON, doneJSON string
			inProgressCanon, doneCanon           sql.NullString
		)
		if err := rows.Scan(&r.ProjectKey, &pickupJSON, &inProgressJSON, &inProgressCanon, &doneJSON, &doneCanon); err != nil {
			return nil, fmt.Errorf("scan jira_project_status_rules: %w", err)
		}
		pickup, err := unmarshalJSONStringSlice(pickupJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal pickup_members for %s: %w", r.ProjectKey, err)
		}
		inProgress, err := unmarshalJSONStringSlice(inProgressJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal in_progress_members for %s: %w", r.ProjectKey, err)
		}
		done, err := unmarshalJSONStringSlice(doneJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal done_members for %s: %w", r.ProjectKey, err)
		}
		r.PickupMembers = pickup
		r.InProgressMembers = inProgress
		r.InProgressCanonical = inProgressCanon.String
		r.DoneMembers = done
		r.DoneCanonical = doneCanon.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func unmarshalJSONStringSlice(s string) ([]string, error) {
	if s == "" {
		return []string{}, nil
	}
	out := []string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func (s *jiraStatusRulesStore) ReplaceForTeam(ctx context.Context, teamID string, rules []domain.JiraProjectStatusRules) error {
	// Refuse empty ProjectKey up front. Silently skipping the row in
	// the upsert loop would also drop the key from the prune list, so
	// a slice of all-empty entries would clear the team — sharply
	// different behavior from "rules is nil/empty, clear all" that the
	// caller almost certainly didn't ask for. Clear-all semantics
	// stay exclusively gated on rules being nil/empty.
	for i, r := range rules {
		if r.ProjectKey == "" {
			return fmt.Errorf("ReplaceForTeam: rules[%d] has empty ProjectKey", i)
		}
	}
	return inTx(ctx, s.app, func(tx queryer) error {
		for _, r := range rules {
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
		// "rules is nil/empty, clear all" case takes the simpler
		// unconditional DELETE. Empty ProjectKey entries can't reach
		// here — the pre-loop validation refuses them.
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
		out = append(out, r.ProjectKey)
	}
	return out
}

func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
