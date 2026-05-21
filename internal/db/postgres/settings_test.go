package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSettingsStores_Postgres runs the shared settings conformance
// suite against the Postgres impl. AdminDB serves both pool slots so
// the conformance suite — which exercises the round-trip contract via
// `...System` reads — bypasses RLS. The RLS gates themselves are pinned
// by the dedicated cross-tenant / admin-vs-member tests below.
func TestSettingsStores_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunSettingsStoresConformance(t, func(t *testing.T) (dbtest.SettingsStores, dbtest.SettingsIDs) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "settings-conf")
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		return dbtest.SettingsStores{
				Orgs:            stores.Orgs,
				Teams:           stores.Teams,
				Users:           stores.Users,
				JiraStatusRules: stores.JiraStatusRules,
			}, dbtest.SettingsIDs{
				OrgID:  orgID,
				TeamID: teamID,
				UserID: userID,
			}
	})
}

// TestOrgsStore_Postgres_GetSettings_IsolatesPerOrg pins the cross-
// tenant guarantee documented in the SKY-356 acceptance criteria:
// a request bound to org A must not be able to read org B's
// org_settings. The app-pool GetSettings runs under JWT claims
// {sub=user, org_id=A}; org_settings_select RLS requires
// org_id = tf.current_org_id() AND user_has_org_access — so the
// query against org B's row trivially returns no rows.
func TestOrgsStore_Postgres_GetSettings_IsolatesPerOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "iso-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "iso-b")

	// Seed a real settings row on orgB so the negative read has
	// something to (fail to) return.
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	if err := stores.Orgs.UpdateSettings(context.Background(), orgB, domain.OrgSettings{
		GitHubBaseURL:       "https://b.example.com",
		GitHubPollInterval:  5 * time.Minute,
		GitHubCloneProtocol: "ssh",
		JiraPollInterval:    5 * time.Minute,
	}); err != nil {
		t.Fatalf("seed orgB settings: %v", err)
	}

	// userA, scoped to orgA, must not see orgB's row. The store
	// returns domain.DefaultOrgSettings() on the underlying
	// sql.ErrNoRows that RLS filtering produces — that's the
	// "no row visible" signal post-SKY-355. The cross-tenant
	// concern is "did userA observe orgB's actual configured
	// state?", so probe against orgB's distinctive seeded
	// BaseURL rather than the (default-or-zero) struct shape.
	// If RLS were broken, got.GitHubBaseURL would equal
	// "https://b.example.com"; under correctly-functioning RLS
	// it stays empty (the default fallback's nullable empty).
	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		got, err := stores.Orgs.GetSettings(context.Background(), orgB)
		if err != nil {
			return err
		}
		if got.GitHubBaseURL == "https://b.example.com" {
			t.Errorf("cross-tenant read leaked orgB's GitHubBaseURL: got %+v (RLS gate broken)", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestOrgsStore_Postgres_UpdateSettings_AdminGated pins the
// admin-vs-member write contract: org_settings_update RLS gates writes
// on tf.user_is_org_admin(). A non-admin member's UPDATE filters every
// row out (RowsAffected=0), and the org_settings_insert WITH CHECK on
// the upsert's INSERT side fails outright with SQLSTATE 42501.
func TestOrgsStore_Postgres_UpdateSettings_AdminGated(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "admin-gate")
	member := pgtest.SeedUser(t, h, "plain-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	// Seed a row as owner so the non-admin update path takes the
	// UPDATE branch (where RLS filters out the row) rather than the
	// INSERT branch (which 42501-errors).
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	if err := stores.Orgs.UpdateSettings(context.Background(), orgID, domain.OrgSettings{
		GitHubBaseURL:       "https://owner-set.example.com",
		GitHubPollInterval:  5 * time.Minute,
		GitHubCloneProtocol: "ssh",
		JiraPollInterval:    5 * time.Minute,
	}); err != nil {
		t.Fatalf("owner seed UpdateSettings: %v", err)
	}

	// Member attempt: INSERT ... ON CONFLICT DO UPDATE always runs the
	// INSERT-side WITH CHECK first, and org_settings_insert gates on
	// tf.user_is_org_admin(). A non-admin trips the gate with a
	// SQLSTATE 42501 RLS violation. The error aborts the tx, so we
	// have to return it (not swallow it) — h.WithUser then rolls back
	// cleanly and surfaces the error to the test for assertion.
	wantPoll := 5 * time.Minute
	memberErr := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		return stores.Orgs.UpdateSettings(context.Background(), orgID, domain.OrgSettings{
			GitHubBaseURL:       "https://member-overwrite.example.com",
			GitHubPollInterval:  9 * time.Minute,
			GitHubCloneProtocol: "ssh",
			JiraPollInterval:    9 * time.Minute,
		})
	})
	if memberErr == nil {
		t.Fatal("member UpdateSettings succeeded; admin gate broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(memberErr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected 42501 RLS error, got %v", memberErr)
	}

	// Owner's row must still be intact.
	got, err := stores.Orgs.GetSettingsSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if got.GitHubBaseURL != "https://owner-set.example.com" {
		t.Errorf("non-admin overwrote org_settings: GitHubBaseURL=%q", got.GitHubBaseURL)
	}
	if got.GitHubPollInterval != wantPoll {
		t.Errorf("non-admin overwrote org_settings: GitHubPollInterval=%v want %v", got.GitHubPollInterval, wantPoll)
	}

	// Owner can update freely — pins the positive side of the gate.
	err = h.WithUser(t, owner, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		return stores.Orgs.UpdateSettings(context.Background(), orgID, domain.OrgSettings{
			GitHubBaseURL:       "https://owner-update.example.com",
			GitHubPollInterval:  7 * time.Minute,
			GitHubCloneProtocol: "ssh",
			JiraPollInterval:    7 * time.Minute,
		})
	})
	if err != nil {
		t.Fatalf("owner UpdateSettings: %v", err)
	}
	got, err = stores.Orgs.GetSettingsSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem post-owner-update: %v", err)
	}
	if got.GitHubBaseURL != "https://owner-update.example.com" {
		t.Errorf("owner update did not land: GitHubBaseURL=%q", got.GitHubBaseURL)
	}
}

// TestJiraStatusRulesStore_Postgres_ReplaceForTeam_TeamAdminGated pins
// the team-admin gate on jira_rules_insert / _update / _delete. A
// plain team member's ReplaceForTeam fails with a 42501 RLS error
// (insert WITH CHECK refuses) and the existing rows survive.
func TestJiraStatusRulesStore_Postgres_ReplaceForTeam_TeamAdminGated(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "rules-admin-gate")
	member := pgtest.SeedUser(t, h, "rules-plain-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	// Owner seeds a baseline rule.
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	seed := []domain.JiraProjectStatusRules{{
		ProjectKey:          "SKY",
		PickupMembers:       []string{"To Do"},
		InProgressMembers:   []string{"In Progress"},
		InProgressCanonical: "In Progress",
		DoneMembers:         []string{"Done"},
		DoneCanonical:       "Done",
	}}
	if err := stores.JiraStatusRules.ReplaceForTeam(context.Background(), teamID, seed); err != nil {
		t.Fatalf("owner seed ReplaceForTeam: %v", err)
	}

	// Plain member's ReplaceForTeam must be refused.
	err := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		return stores.JiraStatusRules.ReplaceForTeam(context.Background(), teamID, []domain.JiraProjectStatusRules{{
			ProjectKey:          "ENG",
			PickupMembers:       []string{"New"},
			InProgressMembers:   []string{"Doing"},
			InProgressCanonical: "Doing",
			DoneMembers:         []string{"Closed"},
			DoneCanonical:       "Closed",
		}})
	})
	if err == nil {
		t.Fatal("member ReplaceForTeam succeeded; admin gate broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("expected 42501 RLS error, got %v", err)
	}

	// Original rule survives.
	got, err := stores.JiraStatusRules.ListForTeamSystem(context.Background(), teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(got) != 1 || got[0].ProjectKey != "SKY" {
		t.Errorf("after refused write, rules=%+v; want one SKY row", got)
	}

	_ = owner // referenced via pgtest.AddOrgMember + RLS context above
}
