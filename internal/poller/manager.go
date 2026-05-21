package poller

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
)

// Manager manages the lifecycle of polling loops, allowing them to be
// stopped and restarted when credentials or config change.
type Manager struct {
	database *sql.DB
	bus      *eventbus.Bus
	// tracker dependencies are held instead of a single Tracker instance
	// because each poll cycle constructs one Tracker per active org —
	// orgID is a per-tracker construction parameter, not a per-call
	// argument. See the per-org loops in runGitHubCycle / runJiraCycle.
	tasks     db.TaskStore
	entities  db.EntityStore
	users     db.UsersStore           // source of the session user's github_username
	repos     db.RepoStore            // configured-repo names for GitHub poller startup
	orgs      db.OrgsStore            // enumerate active orgs at each poll tick + per-org settings (GitHub/Jira base URLs, poll intervals)
	teams     db.TeamsStore           // resolve each org's default team for per-team Jira project rules
	jiraRules db.JiraStatusRulesStore // per-team Jira project rules (replaces deleted config.Jira.Projects)
	secrets   db.SecretStore          // integration creds via SecretStore (keychain in local, vault in multi)

	// OnError fires when a poll cycle returns an error. Source is "github"
	// or "jira"; orgID identifies the tenant whose cycle errored (empty
	// when the failure is upstream of the per-org loop, e.g. listing
	// active orgs itself). Wired from main to a toast helper so users
	// see the failure without log-diving; nil-safe if caller doesn't
	// set it.
	OnError func(source, orgID string, err error)

	mu       sync.Mutex
	ghStop   chan struct{}
	jiraStop chan struct{}
}

func NewManager(database *sql.DB, bus *eventbus.Bus, users db.UsersStore, tasks db.TaskStore, entities db.EntityStore, repos db.RepoStore, orgs db.OrgsStore, teams db.TeamsStore, jiraRules db.JiraStatusRulesStore, secrets db.SecretStore) *Manager {
	return &Manager{
		database:  database,
		bus:       bus,
		tasks:     tasks,
		entities:  entities,
		users:     users,
		repos:     repos,
		orgs:      orgs,
		teams:     teams,
		jiraRules: jiraRules,
		secrets:   secrets,
	}
}

// trackerForOrg builds a Tracker bound to the given tenant. Called
// inside the per-org loops of runGitHubCycle / runJiraCycle so each
// tracker emits events stamped with the correct OrgID and reads/
// writes entities scoped to that tenant. Construction is cheap
// (struct of method holders + store references) so per-cycle
// allocation is fine.
func (m *Manager) trackerForOrg(orgID string) *tracker.Tracker {
	return tracker.New(m.database, m.bus, m.tasks, m.entities, orgID)
}

// reportError invokes the OnError callback if set. Centralized so adding
// behavior later (metrics, rate-limiting) has one call site. orgID
// scopes the failure to a tenant; pass empty when the failure is
// process-level (e.g. the cycle's initial ListActiveSystem itself
// errored before any per-org work began).
func (m *Manager) reportError(source, orgID string, err error) {
	if m.OnError != nil {
		m.OnError(source, orgID, err)
	}
}

// RestartAll stops all polling loops and restarts any that are fully
// configured. orgID identifies the tenant whose credentials drive the
// restart — in local mode that's runmode.LocalDefaultOrgID; in multi
// mode this signature lets a future per-org Manager loop call Restart
// per active org. The poller cycles themselves still iterate active
// orgs internally for the per-org tracker dispatch — orgID here is
// the credential-resolution scope (whose PAT do we boot the client
// with), not the polling scope.
func (m *Manager) RestartAll(ctx context.Context, orgID string) {
	m.stopAll()

	orgSet, _ := m.orgs.GetSettingsSystem(ctx, orgID)
	creds, _ := integrations.Load(ctx, m.secrets, orgID)

	m.startGitHub(orgSet, creds)
	// Jira polling resolves per-org settings/creds/rules inside each
	// cycle, so the only thing startJira needs from the trigger org
	// is the tick interval — orgSet.JiraPollInterval is the process-
	// global cadence (per-org poll cadence is future work).
	m.startJira(orgSet.JiraPollInterval)
}

// RestartGitHub stops and restarts only the GitHub polling loop.
func (m *Manager) RestartGitHub(ctx context.Context, orgID string) {
	m.mu.Lock()
	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	m.mu.Unlock()

	orgSet, _ := m.orgs.GetSettingsSystem(ctx, orgID)
	creds, _ := integrations.Load(ctx, m.secrets, orgID)
	m.startGitHub(orgSet, creds)
}

// RestartJira stops and restarts only the Jira polling loop.
func (m *Manager) RestartJira(ctx context.Context, orgID string) {
	m.mu.Lock()
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
	m.mu.Unlock()

	orgSet, _ := m.orgs.GetSettingsSystem(ctx, orgID)
	m.startJira(orgSet.JiraPollInterval)
}

// loadJiraRules pulls the per-team Jira status rules for the org's
// default team. Local mode collapses to N=1 (the synthetic sentinel
// team); multi-mode per-org Jira project configuration is a future
// concern that follows the same per-team grain. Empty list on error.
func (m *Manager) loadJiraRules(ctx context.Context, orgID string) []domain.JiraProjectStatusRules {
	if m.teams == nil || m.jiraRules == nil {
		return nil
	}
	teamID, err := m.teams.GetDefaultForOrgSystem(ctx, orgID)
	if err != nil || teamID == "" {
		if err != nil {
			log.Printf("[poller] org %s: resolve default team: %v", orgID, err)
		}
		return nil
	}
	rules, err := m.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		log.Printf("[poller] org %s team %s: list jira rules: %v", orgID, teamID, err)
		return nil
	}
	return rules
}

// StopAll stops all running polling loops without restarting.
func (m *Manager) StopAll() {
	m.stopAll()
}

// Restart is a convenience alias for RestartAll.
func (m *Manager) Restart(ctx context.Context, orgID string) {
	m.RestartAll(ctx, orgID)
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
}

// startGitHub launches the GitHub tracking loop. Each tick iterates
// active orgs and dispatches a per-org RefreshGitHub. Per-org repo
// lists and per-org user identities are resolved inside the loop so a
// new org added between ticks picks up on the next cycle without a
// poller restart. Local mode collapses to N=1 (the synthetic sentinel
// org). Bounded per-org concurrency is a future optimization —
// sequential is fine given the poll period (≥1 minute baseline).
func (m *Manager) startGitHub(orgSet domain.OrgSettings, creds auth.Credentials) {
	// The GitHub poll loop reads a single users.github_username
	// keyed by the local synthetic user — in multi mode that row
	// has no FK target and every per-org iteration would silently
	// skip. Per-org GitHub App polling (D11) is the multi-mode
	// path; until then, this loop has no useful work to do outside
	// local mode. Gating at the outer Start boundary keeps the
	// no-op loop out of multi-mode logs entirely.
	if runmode.Current() != runmode.ModeLocal {
		return
	}
	if creds.GitHubPAT == "" || creds.GitHubURL == "" {
		log.Println("[github] credentials not configured, skipping tracker")
		return
	}

	interval := orgSet.GitHubPollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}

	client := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
	stop := make(chan struct{})

	m.mu.Lock()
	m.ghStop = stop
	m.mu.Unlock()

	// Resolve the user's team memberships once per GitHub start. Teams
	// rarely change mid-session, and every GitHub config change (creds,
	// repos) already triggers a RestartGitHub which re-enters this path —
	// so picking up new memberships is a question of when the user next
	// reconnects, not of refresh cadence. An empty list on failure means
	// team-based review requests won't surface until next restart; that's
	// a degraded-but-honest state and the error is logged.
	//
	// Team resolution stays out of the per-org loop because the
	// credential set (cfg.GitHub PAT) is process-global today —
	// per-org credential resolution is deferred (out of D9c scope).
	userTeams, err := client.ListMyTeams()
	if err != nil {
		log.Printf("[github] failed to list teams: %v (team-based review requests will be missed until next restart)", err)
		userTeams = nil
	}

	go func() {
		// Initial poll
		m.runGitHubCycle(client, userTeams)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runGitHubCycle(client, userTeams)
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[github] tracker started (interval: %s, teams: %d)", interval, len(userTeams))
}

// runGitHubCycle enumerates active orgs and dispatches a per-org
// RefreshGitHub. Per-org failures are logged and reported via
// OnError but do not abort the remaining orgs in the cycle — a
// transient failure on org A shouldn't starve orgs B..N of polls.
func (m *Manager) runGitHubCycle(client *ghclient.Client, userTeams []string) {
	ctx := context.Background()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[github] list active orgs: %v", err)
		m.reportError("github", "", err)
		return
	}
	for _, orgID := range orgIDs {
		repos, err := m.repos.ListConfiguredNamesSystem(ctx, orgID)
		if err != nil {
			log.Printf("[github] org %s: load configured repos: %v", orgID, err)
			continue
		}
		if len(repos) == 0 {
			continue
		}
		// NULL/empty github_username means identity hasn't been
		// captured yet (fresh install before first Settings save)
		// — skip this org without surfacing as an error.
		//
		// Local-mode bridge: the poller acts as the lone local user,
		// so we read their github_username to drive predicates like
		// "PR review requested from me". This whole loop is gated
		// to local mode at startGitHub, so the LocalDefaultUserID
		// sentinel here resolves to that one user — multi-mode
		// per-org GitHub App polling (D11) will replace the
		// username read with a GitHubClientFor(ctx, orgID) call
		// against the App's installation identity.
		username, err := m.users.GetGitHubUsernameSystem(ctx, runmode.LocalDefaultUserID)
		if err != nil {
			log.Printf("[github] org %s: read users.github_username: %v", orgID, err)
			continue
		}
		if username == "" {
			continue
		}
		if _, err := m.trackerForOrg(orgID).RefreshGitHub(client, username, userTeams, repos); err != nil {
			log.Printf("[github] org %s: tracker error: %v", orgID, err)
			m.reportError("github", orgID, err)
		}
	}
}

// startJira launches the Jira tracking loop. The outer goroutine
// just drives the tick; runJiraCycle resolves per-org Jira creds +
// project rules + base URL inside the per-org loop so each tenant
// is polled with its own configuration. Orgs without a connected
// Jira integration (no PAT, no URL, no rules) are silently skipped
// each cycle so adding/removing tenants doesn't need a poller
// restart.
//
// Gated to local mode (matching startGitHub). The per-org loop
// shape is correct but SecretStore.Get in Postgres requires
// request.jwt.claims (vault_* enforces org_id ==
// tf.current_org_id()), and the poller goroutine has no claims
// context. Multi-mode Jira polling needs either a SystemGet-style
// SecretStore variant or per-org SyntheticClaimsWithTx routing.
// Until then, multi-mode tenants don't get background polling;
// their data refreshes on the next interactive flow.
//
// interval is process-global (per-org cadence is future work); in
// local mode N=1 so the triggering org's interval IS the global
// interval.
//
// TODO: multi-mode Jira polling — add system-mode SecretStore
// access path (SKY-347 / D11 follow-up) then drop the gate below.
func (m *Manager) startJira(interval time.Duration) {
	if runmode.Current() != runmode.ModeLocal {
		log.Println("[jira] tracker not started — multi-mode Jira polling requires per-org system credentials (see TODO in startJira)")
		return
	}
	if interval < 10*time.Second {
		interval = time.Minute
	}

	stop := make(chan struct{})
	m.mu.Lock()
	m.jiraStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll
		m.runJiraCycle()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runJiraCycle()
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[jira] tracker started (interval: %s, per-org config resolved each cycle)", interval)
}

// runJiraCycle enumerates active orgs and dispatches a per-org
// RefreshJira. Each org's creds + rules + base URL are resolved
// inside the loop so two tenants with different Jira PATs / project
// configurations don't share state. Orgs not configured for Jira
// are skipped silently; per-org failures are logged + reported via
// OnError but do not abort the remaining orgs in the cycle.
func (m *Manager) runJiraCycle() {
	ctx := context.Background()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[jira] list active orgs: %v", err)
		m.reportError("jira", "", err)
		return
	}
	for _, orgID := range orgIDs {
		orgSet, oerr := m.orgs.GetSettingsSystem(ctx, orgID)
		if oerr != nil {
			log.Printf("[jira] org %s: load settings: %v", orgID, oerr)
			m.reportError("jira", orgID, oerr)
			continue
		}
		creds, lerr := integrations.Load(ctx, m.secrets, orgID)
		if lerr != nil {
			log.Printf("[jira] org %s: load creds: %v", orgID, lerr)
			m.reportError("jira", orgID, lerr)
			continue
		}
		rules := m.loadJiraRules(ctx, orgID)
		if creds.JiraPAT == "" || creds.JiraURL == "" || len(rules) == 0 {
			// Not configured for Jira (or rules missing). Skip
			// silently — adding/removing a tenant's Jira config
			// doesn't need a poller restart this way.
			continue
		}
		baseURL := orgSet.JiraBaseURL
		if baseURL == "" {
			baseURL = creds.JiraURL
		}
		client := jiraclient.NewClient(creds.JiraURL, creds.JiraPAT)
		projects := toTrackerJiraRules(rules)
		if _, err := m.trackerForOrg(orgID).RefreshJira(client, baseURL, projects); err != nil {
			log.Printf("[jira] org %s: tracker error: %v", orgID, err)
			m.reportError("jira", orgID, err)
		}
	}
}

// toTrackerJiraRules converts the domain per-project rule slice into
// the tracker-local view. Kept narrow on purpose — the tracker package
// only needs pickup/done members, not the canonicals.
func toTrackerJiraRules(rules []domain.JiraProjectStatusRules) tracker.JiraRules {
	out := make(tracker.JiraRules, 0, len(rules))
	for _, p := range rules {
		out = append(out, tracker.JiraProjectRules{
			Key:           p.ProjectKey,
			PickupMembers: p.PickupMembers,
			DoneMembers:   p.DoneMembers,
		})
	}
	return out
}
