package db

import "context"

// TeamsStore owns the teams table — the membership unit inside an
// org. Most resources (tasks, runs, projects, prompts) carry a
// team_id FK; the request-handler sites that synthesize new rows need
// a way to pick the right team for the requesting org. Single-team-
// per-org is the assumption today (org bootstrap creates one
// "default" team — see internal/db/pgtest/seed.go);
// GetDefaultForOrgSystem codifies that without forcing every handler
// to inline the lookup SQL.
//
// # Pool split (Postgres)
//
// GetDefaultForOrgSystem routes through the admin pool. The caller
// has already authenticated the request via the JWT-claims middleware
// and verified the orgID — at this point picking the org's default
// team is a derived lookup keyed by a value we already trust. Running
// it on the admin pool keeps the call uniform between the request-
// handler sites (where the WithTx body around it carries the user's
// claims) and the cmd/exec / curator dispatch paths (where there's no
// JWT-claims context at all).
type TeamsStore interface {
	// GetDefaultForOrgSystem returns the ID of the org's default
	// team — defined as the oldest team row by created_at. The
	// single-team-per-org assumption means there's only one row to
	// pick in practice; the ORDER BY is the tiebreaker for any
	// future fixture that seeds multiple teams (and pins behavior
	// at "the original team wins" rather than non-deterministic).
	//
	// Returns the empty string with a nil error if the org has no
	// teams. Callers treat that as a hard error — every org gets a
	// default team at create time (multi-mode via SKY-257 D14 org
	// provisioning; local mode via the v1.11.0 baseline migration),
	// and a teamless org is a bootstrap bug.
	GetDefaultForOrgSystem(ctx context.Context, orgID string) (string, error)
}
