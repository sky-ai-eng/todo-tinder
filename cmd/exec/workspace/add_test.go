package workspace

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	_ "modernc.org/sqlite"
)

func TestSplitOwnerRepo(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"sky-ai-eng/triage-factory", "sky-ai-eng", "triage-factory", true},
		{"a/b", "a", "b", true},

		// Malformed inputs all reject — no half-parsed owner/repo.
		{"", "", "", false},
		{"no-slash", "", "", false},
		{"/missing-owner", "", "", false},
		{"missing-repo/", "", "", false},
		{"too/many/slashes", "too", "many/slashes", true}, // SplitN keeps a /-bearing repo half intact; not our concern to reject
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			owner, repo, ok := splitOwnerRepo(c.in)
			if owner != c.wantOwner || repo != c.wantRepo || ok != c.wantOK {
				t.Errorf("splitOwnerRepo(%q) = (%q, %q, %v); want (%q, %q, %v)",
					c.in, owner, repo, ok, c.wantOwner, c.wantRepo, c.wantOK)
			}
		})
	}
}

// newTestDB spins up an in-memory SQLite with the full schema so the
// orchestration tests run against the real DB layer (FK cascades,
// INSERT OR IGNORE behavior on the run_worktrees PK, the actual
// scanProject etc. queries). Mocking DB calls would be testing less.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SeedEventTypes(conn); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	return &db.DB{Conn: conn}
}

// seedJiraRun creates the entity → event → task → prompt → run chain
// needed for materializeWorkspace to find a Jira run. Returns runID.
// issueKey controls the entity's source ID (so tests asserting on
// feature_branch can predict the value: "feature/<issueKey>").
func seedJiraRun(t *testing.T, database *db.DB, runID, issueKey string) {
	t.Helper()
	entity, _, err := db.FindOrCreateEntity(database.Conn, "jira", issueKey, "issue", "T-"+issueKey, "https://x/"+issueKey)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := db.RecordEvent(database.Conn, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := db.FindOrCreateTask(database.Conn, entity.ID, domain.EventJiraIssueAssigned, runID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := db.CreatePrompt(database.Conn, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := db.CreateAgentRun(database.Conn, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// seedGitHubRun creates a GitHub PR run — used to verify the Jira-only
// gate rejects `workspace add` against PR runs cleanly.
func seedGitHubRun(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	entity, _, err := db.FindOrCreateEntity(database.Conn, "github", "owner/repo#"+runID, "pr", "T", "https://x/"+runID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := db.RecordEvent(database.Conn, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := db.FindOrCreateTask(database.Conn, entity.ID, domain.EventGitHubPRCICheckFailed, runID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := db.CreatePrompt(database.Conn, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := db.CreateAgentRun(database.Conn, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// seedRepoProfile inserts a repo profile so the ProfileGate validation
// passes. cloneURL controls the empty-clone-URL rejection branch.
func seedRepoProfile(t *testing.T, database *db.DB, owner, repo, cloneURL, defaultBranch string) {
	t.Helper()
	if err := db.UpsertRepoProfile(database.Conn, domain.RepoProfile{
		ID: owner + "/" + repo, Owner: owner, Repo: repo,
		CloneURL: cloneURL, DefaultBranch: defaultBranch,
		ProfileText: "test profile",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
}

// stubDeps returns addDeps whose createWorktree records the call args
// and returns the predetermined path / error, and whose removeWorktree
// counts invocations for rollback assertions.
type stubCalls struct {
	mu          sync.Mutex
	createCalls int
	createArgs  []createCall
	createPath  string
	createErr   error
	removeCalls int
	removePaths []string
}

type createCall struct {
	owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string
}

func (s *stubCalls) deps() addDeps {
	return addDeps{
		createWorktree: func(_ context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.createCalls++
			s.createArgs = append(s.createArgs, createCall{owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot})
			return s.createPath, s.createErr
		},
		removeWorktree: func(path, _ string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.removeCalls++
			s.removePaths = append(s.removePaths, path)
			return nil
		},
	}
}

func TestMaterializeWorkspace_MissingRunID(t *testing.T) {
	database := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(database, "" /*runID*/, "owner/repo", stub.deps())
	if !errors.Is(err, errMissingRunID) {
		t.Errorf("err = %v, want errMissingRunID", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called %d times on missing run id; should not be invoked before validation", stub.createCalls)
	}
}

func TestMaterializeWorkspace_InvalidOwnerRepo(t *testing.T) {
	database := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(database, "r1", "no-slash", stub.deps())
	if !errors.Is(err, errInvalidOwnerRepo) {
		t.Errorf("err = %v, want errInvalidOwnerRepo", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called %d times on invalid owner/repo", stub.createCalls)
	}
}

func TestMaterializeWorkspace_RunNotFound(t *testing.T) {
	database := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(database, "missing-run", "owner/repo", stub.deps())
	if !errors.Is(err, errRunNotFound) {
		t.Errorf("err = %v, want errRunNotFound", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for missing run")
	}
}

func TestMaterializeWorkspace_RejectsGitHubPRRun(t *testing.T) {
	database := newTestDB(t)
	seedGitHubRun(t, database, "gh-run")
	stub := &stubCalls{}

	_, err := materializeWorkspace(database, "gh-run", "owner/repo", stub.deps())
	if !errors.Is(err, errNotJiraRun) {
		t.Errorf("err = %v, want errNotJiraRun", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for GitHub PR run; should be rejected before create")
	}
}

func TestMaterializeWorkspace_RepoNotConfigured(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	stub := &stubCalls{}

	// No repo profile seeded → profile lookup returns nil.
	_, err := materializeWorkspace(database, "r1", "owner/repo", stub.deps())
	if !errors.Is(err, errRepoNotConfigured) {
		t.Errorf("err = %v, want errRepoNotConfigured", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for unconfigured repo")
	}
}

func TestMaterializeWorkspace_RepoMissingCloneURL(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "owner", "repo", "" /*cloneURL*/, "main")
	stub := &stubCalls{}

	_, err := materializeWorkspace(database, "r1", "owner/repo", stub.deps())
	if !errors.Is(err, errRepoMissingCloneURL) {
		t.Errorf("err = %v, want errRepoMissingCloneURL", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for profile with empty clone URL")
	}
}

func TestMaterializeWorkspace_SuccessfulFirstAdd(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-220")
	seedRepoProfile(t, database, "sky", "core", "https://github.com/sky/core.git", "main")
	stub := &stubCalls{createPath: "/tmp/wt/r1/sky/core"}

	path, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != "/tmp/wt/r1/sky/core" {
		t.Errorf("path = %q, want /tmp/wt/r1/sky/core", path)
	}
	if stub.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", stub.createCalls)
	}
	args := stub.createArgs[0]
	if args.owner != "sky" || args.repo != "core" {
		t.Errorf("create args owner/repo = %s/%s, want sky/core", args.owner, args.repo)
	}
	if args.cloneURL != "https://github.com/sky/core.git" {
		t.Errorf("cloneURL = %q", args.cloneURL)
	}
	if args.featureBranch != "feature/SKY-220" {
		t.Errorf("featureBranch = %q, want feature/SKY-220", args.featureBranch)
	}
	if args.baseBranch != "main" {
		t.Errorf("baseBranch = %q, want main", args.baseBranch)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeWorktree called %d times on success path", stub.removeCalls)
	}

	// Verify the row landed.
	row, err := db.GetRunWorktreeByRepo(database.Conn, "r1", "sky/core")
	if err != nil {
		t.Fatalf("GetRunWorktreeByRepo: %v", err)
	}
	if row == nil {
		t.Fatal("expected run_worktrees row, got nil")
	}
	if row.Path != "/tmp/wt/r1/sky/core" || row.FeatureBranch != "feature/SKY-220" {
		t.Errorf("row = %+v", row)
	}
}

func TestMaterializeWorkspace_BaseBranchFallsBackToDefault(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	// BaseBranch is empty (not set), so the function should use DefaultBranch.
	seedRepoProfile(t, database, "owner", "repo", "https://x", "develop")
	stub := &stubCalls{createPath: "/wt"}

	if _, err := materializeWorkspace(database, "r1", "owner/repo", stub.deps()); err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if stub.createArgs[0].baseBranch != "develop" {
		t.Errorf("baseBranch = %q, want develop (the default branch)", stub.createArgs[0].baseBranch)
	}
}

func TestMaterializeWorkspace_IdempotentSecondAdd(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{createPath: "/tmp/wt/r1/sky/core"}

	// First add lands a row.
	if _, err := materializeWorkspace(database, "r1", "sky/core", stub.deps()); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if stub.createCalls != 1 {
		t.Fatalf("first add createCalls = %d, want 1", stub.createCalls)
	}

	// Second add for the same (run, repo) must short-circuit BEFORE
	// touching createWorktree — that's the whole point of the idempotency
	// gate. It must return the original path.
	path2, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if path2 != "/tmp/wt/r1/sky/core" {
		t.Errorf("idempotent path = %q, want /tmp/wt/r1/sky/core", path2)
	}
	if stub.createCalls != 1 {
		t.Errorf("createWorktree called %d times across two adds; second add should short-circuit", stub.createCalls)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeWorktree called on idempotent re-add")
	}
}

func TestMaterializeWorkspace_RollbackOnInsertRaceLoss(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	// Simulate a concurrent add winning the race: pre-insert a
	// run_worktrees row for the same (run, repo) BEFORE we call
	// materializeWorkspace. The Get-by-repo idempotency check would
	// short-circuit, so we have to bypass it: we'll construct a stub
	// whose createWorktree, when invoked, races by inserting a
	// conflicting row mid-call. That way materializeWorkspace's own
	// idempotency check (which runs first) misses it, but the post-
	// create InsertRunWorktree's INSERT OR IGNORE sees the conflict.
	stub := &stubCalls{createPath: "/tmp/wt/r1/sky/core"}
	stub.deps()
	// Override createWorktree to insert the winning row before returning.
	deps := addDeps{
		createWorktree: func(_ context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string) (string, error) {
			// "Concurrent" winner inserts first.
			if _, _, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
				RunID: runID, RepoID: owner + "/" + repo,
				Path: "/tmp/wt/r1/sky/core-WINNER", FeatureBranch: featureBranch,
			}); err != nil {
				t.Fatalf("inject winning row: %v", err)
			}
			stub.mu.Lock()
			stub.createCalls++
			stub.mu.Unlock()
			return "/tmp/wt/r1/sky/core", nil
		},
		removeWorktree: func(path, _ string) error {
			stub.mu.Lock()
			defer stub.mu.Unlock()
			stub.removeCalls++
			stub.removePaths = append(stub.removePaths, path)
			return nil
		},
	}

	path, err := materializeWorkspace(database, "r1", "sky/core", deps)
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != "/tmp/wt/r1/sky/core-WINNER" {
		t.Errorf("path = %q, want the winning row's path /tmp/wt/r1/sky/core-WINNER", path)
	}
	if stub.removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1 (the loser's just-created path)", stub.removeCalls)
	}
	if stub.removePaths[0] != "/tmp/wt/r1/sky/core" {
		t.Errorf("removed path = %q, want /tmp/wt/r1/sky/core (the loser path our stub returned)", stub.removePaths[0])
	}
}

func TestMaterializeWorkspace_TooManySlashesAccepted(t *testing.T) {
	// SplitN keeps "too/many/slashes" as ("too", "many/slashes") — see
	// TestSplitOwnerRepo. Verify the orchestration treats that as a
	// configured-repo lookup against repoID "too/many/slashes" (which
	// won't exist → repoNotConfigured), not a parse-time reject.
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	stub := &stubCalls{}

	_, err := materializeWorkspace(database, "r1", "too/many/slashes", stub.deps())
	if !errors.Is(err, errRepoNotConfigured) {
		t.Errorf("err = %v, want errRepoNotConfigured (slash-bearing repo should round-trip through repo lookup)", err)
	}
	// The error message should include the full repoID so the agent
	// can see what was looked up.
	if !strings.Contains(err.Error(), "too/many/slashes") {
		t.Errorf("error %q should mention the full repoID", err.Error())
	}
}
