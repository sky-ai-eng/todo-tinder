package delegate

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// These tests cover the validation paths Takeover() walks BEFORE it
// touches the worktree or DB row state. They don't need a real claude
// subprocess or git repo — Takeover bails out early on any of them
// with a typed sentinel error, and the HTTP handler maps those
// sentinels to status codes.
//
// The post-validation paths (copy, mark, abortTakeover) are exercised
// by the worktree and DB suites because they're the ones with
// non-trivial state machines; here we just guard the early-return
// contract the handler depends on.

// newTakeoverTestDB spins up an in-memory SQLite with the full schema
// so we can seed runs in any state Takeover validation cares about.
// Forcing single-conn because :memory: is per-conn — a pooled second
// connection would see an empty schema.
func newTakeoverTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", ":memory:?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SeedEventTypes(database); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	return database
}

// seedRun inserts a run with the requested fields and returns its ID.
// We bypass the spawner's Delegate flow because the validation tests
// don't need a real goroutine — only a row in the runs table.
func seedRun(t *testing.T, database *sql.DB, runID, sessionID, worktreePath string) {
	t.Helper()
	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#"+runID, "pr", "T", "https://example.com/"+runID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := db.RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := db.FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, runID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if existing, _ := db.GetPrompt(database, "test-prompt"); existing == nil {
		if err := db.CreatePrompt(database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create prompt: %v", err)
		}
	}
	if err := db.CreateAgentRun(database, domain.AgentRun{
		ID:           runID,
		TaskID:       task.ID,
		PromptID:     "test-prompt",
		Status:       "running",
		Model:        "claude-sonnet-4-6",
		WorktreePath: worktreePath,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = 'running', session_id = ?, worktree_path = ? WHERE id = ?`, sessionID, worktreePath, runID); err != nil {
		t.Fatalf("update run: %v", err)
	}
}

// newSpawnerWithActiveCancel returns a spawner with one fake "active"
// run registered in the cancels map — Takeover's atomic active-check
// requires this to pass before doing any other work.
func newSpawnerWithActiveCancel(database *sql.DB, runID string) *Spawner {
	s := NewSpawner(database, nil, nil, "claude-sonnet-4-6")
	if runID != "" {
		_, cancel := context.WithCancel(context.Background())
		s.cancels[runID] = cancel
	}
	return s
}

// TestTakeover_EmptyBaseDir is the cheapest validation: no DB or
// goroutine state needed. Returned error is a plain message, not a
// sentinel — empty base dir is a server config bug, not a client
// problem, and the handler routes uncategorized errors to 500.
func TestTakeover_EmptyBaseDir(t *testing.T) {
	s := NewSpawner(nil, nil, nil, "")
	_, err := s.Takeover(context.Background(), "any-run", "")
	if err == nil {
		t.Fatal("expected error on empty baseDir")
	}
	if errors.Is(err, ErrTakeoverInvalidState) ||
		errors.Is(err, ErrTakeoverInProgress) ||
		errors.Is(err, ErrTakeoverRaceLost) {
		t.Errorf("empty baseDir should not match a takeover sentinel; got %v", err)
	}
}

// TestTakeover_NonexistentRun: row doesn't exist → ErrTakeoverInvalidState.
// Maps to 400 in the handler.
func TestTakeover_NonexistentRun(t *testing.T) {
	database := newTakeoverTestDB(t)
	s := NewSpawner(database, nil, nil, "")

	_, err := s.Takeover(context.Background(), "no-such-run", "/tmp/dest")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_NoSessionID: row exists but session_id is empty (the
// agent hasn't produced its system/init event yet). Refuse — the
// resume command would have nothing to attach to.
func TestTakeover_NoSessionID(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-no-sid", "", "/tmp/wt")
	s := newSpawnerWithActiveCancel(database, "run-no-sid")

	_, err := s.Takeover(context.Background(), "run-no-sid", "/tmp/dest")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_NoWorktreePath: the no-repo Jira case — there's no
// worktree to take over, so the operation doesn't make sense.
func TestTakeover_NoWorktreePath(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-no-wt", "sess-1", "")
	s := newSpawnerWithActiveCancel(database, "run-no-wt")

	_, err := s.Takeover(context.Background(), "run-no-wt", "/tmp/dest")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_NoActiveRun: the row passes validation, has a session,
// has a worktree path — but the cancels map doesn't have an entry,
// meaning the goroutine has already exited (run finished naturally
// just before we ran). Refuse; the handler maps this to 400.
func TestTakeover_NoActiveRun(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-not-active", "sess-1", "/tmp/wt")
	// No cancels[runID] set.
	s := NewSpawner(database, nil, nil, "")

	_, err := s.Takeover(context.Background(), "run-not-active", "/tmp/dest")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_AlreadyInProgress: a second concurrent takeover for the
// same run hits the takenOver map check and gets ErrTakeoverInProgress.
// Maps to 409 in the handler. We pre-set the takenOver flag rather
// than running two real Takeover calls because the second one would
// race the first's later steps.
func TestTakeover_AlreadyInProgress(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-double", "sess-1", "/tmp/wt")
	s := newSpawnerWithActiveCancel(database, "run-double")
	s.takenOver["run-double"] = true

	_, err := s.Takeover(context.Background(), "run-double", "/tmp/dest")
	if !errors.Is(err, ErrTakeoverInProgress) {
		t.Errorf("err = %v, want ErrTakeoverInProgress", err)
	}
}

// TestWasTakenOver verifies the small accessor used by every gated
// cleanup path. A nil-safe read — the map is always initialized in
// NewSpawner — but cheap to assert.
func TestWasTakenOver(t *testing.T) {
	s := NewSpawner(nil, nil, nil, "")
	if s.wasTakenOver("missing") {
		t.Error("expected false for missing entry")
	}
	s.takenOver["present"] = true
	if !s.wasTakenOver("present") {
		t.Error("expected true after set")
	}
}
