package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestActiveRunIDsForTask verifies the terminal-state filter matches the one
// used by HasActiveRunForTask — the close cascade depends on this query
// returning exactly the runs that should be cancelled when a task closes.
func TestActiveRunIDsForTask(t *testing.T) {
	database := newTestDB(t)

	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := CreatePrompt(database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}

	// Seed runs in a mix of states. Non-terminal ones should appear in the
	// returned list; terminal ones (including pending_approval, which is
	// "terminal for the purposes of this gate", and taken_over which was
	// added with the takeover feature) must not.
	runs := []struct {
		id     string
		status string
		active bool
	}{
		{"run-init", "initializing", true},
		{"run-cloning", "cloning", true},
		{"run-running", "running", true},
		{"run-completed", "completed", false},
		{"run-failed", "failed", false},
		{"run-cancelled", "cancelled", false},
		{"run-unsolvable", "task_unsolvable", false},
		{"run-pending", "pending_approval", false},
		{"run-taken-over", "taken_over", false},
	}
	for _, r := range runs {
		if err := CreateAgentRun(database, domain.AgentRun{
			ID:       r.id,
			TaskID:   task.ID,
			PromptID: "test-prompt",
			Status:   r.status,
			Model:    "claude-sonnet-4-6",
		}); err != nil {
			t.Fatalf("create run %s: %v", r.id, err)
		}
		if r.status != "initializing" {
			if _, err := database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, r.status, r.id); err != nil {
				t.Fatalf("set run %s status: %v", r.id, err)
			}
		}
	}

	ids, err := ActiveRunIDsForTask(database, task.ID)
	if err != nil {
		t.Fatalf("ActiveRunIDsForTask: %v", err)
	}

	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, r := range runs {
		if r.active && !got[r.id] {
			t.Errorf("expected active run %s (status=%s) in result, missing", r.id, r.status)
		}
		if !r.active && got[r.id] {
			t.Errorf("unexpected terminal run %s (status=%s) in result", r.id, r.status)
		}
	}
}

// TestActiveRunIDsForTask_Empty returns nil (not error) when the task has
// no runs at all.
func TestActiveRunIDsForTask_Empty(t *testing.T) {
	database := newTestDB(t)
	ids, err := ActiveRunIDsForTask(database, "no-such-task")
	if err != nil {
		t.Fatalf("ActiveRunIDsForTask on missing task: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 ids, got %d", len(ids))
	}
}

// takeoverFixture spins up an entity, event, task, prompt, and run all
// pointing at one another so tests can exercise the takeover DB helpers
// without re-doing the FK boilerplate. The run is created with the
// requested initial status (force-set after CreateAgentRun's "cloning"
// default) and worktree_path so race-loss + worktree_path-update assertions
// have something to compare against.
//
// Each call uses a freshly suffixed entity/task ID so the same test file
// can call this multiple times against the same DB without colliding on
// the entity dedup key.
func takeoverFixture(t *testing.T, database *sql.DB, runID, status, worktreePath string) (taskID string) {
	t.Helper()

	entitySource := "owner/repo#" + runID
	entity, _, err := FindOrCreateEntity(database, "github", entitySource, "pr", "Test "+runID, "https://example.com/"+runID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, runID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Use FindOrCreate semantics for the prompt — multiple fixtures in
	// one test would otherwise collide on the unique ID.
	if existing, _ := GetPrompt(database, "test-prompt"); existing == nil {
		if err := CreatePrompt(database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create prompt: %v", err)
		}
	}
	if err := CreateAgentRun(database, domain.AgentRun{
		ID:           runID,
		TaskID:       task.ID,
		PromptID:     "test-prompt",
		Status:       "initializing",
		Model:        "claude-sonnet-4-6",
		WorktreePath: worktreePath,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if status != "initializing" {
		if _, err := database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, runID); err != nil {
			t.Fatalf("set status: %v", err)
		}
	}
	if worktreePath != "" {
		if _, err := database.Exec(`UPDATE runs SET worktree_path = ? WHERE id = ?`, worktreePath, runID); err != nil {
			t.Fatalf("set worktree_path: %v", err)
		}
	}
	return task.ID
}

// TestMarkAgentRunTakenOver_Active is the happy-path: an active run gets
// marked taken_over with the right metadata and worktree_path is updated
// to the takeover destination (so the row no longer points at the soon-
// to-be-deleted /tmp worktree).
func TestMarkAgentRunTakenOver_Active(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-takeover-active", "running", "/tmp/triagefactory-runs/run-takeover-active")

	dest := "/home/user/.triagefactory/takeovers/run-run-takeover-active"
	ok, err := MarkAgentRunTakenOver(database, "run-takeover-active", dest)
	if err != nil {
		t.Fatalf("MarkAgentRunTakenOver: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for active run")
	}

	got, err := GetAgentRun(database, "run-takeover-active")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if got.Status != "taken_over" {
		t.Errorf("Status = %q, want taken_over", got.Status)
	}
	if got.WorktreePath != dest {
		t.Errorf("WorktreePath = %q, want %q", got.WorktreePath, dest)
	}
	if got.StopReason != "user_takeover" {
		t.Errorf("StopReason = %q, want user_takeover", got.StopReason)
	}
	if got.ResultSummary == "" || !contains(got.ResultSummary, dest) {
		t.Errorf("ResultSummary = %q, want it to mention %q", got.ResultSummary, dest)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt was not set")
	} else if time.Since(*got.CompletedAt) > time.Minute {
		t.Errorf("CompletedAt = %v, expected ~now", got.CompletedAt)
	}
}

// TestMarkAgentRunTakenOver_RaceLoss covers every terminal status: if the
// goroutine wrote a real terminal status before our flag could land, the
// guarded UPDATE no-ops and we get ok=false. The original status (and
// worktree_path) must be preserved so the agent's actual outcome isn't
// clobbered with taken_over.
func TestMarkAgentRunTakenOver_RaceLoss(t *testing.T) {
	cases := []string{
		"completed",
		"failed",
		"cancelled",
		"task_unsolvable",
		"pending_approval",
		"taken_over",
	}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			database := newTestDB(t)
			origPath := "/tmp/triagefactory-runs/run-" + status
			takeoverFixture(t, database, "run-"+status, status, origPath)

			ok, err := MarkAgentRunTakenOver(database, "run-"+status, "/somewhere/new")
			if err != nil {
				t.Fatalf("MarkAgentRunTakenOver: %v", err)
			}
			if ok {
				t.Errorf("expected ok=false for terminal status %s", status)
			}

			got, err := GetAgentRun(database, "run-"+status)
			if err != nil {
				t.Fatalf("GetAgentRun: %v", err)
			}
			if got.Status != status {
				t.Errorf("Status changed from %q to %q — race-loss path must preserve original outcome", status, got.Status)
			}
			if got.WorktreePath != origPath {
				t.Errorf("WorktreePath changed from %q to %q — race-loss path must not overwrite", origPath, got.WorktreePath)
			}
		})
	}
}

// TestMarkAgentRunTakenOver_NonexistentRun returns ok=false (no rows) without
// erroring. The takeover handler treats this the same as race-loss.
func TestMarkAgentRunTakenOver_NonexistentRun(t *testing.T) {
	database := newTestDB(t)
	ok, err := MarkAgentRunTakenOver(database, "no-such-run", "/dest")
	if err != nil {
		t.Fatalf("MarkAgentRunTakenOver: %v", err)
	}
	if ok {
		t.Error("expected ok=false for nonexistent run")
	}
}

// TestMarkAgentRunCancelledIfActive_Active flips an active run to
// cancelled with the supplied stop_reason — used by abortTakeover to
// recover from copy/DB failures so the row doesn't sit in 'running'.
func TestMarkAgentRunCancelledIfActive_Active(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-cancellable", "running", "")

	ok, err := MarkAgentRunCancelledIfActive(database, "run-cancellable", "test_reason", "test summary")
	if err != nil {
		t.Fatalf("MarkAgentRunCancelledIfActive: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for active run")
	}

	got, err := GetAgentRun(database, "run-cancellable")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if got.Status != "cancelled" {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}
	if got.StopReason != "test_reason" {
		t.Errorf("StopReason = %q, want test_reason", got.StopReason)
	}
	if got.ResultSummary != "test summary" {
		t.Errorf("ResultSummary = %q, want test summary", got.ResultSummary)
	}
}

// TestMarkAgentRunCancelledIfActive_AlreadyTerminal must no-op for every
// terminal status — the race-loss leg of abortTakeover relies on this to
// preserve the agent's actual outcome instead of overwriting with
// 'cancelled'.
func TestMarkAgentRunCancelledIfActive_AlreadyTerminal(t *testing.T) {
	cases := []string{
		"completed",
		"failed",
		"cancelled",
		"task_unsolvable",
		"pending_approval",
		"taken_over",
	}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			database := newTestDB(t)
			takeoverFixture(t, database, "run-term-"+status, status, "")

			ok, err := MarkAgentRunCancelledIfActive(database, "run-term-"+status, "should_not_apply", "should not apply")
			if err != nil {
				t.Fatalf("MarkAgentRunCancelledIfActive: %v", err)
			}
			if ok {
				t.Errorf("expected ok=false for terminal status %s", status)
			}

			got, err := GetAgentRun(database, "run-term-"+status)
			if err != nil {
				t.Fatalf("GetAgentRun: %v", err)
			}
			if got.Status != status {
				t.Errorf("Status changed from %q to %q — must preserve terminal status", status, got.Status)
			}
		})
	}
}

// TestListTakenOverRunIDs returns only runs whose status is taken_over.
// Used at startup to decide which ~/.claude/projects entries to preserve
// during the orphan sweep.
func TestListTakenOverRunIDs(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-A", "running", "")
	takeoverFixture(t, database, "run-B", "taken_over", "")
	takeoverFixture(t, database, "run-C", "completed", "")
	takeoverFixture(t, database, "run-D", "taken_over", "")

	got, err := ListTakenOverRunIDs(database)
	if err != nil {
		t.Fatalf("ListTakenOverRunIDs: %v", err)
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet["run-B"] || !gotSet["run-D"] {
		t.Errorf("missing taken_over runs; got %v", got)
	}
	if gotSet["run-A"] || gotSet["run-C"] {
		t.Errorf("included non-taken_over runs; got %v", got)
	}
}

// TestListTakenOverRunIDs_Empty returns nil (no runs match the filter)
// without erroring. Startup must tolerate this — it's the common case
// after a clean shutdown.
func TestListTakenOverRunIDs_Empty(t *testing.T) {
	database := newTestDB(t)
	got, err := ListTakenOverRunIDs(database)
	if err != nil {
		t.Fatalf("ListTakenOverRunIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

// contains is a small string-contains helper used by these tests so we
// don't pull strings into the imports for one assertion. Faster to
// inline than to round-trip through strings.Contains.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
