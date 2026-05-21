package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamsStore is the SQLite impl of db.TeamsStore. Local mode seeds a
// single "default" team for runmode.LocalDefaultOrgID via the v1.11.0
// baseline migration; GetDefaultForOrgSystem returns its id by the
// "oldest row wins" rule. The settings methods read/write
// team_settings; SQLite stores jira_projects as a JSON text blob
// (no native array type).
//
// teamsStore — SQLite impl. The constructor accepts two queryers for
// signature parity with the Postgres impl; SQLite has one connection
// so both collapse to the same queryer. The `...System` variants
// delegate to their non-System counterparts.
type teamsStore struct{ q queryer }

func newTeamsStore(q, _ queryer) db.TeamsStore { return &teamsStore{q: q} }

var _ db.TeamsStore = (*teamsStore)(nil)

func (s *teamsStore) GetDefaultForOrgSystem(ctx context.Context, orgID string) (string, error) {
	var id string
	err := s.q.QueryRowContext(ctx, `
		SELECT id FROM teams
		WHERE org_id = ?
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`, orgID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *teamsStore) GetSettings(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.q, teamID)
}

func (s *teamsStore) GetSettingsSystem(ctx context.Context, teamID string) (domain.TeamSettings, error) {
	return getTeamSettings(ctx, s.q, teamID)
}

func getTeamSettings(ctx context.Context, q queryer, teamID string) (domain.TeamSettings, error) {
	var (
		projectsJSON            string
		aiThreshold, aiInterval int
		defaultModel            string
		autoDelegate            bool
	)
	err := q.QueryRowContext(ctx, `
		SELECT jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
		       default_model, auto_delegate_enabled
		FROM team_settings WHERE team_id = ?
	`, teamID).Scan(
		&projectsJSON, &aiThreshold, &aiInterval,
		&defaultModel, &autoDelegate,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TeamSettings{}, nil
	}
	if err != nil {
		return domain.TeamSettings{}, fmt.Errorf("read team_settings: %w", err)
	}
	projects := []string{}
	if projectsJSON != "" {
		if err := json.Unmarshal([]byte(projectsJSON), &projects); err != nil {
			return domain.TeamSettings{}, fmt.Errorf("unmarshal team_settings.jira_projects: %w", err)
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
	projectsJSON, err := marshalJSONArray(u.JiraProjects)
	if err != nil {
		return fmt.Errorf("marshal team_settings.jira_projects: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO team_settings (
			team_id, jira_projects, ai_reprioritize_threshold,
			ai_preference_update_interval, default_model, auto_delegate_enabled,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			jira_projects = excluded.jira_projects,
			ai_reprioritize_threshold = excluded.ai_reprioritize_threshold,
			ai_preference_update_interval = excluded.ai_preference_update_interval,
			default_model = excluded.default_model,
			auto_delegate_enabled = excluded.auto_delegate_enabled,
			updated_at = CURRENT_TIMESTAMP
	`,
		teamID, projectsJSON, u.AIReprioritizeThreshold,
		u.AIPreferenceUpdateInterval, u.DefaultModel, u.AutoDelegateEnabled,
	); err != nil {
		return fmt.Errorf("upsert team_settings: %w", err)
	}
	return nil
}
