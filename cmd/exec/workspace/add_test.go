package workspace

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
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
// INSERT OR IGNORE on the run_worktrees PK, the actual queries).
// Mocking DB calls would test less of the actual code under change.
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

// expectedPath returns the deterministic worktree path materializeWorkspace
// will compute for a given runID + owner/repo. Computed via the same
// worktree.RunRoot helper so test assertions stay aligned with the runtime.
func expectedPath(runID, owner, repo string) string {
	return filepath.Join(worktree.RunRoot(runID), owner, repo)
}

// fakeFileInfo implements os.FileInfo just enough for the stat stub.
type fakeFileInfo struct {
	name  string
	mode  os.FileMode
	isDir bool
}

func (f *fakeFileInfo) Name() string       { return f.name }
func (f *fakeFileInfo) Size() int64        { return 0 }
func (f *fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f *fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f *fakeFileInfo) IsDir() bool        { return f.isDir }
func (f *fakeFileInfo) Sys() any           { return nil }

// stubCalls records create/remove/stat invocations and returns canned
// responses. The default stat behavior returns ErrNotExist (path doesn't
// exist) — appropriate for tests where no prior worktree should appear
// live; tests covering idempotent re-add or stale-row handling override
// statResponse.
type stubCalls struct {
	mu sync.Mutex

	createCalls int
	createArgs  []createCall
	createPath  string
	createErr   error

	removeCalls int
	removePaths []string

	// statResponse maps absolute path → (info, err). Paths not in the
	// map default to (nil, os.ErrNotExist) so the orchestration's
	// "path missing → drop stale row" branch is the default behavior.
	statResponse map[string]statReply
}

type statReply struct {
	info os.FileInfo
	err  error
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
			if s.createErr != nil {
				return "", s.createErr
			}
			path := s.createPath
			if path == "" {
				// Default to the deterministic path the production
				// implementation would produce, so tests don't need
				// to set createPath unless they need a specific value.
				path = filepath.Join(runRoot, owner, repo)
			}
			return path, nil
		},
		removeWorktree: func(path, _ string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.removeCalls++
			s.removePaths = append(s.removePaths, path)
			return nil
		},
		statPath: func(path string) (os.FileInfo, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if r, ok := s.statResponse[path]; ok {
				return r.info, r.err
			}
			return nil, &fs.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
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
	stub := &stubCalls{}

	wantPath := expectedPath("r1", "sky", "core")
	path, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
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

	// Verify the row landed with the deterministic path.
	row, err := db.GetRunWorktreeByRepo(database.Conn, "r1", "sky/core")
	if err != nil {
		t.Fatalf("GetRunWorktreeByRepo: %v", err)
	}
	if row == nil {
		t.Fatal("expected run_worktrees row, got nil")
	}
	if row.Path != wantPath {
		t.Errorf("row.Path = %q, want %q", row.Path, wantPath)
	}
	if row.FeatureBranch != "feature/SKY-220" {
		t.Errorf("row.FeatureBranch = %q", row.FeatureBranch)
	}
}

func TestMaterializeWorkspace_BaseBranchFallsBackToDefault(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	// BaseBranch empty → use DefaultBranch.
	seedRepoProfile(t, database, "owner", "repo", "https://x", "develop")
	stub := &stubCalls{}

	if _, err := materializeWorkspace(database, "r1", "owner/repo", stub.deps()); err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if stub.createArgs[0].baseBranch != "develop" {
		t.Errorf("baseBranch = %q, want develop", stub.createArgs[0].baseBranch)
	}
}

func TestMaterializeWorkspace_IdempotentSecondAdd(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	// Tell the stat stub the worktree dir exists (the precheck will hit
	// it on the second call once the row's been inserted).
	statResp := map[string]statReply{
		wantPath: {info: &fakeFileInfo{name: "core", isDir: true}},
	}

	stub := &stubCalls{statResponse: statResp}

	if _, err := materializeWorkspace(database, "r1", "sky/core", stub.deps()); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if stub.createCalls != 1 {
		t.Fatalf("first add createCalls = %d, want 1", stub.createCalls)
	}

	// Second add: GetRunWorktreeByRepo returns the row, statPath says
	// the dir exists, so we short-circuit before reservation/create.
	path2, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if path2 != wantPath {
		t.Errorf("idempotent path = %q, want %q", path2, wantPath)
	}
	if stub.createCalls != 1 {
		t.Errorf("createWorktree called %d times across two adds; second add should short-circuit on live precheck", stub.createCalls)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeWorktree called on idempotent re-add")
	}
}

func TestMaterializeWorkspace_StaleRowDroppedAndReserved(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	// Pre-insert a row (e.g. from a prior crashed run that left state
	// behind) AND tell the stat stub the path doesn't exist.
	if _, _, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: wantPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	stub := &stubCalls{} // default statPath returns ErrNotExist

	path, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	// Stale row was dropped, then re-reserved + created.
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (stale row should not block re-reservation)", stub.createCalls)
	}
}

func TestMaterializeWorkspace_RaceLossAtReservation(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	// Simulate a concurrent add that won the reservation race: insert
	// the winning row directly, with a distinguishable path so we can
	// confirm the loser returns IT and not its own pre-computed path.
	winnerPath := "/tmp/somewhere-else/winner"
	if _, _, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: winnerPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed winner row: %v", err)
	}
	// Mark the winner's path as a live directory so the precheck sees
	// it as a valid idempotent re-add. (Without this, the stale-row
	// branch would drop the winner's row — unintended in the race
	// scenario, where the winner is mid-create or just-created.)
	stub := &stubCalls{
		statResponse: map[string]statReply{
			winnerPath: {info: &fakeFileInfo{name: "winner", isDir: true}},
		},
	}

	path, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != winnerPath {
		t.Errorf("path = %q, want %q (winner's path)", path, winnerPath)
	}
	if stub.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0; loser must NOT touch git", stub.createCalls)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeCalls = %d, want 0; loser has nothing to remove with the new flow", stub.removeCalls)
	}
}

func TestMaterializeWorkspace_RaceLossAtReservationWithMissingWinnerDir(t *testing.T) {
	// Edge case of the race: the winner inserted the row but its
	// createWorktree is still in flight, so the winner's path doesn't
	// exist on disk yet. The loser races through:
	//   - Precheck: row exists, but stat says path missing → drop
	//     the row as "stale," fall through to reservation.
	//   - Reservation: INSERT succeeds (row was just deleted).
	//   - Create: runs.
	// Net effect: BOTH processes wind up trying to create the same
	// dir, and the second's `git worktree add` fails. The DB state
	// reflects whichever one completed createWorktree successfully.
	//
	// We don't try to fix this edge — the create-in-flight window is
	// brief, and the worktree library's lockRepo serializes the bare
	// operations within a process. Across processes this remains a
	// known limitation tracked by the test below: we DO fall through
	// to recreate, and the bare's per-repo lock + the OS-level "dir
	// already exists" semantics handle the conflict.
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	winnerPath := expectedPath("r1", "sky", "core")
	if _, _, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: winnerPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed winner row: %v", err)
	}
	// Stat says missing → loser drops the row and re-reserves.
	stub := &stubCalls{}

	path, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != winnerPath {
		t.Errorf("path = %q, want %q", path, winnerPath)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (loser fell through and ran create)", stub.createCalls)
	}
}

func TestMaterializeWorkspace_CreateFailureReleasesReservation(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	// Make createWorktree fail (e.g. network error fetching the bare).
	stub := &stubCalls{createErr: errors.New("simulated git failure")}

	_, err := materializeWorkspace(database, "r1", "sky/core", stub.deps())
	if err == nil {
		t.Fatal("expected error from materializeWorkspace, got nil")
	}
	if !strings.Contains(err.Error(), "simulated git failure") {
		t.Errorf("err = %v, expected to wrap 'simulated git failure'", err)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", stub.createCalls)
	}

	// The reservation must have been released so the next attempt
	// can re-reserve. Verify the row is gone.
	row, err := db.GetRunWorktreeByRepo(database.Conn, "r1", "sky/core")
	if err != nil {
		t.Fatalf("GetRunWorktreeByRepo: %v", err)
	}
	if row != nil {
		t.Errorf("expected run_worktrees row to be released after create failure, found %+v", row)
	}
}

func TestMaterializeWorkspace_CreateFailureRetryable(t *testing.T) {
	// End-to-end of the release-on-failure contract: a first attempt
	// fails (createWorktree errors), reservation is released, a second
	// attempt succeeds.
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	stub1 := &stubCalls{createErr: errors.New("network blip")}
	if _, err := materializeWorkspace(database, "r1", "sky/core", stub1.deps()); err == nil {
		t.Fatal("expected first-attempt failure")
	}

	stub2 := &stubCalls{}
	path, err := materializeWorkspace(database, "r1", "sky/core", stub2.deps())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if path != wantPath {
		t.Errorf("retry path = %q, want %q", path, wantPath)
	}
	if stub2.createCalls != 1 {
		t.Errorf("retry createCalls = %d, want 1", stub2.createCalls)
	}
}

func TestMaterializeWorkspace_TooManySlashesAccepted(t *testing.T) {
	// SplitN keeps "too/many/slashes" as ("too", "many/slashes").
	// Verify the orchestration treats that as a configured-repo lookup
	// against repoID "too/many/slashes" (which won't exist →
	// errRepoNotConfigured), not a parse-time reject.
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	stub := &stubCalls{}

	_, err := materializeWorkspace(database, "r1", "too/many/slashes", stub.deps())
	if !errors.Is(err, errRepoNotConfigured) {
		t.Errorf("err = %v, want errRepoNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "too/many/slashes") {
		t.Errorf("error %q should mention the full repoID", err.Error())
	}
}
