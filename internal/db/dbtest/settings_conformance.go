package dbtest

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SettingsStoresFactory hands the conformance suite a wired bundle of
// settings stores + the IDs (orgID, teamID, userID) to address every
// method against. Per-backend tests own row seeding (orgs / teams /
// users / org_memberships / memberships) — the conformance harness is
// schema-blind.
type SettingsStoresFactory func(t *testing.T) (stores SettingsStores, ids SettingsIDs)

// SettingsStores is the slice of stores the conformance suite exercises.
type SettingsStores struct {
	Orgs            db.OrgsStore
	Teams           db.TeamsStore
	Users           db.UsersStore
	JiraStatusRules db.JiraStatusRulesStore
}

// SettingsIDs are the tenancy keys the factory pre-seeded.
type SettingsIDs struct {
	OrgID  string
	TeamID string
	UserID string
}

// RunSettingsStoresConformance is the shared assertion suite. It covers:
//   - Empty-row reads return zero-value structs (and nil errors), so
//     callers can treat "no row yet" identically to "default values".
//   - Round-trip every field on OrgSettings/TeamSettings via the
//     `...System` reader (admin pool — bypasses RLS, isolates the
//     conformance from RLS test coverage which lives in the
//     per-backend test files).
//   - GitHubCloneProtocol defaulting: "" upserts as "ssh" (the column
//     CHECK rejects empty strings on both backends).
//   - JiraStatusRulesStore.ReplaceForTeam bulk-replace semantics:
//     upsert wins on conflict, missing project keys get pruned.
//   - UserSettings round-trip is a no-op probe (v1 has no user fields)
//     that still upserts a row so future tests can assert presence.
func RunSettingsStoresConformance(t *testing.T, factory SettingsStoresFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("OrgSettings_RoundTripsEveryField", func(t *testing.T) {
		stores, ids := factory(t)
		want := domain.OrgSettings{
			GitHubBaseURL:         "https://ghe.example.com",
			GitHubPollInterval:    7 * time.Minute,
			GitHubCloneProtocol:   "https",
			JiraBaseURL:           "https://acme.atlassian.net",
			JiraPollInterval:      3 * time.Minute,
			AnthropicAPIKeyRef:    "vault://orgs/A/anthropic",
			BedrockCredentialsRef: "vault://orgs/A/bedrock",
			MaxLLMModelTier:       "sonnet",
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, want); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("OrgSettings round-trip mismatch\n got: %+v\nwant: %+v", got, want)
		}
	})

	t.Run("OrgSettings_EmptyRow_ReturnsZeroValue", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem on empty row: %v", err)
		}
		var zero domain.OrgSettings
		if !reflect.DeepEqual(got, zero) {
			t.Errorf("GetSettingsSystem on empty row = %+v; want zero value", got)
		}
	})

	t.Run("OrgSettings_EmptyCloneProtocol_DefaultsToSSH", func(t *testing.T) {
		// The github_clone_protocol column CHECK rejects empty string —
		// UpdateSettings substitutes "ssh" so a fresh-mutate caller
		// doesn't have to know the column constraint.
		stores, ids := factory(t)
		in := domain.OrgSettings{
			GitHubPollInterval: 5 * time.Minute,
			JiraPollInterval:   5 * time.Minute,
			// GitHubCloneProtocol intentionally empty.
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.GitHubCloneProtocol != "ssh" {
			t.Errorf("GitHubCloneProtocol=%q; want \"ssh\" (default substitution)", got.GitHubCloneProtocol)
		}
	})

	t.Run("OrgSettings_NullableFields_RoundTripEmpty", func(t *testing.T) {
		// AnthropicAPIKeyRef / BedrockCredentialsRef / MaxLLMModelTier /
		// GitHubBaseURL / JiraBaseURL: empty input writes NULL, scans
		// back as "". Pins the "" ↔ NULL contract.
		stores, ids := factory(t)
		in := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.GitHubBaseURL != "" || got.JiraBaseURL != "" ||
			got.AnthropicAPIKeyRef != "" || got.BedrockCredentialsRef != "" ||
			got.MaxLLMModelTier != "" {
			t.Errorf("nullable empties did not round-trip: %+v", got)
		}
	})

	t.Run("OrgSettings_Update_Overwrites", func(t *testing.T) {
		stores, ids := factory(t)
		first := domain.OrgSettings{
			GitHubBaseURL:       "https://first.example.com",
			GitHubPollInterval:  5 * time.Minute,
			GitHubCloneProtocol: "ssh",
			JiraPollInterval:    5 * time.Minute,
			MaxLLMModelTier:     "haiku",
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, first); err != nil {
			t.Fatalf("first UpdateSettings: %v", err)
		}
		second := first
		second.GitHubBaseURL = "https://second.example.com"
		second.MaxLLMModelTier = "opus"
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, second); err != nil {
			t.Fatalf("second UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if !reflect.DeepEqual(got, second) {
			t.Errorf("after re-Update, GetSettings = %+v; want %+v", got, second)
		}
	})

	t.Run("TeamSettings_RoundTripsEveryField", func(t *testing.T) {
		stores, ids := factory(t)
		want := domain.TeamSettings{
			JiraProjects:               []string{"SKY", "ENG", "OPS"},
			AIReprioritizeThreshold:    7,
			AIPreferenceUpdateInterval: 30,
			DefaultModel:               "opus",
			AutoDelegateEnabled:        true,
		}
		if err := stores.Teams.UpdateSettings(ctx, ids.TeamID, want); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("TeamSettings round-trip mismatch\n got: %+v\nwant: %+v", got, want)
		}
	})

	t.Run("TeamSettings_EmptyRow_ReturnsZeroValue", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem on empty row: %v", err)
		}
		var zero domain.TeamSettings
		if !reflect.DeepEqual(got, zero) {
			t.Errorf("GetSettingsSystem on empty row = %+v; want zero value", got)
		}
	})

	t.Run("TeamSettings_NilProjects_RoundTripsEmpty", func(t *testing.T) {
		// JiraProjects=nil writes []; reads back as []. Keeps "no
		// projects configured" stable for downstream callers.
		stores, ids := factory(t)
		in := domain.TeamSettings{
			AIReprioritizeThreshold:    5,
			AIPreferenceUpdateInterval: 20,
			DefaultModel:               "sonnet",
		}
		if err := stores.Teams.UpdateSettings(ctx, ids.TeamID, in); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if len(got.JiraProjects) != 0 {
			t.Errorf("JiraProjects=%v; want empty slice", got.JiraProjects)
		}
	})

	t.Run("UserSettings_TouchAndRead", func(t *testing.T) {
		// v1 user_settings has no fields — call exercises the upsert
		// path + read-back contract so future per-user prefs land on
		// established wiring.
		stores, ids := factory(t)
		if err := stores.Users.UpdateSettings(ctx, ids.UserID, domain.UserSettings{}); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Users.GetSettings(ctx, ids.UserID)
		if err != nil {
			t.Fatalf("GetSettings: %v", err)
		}
		var zero domain.UserSettings
		if !reflect.DeepEqual(got, zero) {
			t.Errorf("UserSettings = %+v; want zero value", got)
		}
	})

	t.Run("UserSettings_AbsentRow_ReturnsZeroValue", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.Users.GetSettings(ctx, ids.UserID)
		if err != nil {
			t.Fatalf("GetSettings on absent row: %v", err)
		}
		var zero domain.UserSettings
		if !reflect.DeepEqual(got, zero) {
			t.Errorf("GetSettings on absent row = %+v; want zero value", got)
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_UpsertsRows", func(t *testing.T) {
		stores, ids := factory(t)
		input := []domain.JiraProjectStatusRules{
			{
				ProjectKey:          "SKY",
				PickupMembers:       []string{"To Do", "Backlog"},
				InProgressMembers:   []string{"In Progress"},
				InProgressCanonical: "In Progress",
				DoneMembers:         []string{"Done"},
				DoneCanonical:       "Done",
			},
			{
				ProjectKey:          "ENG",
				PickupMembers:       []string{"Open"},
				InProgressMembers:   []string{"Doing"},
				InProgressCanonical: "Doing",
				DoneMembers:         []string{"Closed"},
				DoneCanonical:       "Closed",
			},
		}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, input); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		sortRulesByKey(got)
		sortRulesByKey(input)
		if !reflect.DeepEqual(got, input) {
			t.Errorf("after ReplaceForTeam, ListForTeam = %+v; want %+v", got, input)
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_PrunesMissingKeys", func(t *testing.T) {
		// First insert two rules, then re-call with just one — the
		// missing project_key row must be deleted.
		stores, ids := factory(t)
		two := []domain.JiraProjectStatusRules{
			{
				ProjectKey: "SKY", PickupMembers: []string{"To Do"},
				InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
				DoneMembers: []string{"Done"}, DoneCanonical: "Done",
			},
			{
				ProjectKey: "ENG", PickupMembers: []string{"Open"},
				InProgressMembers: []string{"Doing"}, InProgressCanonical: "Doing",
				DoneMembers: []string{"Closed"}, DoneCanonical: "Closed",
			},
		}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, two); err != nil {
			t.Fatalf("seed ReplaceForTeam: %v", err)
		}
		one := two[:1]
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, one); err != nil {
			t.Fatalf("prune ReplaceForTeam: %v", err)
		}
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 || got[0].ProjectKey != "SKY" {
			t.Errorf("after prune ReplaceForTeam, got=%+v; want one row keyed SKY", got)
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_EmptyClearsAll", func(t *testing.T) {
		stores, ids := factory(t)
		seed := []domain.JiraProjectStatusRules{{
			ProjectKey: "SKY", PickupMembers: []string{"To Do"},
			InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
			DoneMembers: []string{"Done"}, DoneCanonical: "Done",
		}}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, seed); err != nil {
			t.Fatalf("seed ReplaceForTeam: %v", err)
		}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, nil); err != nil {
			t.Fatalf("empty ReplaceForTeam: %v", err)
		}
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("after empty ReplaceForTeam, got=%+v; want empty slice", got)
		}
	})

	t.Run("JiraStatusRules_EmptyTeam_ReturnsEmptySlice", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("ListForTeamSystem on empty team = %+v; want empty slice", got)
		}
	})
}

func sortRulesByKey(rules []domain.JiraProjectStatusRules) {
	sort.Slice(rules, func(i, j int) bool { return rules[i].ProjectKey < rules[j].ProjectKey })
}
