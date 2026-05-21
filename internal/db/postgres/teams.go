package postgres

import (
	"context"
	"database/sql"
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
		projects                []string
		aiThreshold, aiInterval int
		defaultModel            string
		autoDelegate            bool
	)
	err := q.QueryRowContext(ctx, `
		SELECT jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled
		FROM team_settings WHERE team_id = $1
	`, teamID).Scan(
		&projects, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TeamSettings{}, nil
	}
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("read team_settings: %w", err)
	}
	if projects == nil {
		projects = []string{}
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
