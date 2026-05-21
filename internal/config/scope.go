package config

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Scope names the (org, team, user) the caller is operating as when
// reading or writing settings. Every Load/Save needs one; Scope just
// bundles the three IDs so they can be threaded through request paths
// without growing function signatures any further.
type Scope struct {
	OrgID  string
	TeamID string
	UserID string
}

// Load is a convenience wrapper that forwards s.OrgID / s.TeamID /
// s.UserID to the package-level Load. Call sites that already hold a
// Scope (typically from FromContext or the local-mode helpers below)
// can use it instead of unpacking the struct themselves.
func (s Scope) Load(ctx context.Context) (Config, error) {
	return Load(ctx, s.OrgID, s.TeamID, s.UserID)
}

// Save mirrors Scope.Load. See Save for the validation contract.
func (s Scope) Save(ctx context.Context, cfg Config) error {
	return Save(ctx, s.OrgID, s.TeamID, s.UserID, cfg)
}

// LocalScope returns the synthetic local-mode sentinel Scope. The four
// constants in runmode (one per sentinel row in orgs/teams/users) point
// at the rows the SQLite bootstrap inserts; reads/writes through this
// Scope land in those rows. LoadLocal/SaveLocal call into it.
func LocalScope() Scope {
	return Scope{
		OrgID:  runmode.LocalDefaultOrgID,
		TeamID: runmode.LocalDefaultTeamID,
		UserID: runmode.LocalDefaultUserID,
	}
}

// LoadLocal returns the Config for the local-mode sentinel scope.
// Background workers (poller, scorer, repo profiler) and CLI subcommands
// (cmd/exec, cmd/uninstall) don't have a request context to carry org/
// team/user IDs and use this helper directly. Multi-mode call sites use
// Load with the per-request scope instead.
func LoadLocal() (Config, error) {
	s := LocalScope()
	return Load(context.Background(), s.OrgID, s.TeamID, s.UserID)
}

// SaveLocal mirrors LoadLocal. Same scope, same use cases.
func SaveLocal(cfg Config) error {
	s := LocalScope()
	return Save(context.Background(), s.OrgID, s.TeamID, s.UserID, cfg)
}

// scopeCtxKey is the unexported context key WithScope/FromContext use.
// Separate key type per Go convention so collisions with other packages'
// context values are impossible.
type scopeCtxKey struct{}

// WithScope returns a child context that carries the supplied Scope.
// Request middleware that authenticates a multi-mode caller stashes the
// resolved scope here; handlers downstream read it back via FromContext
// without each one re-deriving it from auth claims + URL path values.
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeCtxKey{}, s)
}

// FromContext returns the Scope carried on ctx, or the local-mode
// sentinel Scope when none is present and the process is running in
// local mode. In multi mode without a scope on the context (e.g. a
// background worker that didn't stash one), the returned Scope is the
// zero value — handlers that require a real org should fail closed
// rather than read from the sentinel rows in a multi-mode deployment.
func FromContext(ctx context.Context) Scope {
	if s, ok := ctx.Value(scopeCtxKey{}).(Scope); ok {
		return s
	}
	if runmode.Current() == runmode.ModeLocal {
		return LocalScope()
	}
	return Scope{}
}
