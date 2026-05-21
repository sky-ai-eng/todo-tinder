package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSettingsStores_SQLite runs the shared settings conformance suite
// against the SQLite impl (Orgs/Teams/Users/JiraStatusRules together).
// Local mode resolves every method to the runmode.LocalDefault* sentinel
// rows seeded by the v1.11.0 baseline migration; each subtest opens a
// fresh in-memory DB so row state doesn't leak across cases.
func TestSettingsStores_SQLite(t *testing.T) {
	dbtest.RunSettingsStoresConformance(t, func(t *testing.T) (dbtest.SettingsStores, dbtest.SettingsIDs) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		return dbtest.SettingsStores{
				Orgs:            stores.Orgs,
				Teams:           stores.Teams,
				Users:           stores.Users,
				JiraStatusRules: stores.JiraStatusRules,
			}, dbtest.SettingsIDs{
				OrgID:  runmode.LocalDefaultOrgID,
				TeamID: runmode.LocalDefaultTeamID,
				UserID: runmode.LocalDefaultUserID,
			}
	})
}
