package db

import (
	"database/sql"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// UpsertRepoProfile inserts or updates a repo profile.
// On conflict it updates all metadata fields while preserving the row identity.
func UpsertRepoProfile(database *sql.DB, p domain.RepoProfile) error {
	_, err := database.Exec(`
		INSERT INTO repo_profiles (id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, profiled_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			description  = excluded.description,
			has_readme   = excluded.has_readme,
			has_claude_md = excluded.has_claude_md,
			has_agents_md = excluded.has_agents_md,
			profile_text = excluded.profile_text,
			profiled_at  = excluded.profiled_at,
			updated_at   = datetime('now')
	`,
		p.ID, p.Owner, p.Repo,
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		p.ProfiledAt,
	)
	return err
}
