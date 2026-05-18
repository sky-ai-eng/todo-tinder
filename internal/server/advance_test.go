package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleAdvance_RejectsBotClaimedTask pins SKY-330's guard rule
// for the manual user transition: only user-claimed tasks may move
// through Claimed → In Progress → In Review via /advance. Bot-claimed
// tasks transition automatically via the spawner (see
// internal/delegate/spawner.go advanceTaskFromRunStatus); allowing
// the user to flip them by hand would race the run lifecycle.
func TestHandleAdvance_RejectsBotClaimedTask(t *testing.T) {
	s := newTestServer(t)
	taskID := seedAdvanceTask(t, s.db, "bot-claimed", advanceTaskOpts{
		claimedByAgentID: runmode.LocalDefaultAgentID,
	})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/advance",
		map[string]any{"to": "in_progress"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (bot-claimed task can't be manually advanced); body=%s",
			rec.Code, rec.Body.String())
	}
	// Task status should not have moved.
	got := readTaskStatus(t, s.db, taskID)
	if got != "queued" {
		t.Errorf("task.status = %q, want %q (refusal must not transition lifecycle)", got, "queued")
	}
}

// TestHandleAdvance_RejectsUnclaimedTask covers the second guard:
// /advance requires a user claim. An unclaimed (Queued column) task
// must be claimed first (via /swipe) before it can be advanced; the
// AdvanceStatusForUser store method enforces this with a WHERE
// clause on claimed_by_user_id.
func TestHandleAdvance_RejectsUnclaimedTask(t *testing.T) {
	s := newTestServer(t)
	taskID := seedAdvanceTask(t, s.db, "unclaimed", advanceTaskOpts{})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/advance",
		map[string]any{"to": "in_progress"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (unclaimed task); body=%s",
			rec.Code, rec.Body.String())
	}
	if got := readTaskStatus(t, s.db, taskID); got != "queued" {
		t.Errorf("task.status = %q, want %q", got, "queued")
	}
}

// TestHandleAdvance_RejectsTerminalTask covers the third guard: a
// task in 'done' or 'dismissed' is terminal and the lifecycle is
// frozen. /advance refuses with 409 instead of re-opening it.
func TestHandleAdvance_RejectsTerminalTask(t *testing.T) {
	s := newTestServer(t)
	taskID := seedAdvanceTask(t, s.db, "terminal", advanceTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
		status:          "done",
	})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/advance",
		map[string]any{"to": "in_progress"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (terminal task); body=%s",
			rec.Code, rec.Body.String())
	}
	if got := readTaskStatus(t, s.db, taskID); got != "done" {
		t.Errorf("task.status = %q, want %q (refusal must not change terminal status)", got, "done")
	}
}

// TestHandleAdvance_RejectsInvalidTarget pins the input validation:
// /advance only accepts in_progress and in_review. Done / dismissed
// have their own paths (swipe/complete), queued is requeue territory,
// and snoozed is via /snooze. Anything else is 400.
func TestHandleAdvance_RejectsInvalidTarget(t *testing.T) {
	s := newTestServer(t)
	taskID := seedAdvanceTask(t, s.db, "bad-target", advanceTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
	})

	for _, badTarget := range []string{"done", "dismissed", "queued", "snoozed", "wat"} {
		t.Run(badTarget, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/advance",
				map[string]any{"to": badTarget})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("to=%q: status = %d, want 400; body=%s",
					badTarget, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleAdvance_HappyPath_QueuedToInProgress is the canonical
// flow: a user-claimed task in 'queued' moves to 'in_progress'.
// Verifies the status flip lands and the task_updated WS broadcast
// fires (peer Board sessions need it to re-bucket the card).
func TestHandleAdvance_HappyPath_QueuedToInProgress(t *testing.T) {
	s := newTestServer(t)
	taskID := seedAdvanceTask(t, s.db, "happy-ip", advanceTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
	})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/advance",
		map[string]any{"to": "in_progress"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "in_progress" {
		t.Errorf("response status = %q, want %q", body["status"], "in_progress")
	}
	if got := readTaskStatus(t, s.db, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want %q", got, "in_progress")
	}
}

// TestHandleAdvance_HappyPath_InProgressToInReview is the second
// forward step. Backward (in_review → in_progress) is allowed by
// the same store method — caller responsibility to pick a sane
// next state — and is implicitly covered by the seed using status='in_progress'.
func TestHandleAdvance_HappyPath_InProgressToInReview(t *testing.T) {
	s := newTestServer(t)
	taskID := seedAdvanceTask(t, s.db, "happy-ir", advanceTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
		status:          "in_progress",
	})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/advance",
		map[string]any{"to": "in_review"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := readTaskStatus(t, s.db, taskID); got != "in_review" {
		t.Errorf("task.status = %q, want %q", got, "in_review")
	}
}

// TestHandleAdvance_404OnUnknownTask covers the not-found case.
// AdvanceStatusForUser returns ok=false for any missing-row query,
// so the handler can't distinguish "task not found" from "guard
// tripped" — both surface as 409. Documented here so future
// readers don't expect a separate 404 path.
func TestHandleAdvance_404OnUnknownTask(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost,
		"/api/tasks/00000000-0000-0000-0000-000000000bad/advance",
		map[string]any{"to": "in_progress"})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (guard-tripped vs not-found are merged); body=%s",
			rec.Code, rec.Body.String())
	}
}

// --- Fixture helpers ---

type advanceTaskOpts struct {
	status           string // defaults to "queued"
	claimedByUserID  string
	claimedByAgentID string
}

// seedAdvanceTask inserts a task row directly. Mirrors the
// pendingApprovalFixture pattern in requeue_test.go but without the
// run / review / memory machinery — /advance tests only touch the
// task row. Each call uses a unique suffix to dodge the dedup index.
func seedAdvanceTask(t *testing.T, database *sql.DB, suffix string, opts advanceTaskOpts) string {
	t.Helper()
	ctx := context.Background()

	const eventType = "github:pr:ci_check_passed"
	entityID := "e_adv_" + suffix
	eventID := "ev_adv_" + suffix
	taskID := "t_adv_" + suffix

	if _, err := database.ExecContext(ctx,
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES (?, 'github', ?, 'pr', 'active')`,
		entityID, "owner/repo#adv-"+suffix,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES (?, ?, ?, '')`,
		eventID, entityID, eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	status := opts.status
	if status == "" {
		status = "queued"
	}
	var agentClaim, userClaim any
	if opts.claimedByAgentID != "" {
		agentClaim = opts.claimedByAgentID
	}
	if opts.claimedByUserID != "" {
		userClaim = opts.claimedByUserID
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id,
		                    status, claimed_by_agent_id, claimed_by_user_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		taskID, entityID, eventType, eventID, status, agentClaim, userClaim,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

func readTaskStatus(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		t.Fatalf("scan task status: %v", err)
	}
	return status
}
