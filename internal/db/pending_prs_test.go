package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedPendingPRRun installs a Jira run wired through to a fake task,
// then returns the run id. pending_prs.run_id has a FK + UNIQUE
// constraint, so every pending-PR test needs a real runs row anchored
// to a task. Reuses the existing seedJiraRun helper from
// run_worktrees_test.go.
func seedPendingPRRun(t *testing.T, database *sql.DB, runID string) {
	t.Helper()
	seedJiraRun(t, database, runID)
}

func TestCreatePendingPR_SnapshotsOriginalsAtInsert(t *testing.T) {
	// At-queue-time snapshot: even before LockPendingPR runs, the
	// row should already have original_title / original_body
	// captured. This matters because human edits via PATCH can land
	// before the agent calls Lock, and the human-feedback diff needs
	// a stable baseline.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")

	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc123", BaseBranch: "main",
		Title: "Agent draft title",
		Body:  "Agent draft body",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}

	got, err := GetPendingPR(database, "pr1")
	if err != nil {
		t.Fatalf("GetPendingPR: %v", err)
	}
	if got == nil {
		t.Fatal("expected row, got nil")
	}
	if got.OriginalTitle == nil || *got.OriginalTitle != "Agent draft title" {
		t.Errorf("OriginalTitle = %v, want pointer to \"Agent draft title\"", got.OriginalTitle)
	}
	if got.OriginalBody == nil || *got.OriginalBody != "Agent draft body" {
		t.Errorf("OriginalBody = %v, want pointer to \"Agent draft body\"", got.OriginalBody)
	}
	if got.Locked {
		t.Error("Locked = true on fresh row, want false")
	}
	if got.SubmittedAt != nil {
		t.Errorf("SubmittedAt = %v, want nil on fresh row", got.SubmittedAt)
	}
}

func TestCreatePendingPR_RunIDIsUnique(t *testing.T) {
	// One pending PR per run — same contract as reviews. Second
	// CreatePendingPR with the same run_id violates the UNIQUE
	// constraint and surfaces as a SQL error.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")

	base := domain.PendingPR{
		RunID: "r1", Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc", BaseBranch: "main",
		Title: "T1",
	}
	first := base
	first.ID = "pr1"
	if err := CreatePendingPR(database, first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	second := base
	second.ID = "pr2" // distinct PR id, same run_id
	if err := CreatePendingPR(database, second); err == nil {
		t.Errorf("expected UNIQUE-constraint error on second insert with same run_id")
	}
}

func TestUpdatePendingPRTitleBody_PreservesOriginals(t *testing.T) {
	// Human-edit path. The agent's snapshot is frozen; subsequent
	// user edits move the visible title/body forward but originals
	// stay put.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc", BaseBranch: "main",
		Title: "Agent draft", Body: "Agent body",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}

	if err := UpdatePendingPRTitleBody(database, "pr1", "Human edit", "Human body"); err != nil {
		t.Fatalf("UpdatePendingPRTitleBody: %v", err)
	}

	got, err := GetPendingPR(database, "pr1")
	if err != nil {
		t.Fatalf("GetPendingPR: %v", err)
	}
	if got.Title != "Human edit" {
		t.Errorf("Title = %q, want %q", got.Title, "Human edit")
	}
	if got.Body != "Human body" {
		t.Errorf("Body = %q, want %q", got.Body, "Human body")
	}
	if got.OriginalTitle == nil || *got.OriginalTitle != "Agent draft" {
		t.Errorf("OriginalTitle = %v, want frozen at \"Agent draft\"", got.OriginalTitle)
	}
	if got.OriginalBody == nil || *got.OriginalBody != "Agent body" {
		t.Errorf("OriginalBody = %v, want frozen at \"Agent body\"", got.OriginalBody)
	}
}

func TestLockPendingPR_FirstCallSucceedsAndSetsLocked(t *testing.T) {
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc", BaseBranch: "main",
		Title: "T", Body: "B",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}

	if err := LockPendingPR(database, "pr1", "T-locked", "B-locked"); err != nil {
		t.Fatalf("LockPendingPR first call: %v", err)
	}

	got, err := GetPendingPR(database, "pr1")
	if err != nil {
		t.Fatalf("GetPendingPR: %v", err)
	}
	if !got.Locked {
		t.Error("Locked = false after lock, want true")
	}
	if got.Title != "T-locked" || got.Body != "B-locked" {
		t.Errorf("title/body = %q/%q, want T-locked/B-locked", got.Title, got.Body)
	}
}

func TestLockPendingPR_SecondCallReturnsAlreadyQueued(t *testing.T) {
	// SKY-212 anti-retry: the agent retries `pr create` (e.g. didn't
	// see the response, ambiguous tool result). Second LockPendingPR
	// must return the typed sentinel so the CLI can render a clean
	// "already queued" message rather than a generic SQL error.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc", BaseBranch: "main",
		Title: "T",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}
	if err := LockPendingPR(database, "pr1", "T", "B"); err != nil {
		t.Fatalf("first lock: %v", err)
	}

	err := LockPendingPR(database, "pr1", "T-2nd", "B-2nd")
	if !errors.Is(err, ErrPendingPRAlreadyQueued) {
		t.Errorf("err = %v, want ErrPendingPRAlreadyQueued", err)
	}

	// Sanity: the second call must NOT have mutated title/body.
	got, _ := GetPendingPR(database, "pr1")
	if got.Title != "T" || got.Body != "B" {
		t.Errorf("title/body mutated by rejected second lock: %q/%q", got.Title, got.Body)
	}
}

func TestLockPendingPR_BogusIDIsDistinctFromAlreadyQueued(t *testing.T) {
	// A non-existent id should NOT get the SKY-212 "already queued"
	// message — that would mislead the agent into thinking it was a
	// retry. Surface a "not found" error instead.
	database := newTestDB(t)
	err := LockPendingPR(database, "no-such-pr", "T", "B")
	if errors.Is(err, ErrPendingPRAlreadyQueued) {
		t.Errorf("bogus id got ErrPendingPRAlreadyQueued; want a not-found error instead")
	}
	if err == nil {
		t.Error("expected an error for bogus id, got nil")
	}
}

func TestMarkPendingPRSubmitted_FirstCallWins(t *testing.T) {
	// Concurrent-submit guard: two browser tabs click "Open PR" at
	// the same time; only one should proceed to GitHub's CreatePR.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc", BaseBranch: "main",
		Title: "T",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}

	winner1, err := MarkPendingPRSubmitted(database, "pr1")
	if err != nil {
		t.Fatalf("first MarkPendingPRSubmitted: %v", err)
	}
	if !winner1 {
		t.Error("first call winner = false, want true")
	}

	winner2, err := MarkPendingPRSubmitted(database, "pr1")
	if !errors.Is(err, ErrPendingPRSubmitInFlight) {
		t.Errorf("second call err = %v, want ErrPendingPRSubmitInFlight", err)
	}
	if winner2 {
		t.Error("second call winner = true, want false")
	}
}

func TestClearPendingPRSubmitted_AllowsRetry(t *testing.T) {
	// On submit failure the server clears the guard so the user can
	// retry. After clear, MarkPendingPRSubmitted wins again.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc", BaseBranch: "main",
		Title: "T",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}
	if _, err := MarkPendingPRSubmitted(database, "pr1"); err != nil {
		t.Fatalf("first mark: %v", err)
	}
	if err := ClearPendingPRSubmitted(database, "pr1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	winner, err := MarkPendingPRSubmitted(database, "pr1")
	if err != nil {
		t.Fatalf("retry mark: %v", err)
	}
	if !winner {
		t.Error("retry mark winner = false; expected true after clear")
	}
}

func TestPendingPRByRunID_FindsAndProjects(t *testing.T) {
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "owner", Repo: "repo",
		HeadBranch: "feature/SKY-1", HeadSHA: "abc", BaseBranch: "main",
		Title: "T", Body: "B",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}

	got, err := PendingPRByRunID(database, "r1")
	if err != nil {
		t.Fatalf("PendingPRByRunID: %v", err)
	}
	if got == nil || got.ID != "pr1" {
		t.Errorf("got = %+v, want id=pr1", got)
	}

	missing, err := PendingPRByRunID(database, "does-not-exist")
	if err != nil {
		t.Fatalf("PendingPRByRunID missing: %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for missing run, got %+v", missing)
	}
}

func TestDeletePendingPRByRunID_IsNoOpWhenNoneExists(t *testing.T) {
	// Idempotent — used by cleanupPendingApprovalRun, which calls
	// both DeletePendingReviewByRunID and DeletePendingPRByRunID
	// regardless of which side-table actually has a row.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	// No CreatePendingPR. Just check we can delete-by-run with no
	// row present.
	if err := DeletePendingPRByRunID(database, "r1"); err != nil {
		t.Errorf("expected no-op, got %v", err)
	}
}

func TestDeletePendingPRByRunID_RemovesRow(t *testing.T) {
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "s", BaseBranch: "main",
		Title: "T",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}
	if err := DeletePendingPRByRunID(database, "r1"); err != nil {
		t.Fatalf("DeletePendingPRByRunID: %v", err)
	}
	got, err := PendingPRByRunID(database, "r1")
	if err != nil {
		t.Fatalf("PendingPRByRunID: %v", err)
	}
	if got != nil {
		t.Errorf("expected row deleted, got %+v", got)
	}
}

func TestPendingPR_CascadeOnRunDelete(t *testing.T) {
	// FK cascade — deleting the runs row should reap the pending_prs
	// row. Confirms the FK definition in the migration.
	database := newTestDB(t)
	seedPendingPRRun(t, database, "r1")
	if err := CreatePendingPR(database, domain.PendingPR{
		ID: "pr1", RunID: "r1",
		Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "s", BaseBranch: "main",
		Title: "T",
	}); err != nil {
		t.Fatalf("CreatePendingPR: %v", err)
	}

	if _, err := database.Exec(`DELETE FROM runs WHERE id = ?`, "r1"); err != nil {
		t.Fatalf("delete run: %v", err)
	}

	// CASCADE should have removed the pending_prs row.
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pending_prs WHERE id = ?`, "pr1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("pending_prs row survived run deletion: count=%d", count)
	}
}
