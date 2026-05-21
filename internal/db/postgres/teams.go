package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamsStore is the Postgres impl of db.TeamsStore. Holds both pools —
// see the TeamsStore interface comment for the pool-split rationale.
//
//   - admin: GetDefaultForOrgSystem, GetSettingsSystem. Boot-time
//     pollers/scorer/delegation spawner without a JWT-claims context.
//   - app: GetSettings, UpdateSettings. Request-handler reads/writes
//     gated by the team_settings_select / team_settings_update RLS
//     policies (team membership / team admin).
type teamsStore struct {
	app   queryer
	admin queryer
}

func newTeamsStore(app, admin queryer) db.TeamsStore {
	return &teamsStore{app: app, admin: admin}
}

var _ db.TeamsStore = (*teamsStore)(nil)

func (s *teamsStore) GetDefaultForOrgSystem(ctx context.Context, orgID string) (string, error) {
	var id string
	err := s.admin.QueryRowContext(ctx, `
		SELECT id::text FROM teams
		WHERE org_id = $1
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`, orgID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *teamsStore) GetSettings(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.app, teamID)
}

func (s *teamsStore) GetSettingsSystem(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.admin, teamID)
}

func getTeamSettings(ctx context.Context, q queryer, teamID string) (domain.TeamSettings, error) {
	var (
		projectsJSON            string
		aiThreshold, aiInterval int
		defaultModel            string
		autoDelegate            bool
	)
	// array_to_json(...)::text round-trips text[] as a JSON literal.
	// database/sql + pgx stdlib doesn't ship a scanner for *[]string,
	// so the JSON detour is the portable shape — matches what the
	// jira_project_status_rules reader does for its array columns.
	err := q.QueryRowContext(ctx, `
		SELECT array_to_json(jira_projects)::text,
		       ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled
		FROM team_settings WHERE team_id = $1
	`, teamID).Scan(
		&projectsJSON, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// See OrgsStore for the rationale. Matches team_settings'
		// schema DEFAULT clauses.
		return domain.DefaultTeamSettings(), nil
	}
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("read team_settings: %w", err)
	}
	projects := []string{}
	if projectsJSON != "" {
		if err := json.Unmarshal([]byte(projectsJSON), &projects); err != nil {
			return domain.TeamSettings{}, fmt.Errorf("unmarshal team_settings.jira_projects: %w", err)
		}
		if projects == nil {
			projects = []string{}
		}
	}
	return domain.TeamSettings{
		JiraProjects:               projects,
		AIReprioritizeThreshold:    aiThreshold,
		AIPreferenceUpdateInterval: aiInterval,
		DefaultModel:               defaultModel,
		AutoDelegateEnabled:        autoDelegate,
	}, nil
}

func (s *teamsStore) UpdateSettings(ctx context.Context, teamID string, u domain.TeamSettings) error {
	projects := u.JiraProjects
	if projects == nil {
		projects = []string{}
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO team_settings (
			team_id, jira_projects, ai_reprioritize_threshold,
			ai_preference_update_interval, default_model, auto_delegate_enabled,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (team_id) DO UPDATE SET
			jira_projects = EXCLUDED.jira_projects,
			ai_reprioritize_threshold = EXCLUDED.ai_reprioritize_threshold,
			ai_preference_update_interval = EXCLUDED.ai_preference_update_interval,
			default_model = EXCLUDED.default_model,
			auto_delegate_enabled = EXCLUDED.auto_delegate_enabled,
			updated_at = now()
	`,
		teamID, projects, u.AIReprioritizeThreshold,
		u.AIPreferenceUpdateInterval, u.DefaultModel, u.AutoDelegateEnabled,
	)
	if err != nil {
		return fmt.Errorf("upsert team_settings: %w", err)
	}
	return nil
}
