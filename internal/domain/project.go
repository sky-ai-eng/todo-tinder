package domain

import "time"

// Project is the top-level concept that segments work items by *concept*
// rather than by repo. SKY-211 / SKY-215. The Curator is the per-project
// long-lived Claude Code session that owns project context — its session
// id lives on this row. The knowledge base lives on disk at
// `~/.triagefactory/projects/<id>/knowledge-base/*.md`; SummaryMD is the
// distilled version that gets injected into delegated agents' worktrees.
type Project struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	SummaryMD         string    `json:"summary_md,omitempty"`
	SummaryStale      bool      `json:"summary_stale"`
	DesignerSessionID string    `json:"designer_session_id,omitempty"`
	PinnedRepos       []string  `json:"pinned_repos"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}
