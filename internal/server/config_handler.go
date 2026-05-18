package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// configResponse is the FE-facing shape exposed by GET /api/config.
//
// Single consumer + single purpose: AuthGate (SKY-252 D8) reads
// deployment_mode at boot to choose between the local keychain-capture
// flow and the multi-mode cookie auth flow. The call is unauthenticated
// — it has to work before login, hence the pre-auth allowlist mount in
// routes(). Per-user identity (github_username, jira_*) used to live
// here for the predicate editor; that data moved to /api/me, which now
// returns the same fields in both modes.
//
// Don't conflate with /api/settings (user-mutable preferences),
// /api/me (the caller's identity + org list), or /api/team/members
// (mutable team roster).
type configResponse struct {
	DeploymentMode string `json:"deployment_mode"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configResponse{
		DeploymentMode: string(runmode.Current()),
	})
}

// teamMembersResponse is the roster shown to Variant B (multi-person team)
// of the predicate editor. Each row carries display_name + github_username
// + is_current_user so the FE can pre-rank the dropdown and highlight
// "you" among teammates.
type teamMembersResponse struct {
	Members []teamMemberRow `json:"members"`
}

type teamMemberRow struct {
	UserID         string  `json:"user_id"`
	DisplayName    string  `json:"display_name"`
	GitHubUsername *string `json:"github_username"` // null when member hasn't captured identity
	JiraAccountID  *string `json:"jira_account_id"` // null when member hasn't connected Jira
	IsCurrentUser  bool    `json:"is_current_user"`
}

func (s *Server) handleTeamMembers(w http.ResponseWriter, r *http.Request) {
	// Multi mode would query memberships for the session user's active
	// team — gated behind the org-team roster work that hasn't landed
	// yet. Refuse rather than return a synthetic local roster that would
	// mislead the FE's "you" highlighting.
	if runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "/api/team/members is not yet wired for multi mode",
		})
		return
	}

	userID := ClaimsFrom(r.Context()).Subject
	// orgID via OrgIDFrom — the 501 gate above means this only runs
	// in local mode today, where OrgIDFrom returns the local sentinel.
	// Reading via the accessor (rather than referencing the sentinel
	// directly) keeps this consistent with the rest of the handler
	// surface for when the multi-mode path lands.
	orgID := OrgIDFrom(r.Context())
	var username, displayName, jiraAccount string
	_ = s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		username, _ = tx.Users.GetGitHubUsername(r.Context(), userID)
		displayName, _ = tx.Users.GetDisplayName(r.Context(), userID)
		jiraAccount, _, _ = tx.Users.GetJiraIdentity(r.Context(), userID)
		return nil
	})
	var login, jiraID *string
	if username != "" {
		login = &username
	}
	if jiraAccount != "" {
		jiraID = &jiraAccount
	}
	writeJSON(w, http.StatusOK, teamMembersResponse{
		Members: []teamMemberRow{
			{
				UserID:         userID,
				DisplayName:    displayName,
				GitHubUsername: login,
				JiraAccountID:  jiraID,
				IsCurrentUser:  true,
			},
		},
	})
}
