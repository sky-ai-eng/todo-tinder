package domain

import "time"

// OrgSettings is the org-scope settings row (org_settings table).
//
// Field nullability:
//   - GitHubPollInterval / JiraPollInterval / GitHubCloneProtocol ship
//     NOT NULL with defaults; a freshly-inserted row always carries
//     them populated.
//   - GitHubBaseURL / JiraBaseURL / AnthropicAPIKeyRef /
//     BedrockCredentialsRef / MaxLLMModelTier are nullable columns.
//     Empty string round-trips "" ↔ NULL: "not configured yet" (base
//     URLs), "use deployment default" (vault refs), or "no cap" (max
//     tier). Callers never need to distinguish "" from NULL.
//
// GitHubCloneProtocol is "ssh" or "https" only — enforced by a CHECK
// on both backends. An empty string from a caller is treated as
// "leave the default in place" by UpdateSettings (substitutes "ssh"),
// never written to the column.
type OrgSettings struct {
	GitHubBaseURL       string
	GitHubPollInterval  time.Duration
	GitHubCloneProtocol string

	JiraBaseURL      string
	JiraPollInterval time.Duration

	AnthropicAPIKeyRef    string
	BedrockCredentialsRef string
	MaxLLMModelTier       string // "haiku" | "sonnet" | "opus" | ""
}

// TeamSettings is the team-scope settings row (team_settings table).
// JiraProjects holds the team's tracked Jira project keys — the full
// per-project rule rows live in jira_project_status_rules and are
// owned by JiraStatusRulesStore, not this struct. JiraProjects on
// this row is a denormalized fast path for "which projects to poll"
// without joining; the rules table is the source of truth for the
// per-project status semantics.
//
// DefaultModel + AutoDelegateEnabled moved off user_settings in SKY-354:
// the team owns the AI behavior policy, users do not override in v1.
type TeamSettings struct {
	JiraProjects               []string
	AIReprioritizeThreshold    int
	AIPreferenceUpdateInterval int
	DefaultModel               string // "haiku" | "sonnet" | "opus"
	AutoDelegateEnabled        bool
}

// UserSettings is the user-scope settings row (user_settings table).
// Reserved for future per-user prefs (theme, notification destinations,
// swipe sensitivity, onboarding state). Empty for v1 post-SKY-354
// cleanup — the AI model + auto-delegate toggle that used to live here
// moved to TeamSettings. The struct stays so the store API can grow
// fields without a signature change.
type UserSettings struct{}

// JiraProjectStatusRules is one row of jira_project_status_rules —
// the team's status configuration for a single Jira project. Multiple
// rows per team (keyed `(team_id, project_key)`) so two projects on
// the same team can have different workflows. The CHECK constraints
// on the table guarantee any persisted row carries a non-empty pickup
// set + members + canonical for both write-target rules.
type JiraProjectStatusRules struct {
	ProjectKey          string
	PickupMembers       []string
	InProgressMembers   []string
	InProgressCanonical string
	DoneMembers         []string
	DoneCanonical       string
}
