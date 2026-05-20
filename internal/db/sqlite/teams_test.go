package sqlite_test

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamsStore_SQLite_GetDefaultForOrgSystem_ReturnsSentinelTeam
// pins local-mode behavior: the seeded "default" team for the
// LocalDefaultOrgID sentinel is the resolved row, regardless of how
// many handlers call this lookup. Handler-side sites that previously
// hardcoded runmode.LocalDefaultTeamID now route through this helper.
func TestTeamsStore_SQLite_GetDefaultForOrgSystem_ReturnsSentinelTeam(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("GetDefaultForOrgSystem = %q; want %q (seeded default team)", got, runmode.LocalDefaultTeamID)
	}
}

// TestTeamsStore_SQLite_GetDefaultForOrgSystem_NoTeamsReturnsEmpty
// — an org with zero teams returns ("", nil) rather than an error.
// Callers treat the empty string as a bootstrap bug (handlers in
// projects/stock/factory_delegate surface a 500 rather than mint a
// wrong-team row).
func TestTeamsStore_SQLite_GetDefaultForOrgSystem_NoTeamsReturnsEmpty(t *testing.T) {
	conn := openSQLiteForTest(t)
	if _, err := conn.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		"00000000-0000-0000-0000-000000000200", "teamless", "Teamless",
	); err != nil {
		t.Fatalf("seed extra org: %v", err)
	}
	stores := sqlitestore.New(conn)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), "00000000-0000-0000-0000-000000000200")
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != "" {
		t.Errorf("GetDefaultForOrgSystem on teamless org = %q; want empty string", got)
	}
}

// TestTeamsStore_SQLite_GetDefaultForOrgSystem_OldestWins pins the
// ordering rule: when multiple teams exist for an org, the oldest
// (by created_at) wins. This is the "single-team-per-org" assumption
// safety net — even if a future fixture seeds extra teams, the
// resolved default stays deterministic.
func TestTeamsStore_SQLite_GetDefaultForOrgSystem_OldestWins(t *testing.T) {
	conn := openSQLiteForTest(t)
	if _, err := conn.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		"00000000-0000-0000-0000-000000000200", "multi-team", "Multi-Team",
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO teams (id, org_id, slug, name, created_at)
		VALUES ($1, $2, 'older', 'older', '2026-01-01 00:00:00')
	`, "team-older", "00000000-0000-0000-0000-000000000200"); err != nil {
		t.Fatalf("seed older team: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO teams (id, org_id, slug, name, created_at)
		VALUES ($1, $2, 'newer', 'newer', '2026-06-01 00:00:00')
	`, "team-newer", "00000000-0000-0000-0000-000000000200"); err != nil {
		t.Fatalf("seed newer team: %v", err)
	}
	stores := sqlitestore.New(conn)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), "00000000-0000-0000-0000-000000000200")
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != "team-older" {
		t.Errorf("GetDefaultForOrgSystem = %q; want \"team-older\" (oldest by created_at)", got)
	}
}
