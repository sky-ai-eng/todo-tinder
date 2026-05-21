package config_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestFromContext_LocalDefault locks in the "no scope on context, local
// mode → sentinels" branch. Background workers and tests that don't
// thread a scope through still get a sensible Load.
func TestFromContext_LocalDefault(t *testing.T) {
	if runmode.Current() != runmode.ModeLocal {
		t.Skip("test assumes local mode")
	}
	s := config.FromContext(context.Background())
	if s.OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("OrgID = %q; want LocalDefaultOrgID", s.OrgID)
	}
	if s.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("TeamID = %q; want LocalDefaultTeamID", s.TeamID)
	}
	if s.UserID != runmode.LocalDefaultUserID {
		t.Errorf("UserID = %q; want LocalDefaultUserID", s.UserID)
	}
}

// TestFromContext_RoundTrip pins the WithScope + FromContext pair:
// a scope stashed on the context comes back out unchanged. The HTTP
// middleware path relies on this for per-request scope plumbing.
func TestFromContext_RoundTrip(t *testing.T) {
	want := config.Scope{
		OrgID:  "11111111-1111-1111-1111-111111111111",
		TeamID: "22222222-2222-2222-2222-222222222222",
		UserID: "33333333-3333-3333-3333-333333333333",
	}
	ctx := config.WithScope(context.Background(), want)
	got := config.FromContext(ctx)
	if got != want {
		t.Errorf("FromContext = %+v; want %+v", got, want)
	}
}

// TestLocalScope_MatchesSentinels asserts the helper agrees with the
// runmode constants — drift here would silently misroute local-mode
// Load/Save reads to a non-existent scope.
func TestLocalScope_MatchesSentinels(t *testing.T) {
	s := config.LocalScope()
	if s.OrgID != runmode.LocalDefaultOrgID ||
		s.TeamID != runmode.LocalDefaultTeamID ||
		s.UserID != runmode.LocalDefaultUserID {
		t.Errorf("LocalScope() = %+v; want runmode sentinels", s)
	}
}
