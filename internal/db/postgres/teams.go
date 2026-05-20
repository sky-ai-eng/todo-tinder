package postgres

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// teamsStore is the Postgres impl of db.TeamsStore. Routes through
// the admin pool — see the TeamsStore interface comment for the pool-
// split rationale.
type teamsStore struct{ admin queryer }

func newTeamsStore(admin queryer) db.TeamsStore { return &teamsStore{admin: admin} }

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
