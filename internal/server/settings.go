package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// jiraProjectKeyRe matches Jira's standard project-key rule: a
// leading uppercase letter followed by uppercase letters or digits.
// Keys arriving through the API are uppercased before matching so
// users typing "sky" land on the same canonical form as Jira's
// wire-side "SKY-123".
var jiraProjectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)

// normalizeJiraProjectKey trims whitespace and uppercases. Used at
// the HTTP boundary in handleSettingsPost (the write path) and in
// validateTrackerKeys (the read/compare path) so lookups match
// regardless of how the user typed the key.
func normalizeJiraProjectKey(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// jiraStatusRule is the wire shape for a single status rule (pickup,
// in-progress, or done). Local to this package now that internal/
// config is gone; mirrors the prior config.JiraStatusRule view.
type jiraStatusRule struct {
	Members   []string `json:"members"`
	Canonical string   `json:"canonical,omitempty"`
}

// jiraProjectConfig is the per-project wire shape for the settings
// handler. Three status rules — pickup, in-progress, done — keyed by
// project_key. Mirrors what the deleted config.JiraProjectConfig
// exposed.
type jiraProjectConfig struct {
	Key        string         `json:"key"`
	Pickup     jiraStatusRule `json:"pickup"`
	InProgress jiraStatusRule `json:"in_progress"`
	Done       jiraStatusRule `json:"done"`
}

// validateProjectRules enforces the per-project invariant that every
// persisted project carries fully-populated Pickup/InProgress/Done
// rules. The jpsr_*_populated CHECK constraints in the baseline are
// the DB-level mirror; this is the user-facing gate that surfaces a
// readable error instead of a constraint violation.
//
// Pickup: members required, canonical must be empty (TF never writes
// to pickup). InProgress/Done: members + canonical required, and the
// canonical must itself be a member (PG CHECK can't subquery, so this
// check stays in Go).
func validateProjectRules(p jiraProjectConfig) error {
	if len(p.Pickup.Members) == 0 {
		return fmt.Errorf("project %s: pickup members are required", p.Key)
	}
	if p.Pickup.Canonical != "" {
		return fmt.Errorf("project %s: pickup canonical must be empty — TF never writes tickets back to pickup", p.Key)
	}
	for _, r := range []struct {
		name string
		rule jiraStatusRule
	}{
		{"in_progress", p.InProgress},
		{"done", p.Done},
	} {
		if len(r.rule.Members) == 0 {
			return fmt.Errorf("project %s: %s members are required", p.Key, r.name)
		}
		if r.rule.Canonical == "" {
			return fmt.Errorf("project %s: %s canonical is required", p.Key, r.name)
		}
		if !slices.Contains(r.rule.Members, r.rule.Canonical) {
			return fmt.Errorf("project %s: %s canonical %q is not in members", p.Key, r.name, r.rule.Canonical)
		}
	}
	return nil
}

// normalizeMembers returns a sorted, deduplicated copy of members so rules can
// be compared using set semantics without mutating the original slice.
func normalizeMembers(members []string) []string {
	normalized := slices.Clone(members)
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

// ruleEqual compares two status rules by value. Used by change detection to
// decide whether a Jira poller restart is needed. Nil-safe on the Members slice.
func ruleEqual(a, b jiraStatusRule) bool {
	return a.Canonical == b.Canonical &&
		slices.Equal(normalizeMembers(a.Members), normalizeMembers(b.Members))
}

// cloneJiraProjects returns a deep copy so the pre-change snapshot
// stays stable while the handler mutates the desired project list.
func cloneJiraProjects(in []jiraProjectConfig) []jiraProjectConfig {
	out := make([]jiraProjectConfig, len(in))
	for i, p := range in {
		out[i] = jiraProjectConfig{
			Key: p.Key,
			Pickup: jiraStatusRule{
				Members:   slices.Clone(p.Pickup.Members),
				Canonical: p.Pickup.Canonical,
			},
			InProgress: jiraStatusRule{
				Members:   slices.Clone(p.InProgress.Members),
				Canonical: p.InProgress.Canonical,
			},
			Done: jiraStatusRule{
				Members:   slices.Clone(p.Done.Members),
				Canonical: p.Done.Canonical,
			},
		}
	}
	return out
}

// jiraProjectsEqual compares two per-project lists by value, treating
// order as significant (the user-facing UI keeps projects in the order
// they were added; reordering counts as a change worth restarting the
// poller for). Rules are compared with set-equality on Members.
func jiraProjectsEqual(a, b []jiraProjectConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
		if !ruleEqual(a[i].Pickup, b[i].Pickup) ||
			!ruleEqual(a[i].InProgress, b[i].InProgress) ||
			!ruleEqual(a[i].Done, b[i].Done) {
			return false
		}
	}
	return true
}

// projectConfigsToRules is the inverse of rulesToProjectConfigsOrdered. Used
// by handleSettingsPost when persisting the team's project list back
// to jira_project_status_rules via JiraStatusRulesStore.ReplaceForTeam.
func projectConfigsToRules(projects []jiraProjectConfig) []domain.JiraProjectStatusRules {
	out := make([]domain.JiraProjectStatusRules, 0, len(projects))
	for _, p := range projects {
		out = append(out, domain.JiraProjectStatusRules{
			ProjectKey:          p.Key,
			PickupMembers:       slices.Clone(p.Pickup.Members),
			InProgressMembers:   slices.Clone(p.InProgress.Members),
			InProgressCanonical: p.InProgress.Canonical,
			DoneMembers:         slices.Clone(p.Done.Members),
			DoneCanonical:       p.Done.Canonical,
		})
	}
	return out
}

// defaultedCloneProtocolView normalizes a stored CloneProtocol value
// for the API surface using the same effective semantics as backend
// clone URL selection: only the literal value "ssh" selects SSH;
// empty, "https", and any other invalid/stale value are treated as
// HTTPS. Clients should always see one of the two known forms.
func defaultedCloneProtocolView(stored string) string {
	if stored == "ssh" {
		return "ssh"
	}
	return "https"
}

// settingsResponse combines settings values with auth status so the
// frontend can render everything on one page.
type settingsResponse struct {
	GitHub githubSettings `json:"github"`
	Jira   jiraSettings   `json:"jira"`
	Server serverSettings `json:"server"`
	AI     aiSettings     `json:"ai"`
}

type githubSettings struct {
	Enabled       bool   `json:"enabled"`
	BaseURL       string `json:"base_url"`
	HasToken      bool   `json:"has_token"`
	PollInterval  string `json:"poll_interval"`
	CloneProtocol string `json:"clone_protocol"` // "ssh" | "https"
}

type jiraSettings struct {
	Enabled      bool                  `json:"enabled"`
	BaseURL      string                `json:"base_url"`
	HasToken     bool                  `json:"has_token"`
	PollInterval string                `json:"poll_interval"`
	Projects     []jiraProjectSettings `json:"projects"`
}

// jiraProjectSettings is the per-project wire shape. Mirrors
// jiraProjectConfig but with explicit empty-slice initialization so the
// JSON response always carries members:[] rather than members:null.
type jiraProjectSettings struct {
	Key        string         `json:"key"`
	Pickup     jiraStatusRule `json:"pickup"`
	InProgress jiraStatusRule `json:"in_progress"`
	Done       jiraStatusRule `json:"done"`
}

type serverSettings struct {
	Port int `json:"port"`
}

type aiSettings struct {
	Model                    string `json:"model"`
	ReprioritizeThreshold    int    `json:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `json:"preference_update_interval"`
	AutoDelegateEnabled      bool   `json:"auto_delegate_enabled"`
}

// loadSettingsView reads org, team, and per-team Jira rules for the
// requesting tenant and folds them into the legacy combined view the
// handler exposes. SKY-357 will split this into per-scope endpoints;
// until then the single-endpoint shape is preserved.
//
// Runs inside the caller's WithTx and uses the app-pool variants
// (GetSettings / ListForTeam) so org_settings_select / team_settings_
// select / jira_rules_select RLS gates fire under the user's claims.
// Returns an error when the org has no default team — that's a
// bootstrap bug elsewhere in the codebase (provisioning always seeds
// one), and silently falling back to zero values would let the
// Settings UI display empty model + 0 thresholds.
func loadSettingsView(ctx context.Context, tx db.TxStores, orgID string) (orgSet domain.OrgSettings, teamSet domain.TeamSettings, projects []jiraProjectConfig, teamID string, err error) {
	orgSet, err = tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return
	}
	teamID, err = tx.Teams.GetDefaultForOrg(ctx, orgID)
	if err != nil {
		return
	}
	if teamID == "" {
		err = fmt.Errorf("org %s has no default team", orgID)
		return
	}
	teamSet, err = tx.Teams.GetSettings(ctx, teamID)
	if err != nil {
		return
	}
	var rules []domain.JiraProjectStatusRules
	rules, err = tx.JiraStatusRules.ListForTeam(ctx, teamID)
	if err != nil {
		return
	}
	// JiraStatusRulesStore.ListForTeam returns rules in project_key
	// ASC; team_settings.jira_projects holds the denormalized ordered
	// list the user actually set in Settings. Reorder the rule slice
	// to match teamSet.JiraProjects so the wire response reflects
	// user-set order and jiraProjectsEqual's change detection
	// doesn't spuriously fire just because the store sorted them.
	projects = rulesToProjectConfigsOrdered(rules, teamSet.JiraProjects)
	return
}

// rulesToProjectConfigsOrdered converts the domain rules into the
// wire shape and reorders them to match order. Keys present in
// order but missing from rules are skipped (the store is the source
// of truth for rule existence); keys in rules but not in order get
// appended after the ordered set in their store-returned (ASC)
// position so a manual DB poke that adds a row without updating
// team_settings.jira_projects still surfaces.
func rulesToProjectConfigsOrdered(rules []domain.JiraProjectStatusRules, order []string) []jiraProjectConfig {
	if len(rules) == 0 {
		return nil
	}
	byKey := make(map[string]domain.JiraProjectStatusRules, len(rules))
	for _, r := range rules {
		byKey[r.ProjectKey] = r
	}
	out := make([]jiraProjectConfig, 0, len(rules))
	seen := make(map[string]bool, len(rules))
	for _, k := range order {
		r, ok := byKey[k]
		if !ok {
			continue
		}
		out = append(out, ruleToProjectConfig(r))
		seen[k] = true
	}
	for _, r := range rules {
		if seen[r.ProjectKey] {
			continue
		}
		out = append(out, ruleToProjectConfig(r))
	}
	return out
}

func ruleToProjectConfig(r domain.JiraProjectStatusRules) jiraProjectConfig {
	return jiraProjectConfig{
		Key: r.ProjectKey,
		Pickup: jiraStatusRule{
			Members: slices.Clone(r.PickupMembers),
		},
		InProgress: jiraStatusRule{
			Members:   slices.Clone(r.InProgressMembers),
			Canonical: r.InProgressCanonical,
		},
		Done: jiraStatusRule{
			Members:   slices.Clone(r.DoneMembers),
			Canonical: r.DoneCanonical,
		},
	}
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	// Wrap the whole read in WithTx so the per-scope reads + the
	// integrations.Load (vault) call go through the app pool under
	// the user's claims. Multi-mode RLS gates org_settings_select /
	// team_settings_select / jira_rules_select on membership, and
	// SecretStore's Postgres impl needs request.jwt.claims set for
	// vault_decrypt — none of those work outside WithTx.
	var (
		orgSet   domain.OrgSettings
		teamSet  domain.TeamSettings
		projects []jiraProjectConfig
		creds    auth.Credentials
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, teamSet, projects, _, lerr = loadSettingsView(r.Context(), tx, orgID)
		if lerr != nil {
			return lerr
		}
		// SecretStore errors are non-fatal — the keychain may be
		// empty on a fresh install. Suppressed here so a missing
		// row doesn't 500 the whole Settings page.
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		return nil
	}); err != nil {
		internalError(w, "settings", err)
		return
	}

	resp := settingsResponse{
		GitHub: githubSettings{
			Enabled:       creds.GitHubPAT != "",
			BaseURL:       creds.GitHubURL,
			HasToken:      creds.GitHubPAT != "",
			PollInterval:  orgSet.GitHubPollInterval.String(),
			CloneProtocol: defaultedCloneProtocolView(orgSet.GitHubCloneProtocol),
		},
		Jira: jiraSettings{
			Enabled:      creds.JiraPAT != "",
			BaseURL:      creds.JiraURL,
			HasToken:     creds.JiraPAT != "",
			PollInterval: orgSet.JiraPollInterval.String(),
			Projects:     toJiraProjectSettings(projects),
		},
		Server: serverSettings{
			// Read once at boot from instance_config.server_port and
			// stashed on the Server struct. The actual bind port wins
			// from --port; this is the stored override the frontend's
			// Settings input renders.
			Port: s.serverPort,
		},
		AI: aiSettings{
			Model:                    teamSet.DefaultModel,
			ReprioritizeThreshold:    teamSet.AIReprioritizeThreshold,
			PreferenceUpdateInterval: teamSet.AIPreferenceUpdateInterval,
			AutoDelegateEnabled:      teamSet.AutoDelegateEnabled,
		},
	}

	if resp.Jira.Projects == nil {
		resp.Jira.Projects = []jiraProjectSettings{}
	}

	writeJSON(w, http.StatusOK, resp)
}

// toJiraProjectSettings converts the persisted view into the wire
// shape, normalizing nil Members slices to empty slices so the JSON
// response is friendly to FE consumers (no `members:null`).
func toJiraProjectSettings(in []jiraProjectConfig) []jiraProjectSettings {
	out := make([]jiraProjectSettings, 0, len(in))
	for _, p := range in {
		out = append(out, jiraProjectSettings{
			Key:        p.Key,
			Pickup:     normalizeRule(p.Pickup),
			InProgress: normalizeRule(p.InProgress),
			Done:       normalizeRule(p.Done),
		})
	}
	return out
}

// normalizeRule replaces a nil Members slice with an empty one so the
// JSON encoding is `[]` rather than `null`. Canonical and other fields
// pass through unchanged.
func normalizeRule(r jiraStatusRule) jiraStatusRule {
	if r.Members == nil {
		r.Members = []string{}
	}
	return r
}

type settingsUpdateRequest struct {
	// Connections — only validate/store if token is non-empty
	GitHubEnabled bool   `json:"github_enabled"`
	GitHubURL     string `json:"github_url"`
	GitHubPAT     string `json:"github_pat"` // empty means "keep existing"
	JiraEnabled   bool   `json:"jira_enabled"`
	JiraURL       string `json:"jira_url"`
	JiraPAT       string `json:"jira_pat"` // empty means "keep existing"

	// Config
	GitHubPollInterval  string `json:"github_poll_interval"`
	GitHubCloneProtocol string `json:"github_clone_protocol"` // "ssh" | "https" | "" (don't touch)
	JiraPollInterval    string `json:"jira_poll_interval"`
	// JiraProjects is a pointer so the request can distinguish "don't
	// touch" (nil) from "wipe to empty" ([]). When non-nil, the slice
	// is the full new project list — each entry carries its own
	// Pickup/InProgress/Done rules. SKY-272 collapsed the previous
	// global rule fields into this per-project shape.
	JiraProjects   *[]jiraProjectSettings `json:"jira_projects,omitempty"`
	AIModel        string                 `json:"ai_model"`
	AIAutoDelegate *bool                  `json:"ai_auto_delegate_enabled"` // pointer to distinguish absent from false
	ServerPort     int                    `json:"server_port"`              // accepted for compat; deployment-scope, not persisted via this handler post-internal/config deletion
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	// Settings POST mutates org-scoped credentials via the SecretStore.
	// Multi-mode requires an active org because the Postgres impl
	// refuses a vault write with an empty org_id claim; local mode
	// always has the sentinel orgID via the withSession shim so this
	// 409 only ever fires for a freshly-signed-in multi-mode user
	// with no membership/active-org row yet. The post-SKY-242 split
	// (user/team/org settings on distinct surfaces) will let the
	// user-scoped fields move to their own handler without this gate.
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	var req settingsUpdateRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// Load existing state — snapshot for change detection. Reads go
	// through the app pool under the user's claims so RLS gates fire
	// (and so the Postgres SecretStore's vault_decrypt sees claims).
	var (
		orgSet   domain.OrgSettings
		teamSet  domain.TeamSettings
		projects []jiraProjectConfig
		teamID   string
		creds    auth.Credentials
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, teamSet, projects, teamID, lerr = loadSettingsView(r.Context(), tx, orgID)
		if lerr != nil {
			return lerr
		}
		// Credential errors are non-fatal — first-time setup arrives
		// with an empty keychain, and we still need to land the rest
		// of the settings POST.
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		return nil
	}); err != nil {
		internalError(w, "settings", err)
		return
	}

	// Track which disconnects hit the env-overlay no-op so we can surface
	// a single user-facing warning in the success response. The FE renders
	// this as a toast/banner — telling the user "Disconnect succeeded but
	// env vars still supply the credential" is the load-bearing UX from
	// the SecretStore sweep.
	envOverlayWarn := []string{}

	// Disconnect flags drive the persist WithTx below — clearing
	// keychain entries happens inside the same tx as the settings
	// writes so a mid-flight failure can't leave the SecretStore
	// emptied while the settings UI still claims the integration is
	// configured.
	var (
		clearGitHubSecrets bool
		clearJiraSecrets   bool
	)

	// Snapshot pre-change values for diffing
	prevGHURL := creds.GitHubURL
	prevGHPAT := creds.GitHubPAT
	prevGHPollInterval := orgSet.GitHubPollInterval
	prevGHCloneProtocol := orgSet.GitHubCloneProtocol
	prevJiraURL := creds.JiraURL
	prevJiraPAT := creds.JiraPAT
	prevJiraProjects := cloneJiraProjects(projects)
	prevJiraPollInterval := orgSet.JiraPollInterval

	// --- Handle GitHub ---
	//
	// The GitHub login lives on users.github_username (not the keychain).
	// This handler writes the column directly when we validate a new PAT
	// or backfill an empty row.
	if req.GitHubEnabled {
		if req.GitHubURL != "" {
			orgSet.GitHubBaseURL = req.GitHubURL
			creds.GitHubURL = req.GitHubURL
		}
		// New token provided — validate it.
		if req.GitHubPAT != "" {
			url := req.GitHubURL
			if url == "" {
				url = creds.GitHubURL
			}
			if url == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub URL is required"})
				return
			}
			ghUser, err := auth.ValidateGitHub(url, req.GitHubPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "GitHub: " + err.Error(),
					"field": "github",
				})
				return
			}
			creds.GitHubPAT = req.GitHubPAT
			// Username persists onto the requesting user's row, identified
			// by the JWT claim's subject (sentinel in local mode via the
			// shim, real UUID in multi mode).
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				return tx.Users.SetGitHubUsername(r.Context(), userID, ghUser.Login)
			}); err != nil {
				log.Printf("[settings] failed to persist users.github_username: %v", err)
			}
		}
		// Backfill username on the users row when we have a PAT but the row
		// is empty (e.g. user saves a PAT for the first time without changing
		// it through the validation branch above). Skip on DB error — a
		// transient read failure shouldn't fan out into a GitHub API call;
		// the next Settings save retries naturally.
		if creds.GitHubPAT != "" {
			var stored string
			storedErr := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				stored, e = tx.Users.GetGitHubUsername(r.Context(), userID)
				return e
			})
			if storedErr != nil {
				log.Printf("[settings] failed to read users.github_username for backfill: %v (skipping backfill this save)", storedErr)
			} else if stored == "" {
				url := creds.GitHubURL
				if url == "" {
					url = orgSet.GitHubBaseURL
				}
				if url != "" {
					if ghUser, err := auth.ValidateGitHub(url, creds.GitHubPAT); err == nil {
						if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
							return tx.Users.SetGitHubUsername(r.Context(), userID, ghUser.Login)
						}); err != nil {
							log.Printf("[settings] failed to backfill users.github_username: %v", err)
						}
					}
				}
			}
		}
	} else {
		// Disabled — clear GitHub credentials, keychain entries, and tracked data
		// Disconnect is a soft gesture — clear credentials and stop polling,
		// but keep entities/tasks/runs/memory intact. Reconnecting the same
		// account resumes where we left off. Full wipe is a separate
		// destructive action (not implemented in v1).
		creds.GitHubURL = ""
		creds.GitHubPAT = ""
		orgSet.GitHubBaseURL = ""
		clearGitHubSecrets = true
		// Env-overlay note: when a TRIAGE_FACTORY_GITHUB_* env var supplies
		// the credential, the SecretStore Delete is a no-op and the value
		// will still surface on the next Get — record it so the response
		// can warn the user. Local-mode only; multi-mode has no env overlay.
		if runmode.Current() == runmode.ModeLocal {
			for _, e := range auth.EnvProvided() {
				if e == "github" {
					envOverlayWarn = append(envOverlayWarn, "github")
					log.Printf("[settings] note: TRIAGE_FACTORY_GITHUB_URL/PAT env vars still supply GitHub credentials — unset them in your shell to fully disconnect")
					break
				}
			}
		}
		// Also clear the captured login on the users row so a downstream
		// "are we connected to GitHub" check via DB stays in sync with the
		// keychain reality (PAT gone → username should be gone too).
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Users.SetGitHubUsername(r.Context(), userID, "")
		}); err != nil {
			log.Printf("[settings] failed to clear users.github_username: %v", err)
		}
	}

	// --- Handle Jira ---
	if req.JiraEnabled {
		if req.JiraURL != "" {
			orgSet.JiraBaseURL = req.JiraURL
			creds.JiraURL = req.JiraURL
		}
		if req.JiraPAT != "" {
			url := req.JiraURL
			if url == "" {
				url = creds.JiraURL
			}
			if url == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira URL is required"})
				return
			}
			jiraUser, err := auth.ValidateJira(url, req.JiraPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "Jira: " + err.Error(),
					"field": "jira",
				})
				return
			}
			creds.JiraPAT = req.JiraPAT
			// SKY-270: Jira identity (account ID + display name) lives on
			// the users row, derived from the same /myself response. Same
			// pattern as the GitHub branch above.
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				return tx.Users.SetJiraIdentity(r.Context(), userID, jiraUser.StableID(), jiraUser.DisplayName)
			}); err != nil {
				log.Printf("[settings] failed to persist users.jira_identity: %v", err)
			}
		}
	} else {
		// Soft disconnect — keep entities/tasks/runs/memory intact.
		creds.JiraURL = ""
		creds.JiraPAT = ""
		orgSet.JiraBaseURL = ""
		clearJiraSecrets = true
		if runmode.Current() == runmode.ModeLocal {
			for _, e := range auth.EnvProvided() {
				if e == "jira" {
					envOverlayWarn = append(envOverlayWarn, "jira")
					log.Printf("[settings] note: TRIAGE_FACTORY_JIRA_URL/PAT env vars still supply Jira credentials — unset them in your shell to fully disconnect")
					break
				}
			}
		}
		// Clear the captured identity on the users row so downstream
		// "are we connected to Jira" checks stay in sync with the
		// keychain reality (PAT gone → identity should be gone too).
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Users.SetJiraIdentity(r.Context(), userID, "", "")
		}); err != nil {
			log.Printf("[settings] failed to clear users.jira_identity: %v", err)
		}
	}

	// --- Update config fields ---
	if req.GitHubPollInterval != "" {
		if d, err := time.ParseDuration(req.GitHubPollInterval); err == nil && d >= 10*time.Second {
			orgSet.GitHubPollInterval = d
		}
	}
	// Empty string means "don't touch" so the toggle UX (which always
	// sends one of "ssh" / "https") flips the value while older clients
	// that omit the field leave it alone. Unrecognized values are
	// rejected rather than silently coerced — the frontend should never
	// send anything other than the two known values.
	if req.GitHubCloneProtocol != "" {
		if req.GitHubCloneProtocol != "ssh" && req.GitHubCloneProtocol != "https" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "github_clone_protocol must be 'ssh' or 'https'"})
			return
		}
		orgSet.GitHubCloneProtocol = req.GitHubCloneProtocol
	}
	if req.JiraPollInterval != "" {
		if d, err := time.ParseDuration(req.JiraPollInterval); err == nil && d >= 10*time.Second {
			orgSet.JiraPollInterval = d
		}
	}
	// JiraProjects carries the full per-project array. Validation runs
	// over each entry's rules before any mutation — one bad rule rejects
	// the whole request so projects never lands in a partial state. Keys
	// are normalized (trim + uppercase) and regex-validated against
	// Jira's own project-key shape; duplicates after normalization are
	// rejected so "SKY" and "sky" can't both land. Rule completeness is
	// enforced by validateProjectRules — partial saves are not a
	// supported state.
	if req.JiraProjects != nil {
		seen := map[string]bool{}
		next := make([]jiraProjectConfig, 0, len(*req.JiraProjects))
		for _, p := range *req.JiraProjects {
			key := normalizeJiraProjectKey(p.Key)
			if key == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "jira_projects: project key must not be empty"})
				return
			}
			if !jiraProjectKeyRe.MatchString(key) {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "jira_projects: invalid project key " + key + " (must match Jira's format: a leading uppercase letter followed by uppercase letters, digits, or underscores)",
				})
				return
			}
			if seen[key] {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "jira_projects: duplicate project key " + key})
				return
			}
			seen[key] = true
			normalized := jiraProjectConfig{
				Key:        key,
				Pickup:     p.Pickup,
				InProgress: p.InProgress,
				Done:       p.Done,
			}
			if err := validateProjectRules(normalized); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			next = append(next, normalized)
		}
		projects = next
	}
	if req.AIModel != "" {
		teamSet.DefaultModel = req.AIModel
	}
	if req.AIAutoDelegate != nil {
		teamSet.AutoDelegateEnabled = *req.AIAutoDelegate
	}
	// req.ServerPort is accepted for backwards compat but ignored:
	// server port is deployment scope (instance_config), read once
	// at boot in main.go and not editable through this handler.
	// SKY-357's settings split makes the boundary explicit; until
	// then we drop the field but surface a warning when the client
	// sent a non-zero value that differs from the stored port, so
	// older frontends or curl users get visible feedback instead of
	// a silent 200 OK.
	var serverPortIgnored bool
	if req.ServerPort > 0 && req.ServerPort != s.serverPort {
		serverPortIgnored = true
	}

	// JiraProjects is also denormalized onto team_settings.jira_projects
	// as the fast path "which projects to poll" without joining.
	teamSet.JiraProjects = projectKeysFromConfigs(projects)

	// Hard-block a transition into SSH mode if our preflight against
	// the configured GitHub host can't authenticate. Otherwise the
	// toggle would "succeed" silently — repairOriginURL is a local
	// settings write that doesn't test connectivity, so the failure
	// wouldn't surface until the next poll/delegation tries to fetch.
	// Only gate the transition (prev != "ssh") so a user with broken
	// SSH today can still save unrelated fields without being held
	// hostage to fix SSH first; switching AWAY from SSH is also
	// unblocked. Probe target is derived from creds.GitHubURL so GHE
	// users see hints with their hostname, not "github.com".
	if orgSet.GitHubCloneProtocol == "ssh" && prevGHCloneProtocol != "ssh" {
		sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := worktree.PreflightSSH(ctx, sshHost)
		cancel()
		if err != nil {
			log.Printf("[settings] blocked SSH switch against %s for %q: preflight failed: %v", sshHost, creds.GitHubURL, err)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  fmt.Sprintf("SSH preflight against %s failed — fix your SSH setup or keep HTTPS. %s", sshHost, err.Error()),
				"field":  "github_clone_protocol",
				"stderr": err.Error(),
			})
			return
		}
	}

	// Persist the credential blob + org/team/jira rule updates +
	// per-integration clears in one WithTx so this block either
	// fully commits or fully rolls back, and so the Postgres vault
	// writes + RLS-gated UPDATEs see request.jwt.claims set.
	//
	// User-row identity writes (SetGitHubUsername / SetJiraIdentity)
	// happen earlier in their own short WithTx calls — they're
	// sequenced with the GitHub/Jira validate-PAT HTTP calls and
	// commit before this final tx runs. That's an intentional
	// looseness: the identity row is auxiliary metadata, and a
	// failure here just leaves a stale username/account-id row that
	// the next save corrects. The atomicity that *matters* —
	// "creds + settings + rules can't half-save" — is what this
	// outer tx enforces.
	//
	// Local mode collapses to a single SQLite tx with the same shape.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Process disconnect clears first so a same-request reconnect
		// (e.g. switch from GitHub PAT A to PAT B by setting
		// github_enabled=false then immediately =true in a separate
		// request would never happen, but a disconnect-and-reapply via
		// integrations.Save below stays valid because Save skips empty
		// values). In practice the two paths are mutually exclusive
		// per integration — disconnect zeros creds, save skips empties.
		if clearGitHubSecrets {
			if err := integrations.ClearGitHub(r.Context(), tx.Secrets, orgID); err != nil {
				return fmt.Errorf("clear GitHub keychain entry: %w", err)
			}
		}
		if clearJiraSecrets {
			if err := integrations.ClearJira(r.Context(), tx.Secrets, orgID); err != nil {
				return fmt.Errorf("clear Jira keychain entry: %w", err)
			}
		}
		if err := integrations.Save(r.Context(), tx.Secrets, orgID, creds); err != nil {
			return fmt.Errorf("store credentials: %w", err)
		}
		if err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		if err := tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		if err := tx.JiraStatusRules.ReplaceForTeam(r.Context(), teamID, projectConfigsToRules(projects)); err != nil {
			return fmt.Errorf("save Jira rules: %w", err)
		}
		return nil
	}); err != nil {
		internalError(w, "settings", err)
		return
	}

	// Detect what changed and fire the appropriate callback. Treat a
	// CloneProtocol flip the same as URL/PAT/PollInterval changes:
	// onGitHubChanged re-runs profiling AND bootstrapBareClones, which
	// is exactly what we need to repair every bare's origin URL to the
	// new form.
	ghChanged := creds.GitHubURL != prevGHURL ||
		creds.GitHubPAT != prevGHPAT ||
		orgSet.GitHubPollInterval != prevGHPollInterval ||
		orgSet.GitHubCloneProtocol != prevGHCloneProtocol

	jiraChanged := creds.JiraURL != prevJiraURL ||
		creds.JiraPAT != prevJiraPAT ||
		!jiraProjectsEqual(projects, prevJiraProjects) ||
		orgSet.JiraPollInterval != prevJiraPollInterval

	// Mark Jira restarted synchronously before launching the async callback so
	// jiraPollReady flips false before this request returns. Otherwise the
	// frontend can race ahead and hit /api/jira/stock while the old state is
	// still reported as ready.
	if ghChanged && s.onGitHubChanged != nil {
		// GitHub change triggers full restart (includes Jira poller restart)
		s.MarkJiraRestarted()
		go s.onGitHubChanged(orgID)
	} else if jiraChanged && s.onJiraChanged != nil {
		// Jira-only change — just restart Jira poller
		s.MarkJiraRestarted()
		go s.onJiraChanged(orgID)
	}

	resp := map[string]any{"status": "saved"}
	if len(envOverlayWarn) > 0 {
		resp["warning"] = "env vars still supply credentials for the disconnected integration(s) — unset them in your shell to fully disconnect"
		resp["env_overlay_blocks_disconnect"] = envOverlayWarn
	}
	if serverPortIgnored {
		resp["server_port_ignored"] = true
		resp["server_port_warning"] = fmt.Sprintf("server_port=%d was accepted but not persisted — port is deployment-scope and stays at %d. Set via --port at boot.", req.ServerPort, s.serverPort)
	}
	writeJSON(w, http.StatusOK, resp)
}

// projectKeysFromConfigs returns the ordered project keys from a
// per-project config slice with empty entries filtered out.
func projectKeysFromConfigs(projects []jiraProjectConfig) []string {
	keys := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.Key != "" {
			keys = append(keys, p.Key)
		}
	}
	return keys
}

// handleJiraConnect validates and stores Jira credentials without saving
// the rest of the settings. This powers the two-stage settings flow: connect
// first, then configure projects and statuses.
//
// Requires an active org because credentials write through the SecretStore
// — see handleSettingsPost for the multi-mode rationale.
func (s *Server) handleJiraConnect(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
		PAT string `json:"pat"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.URL == "" || req.PAT == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url and pat are required"})
		return
	}

	jiraUser, err := auth.ValidateJira(req.URL, req.PAT)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	// One WithTx for the whole read + write window: credentials go
	// through tx.Secrets (Postgres vault writes need claims set),
	// org_settings goes through tx.Orgs (org_settings_update RLS
	// gates on admin), and SKY-270's Jira identity write goes through
	// tx.Users. All-or-nothing so creds + settings + identity can't
	// land in a partial state. The earlier "manual rollback via
	// ClearJira" pattern collapses to plain tx rollback semantics.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, err := integrations.Load(r.Context(), tx.Secrets, orgID)
		if err != nil {
			return fmt.Errorf("load credentials: %w", err)
		}
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		creds.JiraURL = req.URL
		creds.JiraPAT = req.PAT
		orgSet.JiraBaseURL = req.URL
		if err := integrations.Save(r.Context(), tx.Secrets, orgID, creds); err != nil {
			return fmt.Errorf("store credentials: %w", err)
		}
		if err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		// SKY-270: persist the captured Jira identity on the users
		// row. Bundled in the same tx so the connect either fully
		// lands (creds + URL + identity) or fully rolls back.
		if err := tx.Users.SetJiraIdentity(r.Context(), userID, jiraUser.StableID(), jiraUser.DisplayName); err != nil {
			return fmt.Errorf("persist users.jira_identity: %w", err)
		}
		return nil
	}); err != nil {
		// Log the underlying wrap-chain (SQL / vault / FK errors) for
		// operator debugging, but return a stable user-facing message
		// so we don't leak Postgres internals to API clients.
		log.Printf("[settings] handleJiraConnect persist failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to connect Jira"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":       "connected",
		"display_name": jiraUser.DisplayName,
	})
}

// handleJiraStatuses returns available statuses for given Jira projects.
// Query params: ?project=PROJ1&project=PROJ2 (or uses configured projects if omitted).
func (s *Server) handleJiraStatuses(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	projects := r.URL.Query()["project"]
	// Read creds + (if needed) the team's tracked-projects fallback
	// through the app pool inside WithTx so vault_decrypt and
	// team_settings_select run under the user's claims.
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		if len(projects) > 0 {
			return nil
		}
		teamID, e := tx.Teams.GetDefaultForOrg(r.Context(), orgID)
		if e != nil || teamID == "" {
			return nil
		}
		teamSet, te := tx.Teams.GetSettings(r.Context(), teamID)
		if te != nil {
			return nil
		}
		projects = append(projects, teamSet.JiraProjects...)
		return nil
	}); err != nil {
		internalError(w, "settings", err)
		return
	}
	if creds.JiraPAT == "" || creds.JiraURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}
	if len(projects) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no projects specified"})
		return
	}

	client := jira.NewClient(creds.JiraURL, creds.JiraPAT)

	// Intersect statuses across all projects — only return statuses that
	// exist in every project. A union would let users pick a status that
	// fails on some projects (TransitionTo can't find the transition).
	var counts map[string]int            // status name → number of projects it appears in
	var canonical map[string]jira.Status // status name → first-seen Status object
	for i, proj := range projects {
		projectStatuses, err := client.ProjectStatuses(proj)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch statuses for " + proj + ": " + err.Error()})
			return
		}
		if i == 0 {
			counts = make(map[string]int, len(projectStatuses))
			canonical = make(map[string]jira.Status, len(projectStatuses))
		}
		seen := map[string]bool{}
		for _, st := range projectStatuses {
			if !seen[st.Name] {
				seen[st.Name] = true
				counts[st.Name]++
				if _, ok := canonical[st.Name]; !ok {
					canonical[st.Name] = st
				}
			}
		}
	}

	var statuses []jira.Status
	for name, count := range counts {
		if count == len(projects) {
			statuses = append(statuses, canonical[name])
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Name < statuses[j].Name
	})

	writeJSON(w, http.StatusOK, statuses)
}

// handleGitHubPreflightSSH probes whether the user's machine can
// authenticate to GitHub over SSH (key + agent + known_hosts all
// usable). Powers the Setup wizard's gating UX and the Settings
// page's "Test SSH connection" button. Always returns HTTP 200 — the
// body's "ok" flag is the verdict — so the client can distinguish
// "preflight reported failure" from "the server itself errored".
//
// Logs both the success path and the failure stderr to the daemon's
// log so users investigating issues see the exact ssh output even
// when the UI only renders the friendly summary.
func (s *Server) handleGitHubPreflightSSH(w http.ResponseWriter, r *http.Request) {
	// Probe target tracks the configured GitHub base URL so the Test
	// SSH button on the Settings page works for GHE deployments. We
	// load creds (not settings) because creds.GitHubURL is the URL the
	// user actually authenticates against; org_settings.github_base_url
	// mirrors it but the SecretStore copy is the source of truth.
	// Wrapped in WithTx so vault_decrypt sees claims and the read
	// matches the rest of the post-SKY-355 settings surface.
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		return nil
	}); err != nil {
		internalError(w, "settings", err)
		return
	}
	sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := worktree.PreflightSSH(ctx, sshHost); err != nil {
		log.Printf("[settings] SSH preflight against %s failed: %v", sshHost, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"stderr": err.Error(),
			"host":   sshHost,
		})
		return
	}
	log.Printf("[settings] SSH preflight ok (%s authenticated)", sshHost)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": sshHost})
}
