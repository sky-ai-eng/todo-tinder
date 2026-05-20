package sqlite

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// teamsStore is the SQLite impl of db.TeamsStore. Local mode seeds a
// single "default" team for runmode.LocalDefaultOrgID via the v1.11.0
// baseline migration; GetDefaultForOrgSystem returns its id by the
// "oldest row wins" rule.
type teamsStore struct{ q queryer }

func newTeamsStore(q queryer) db.TeamsStore { return &teamsStore{q: q} }

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
