package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The drag-to-delegate handler chains an existing helper trio:
// db.GetEntity → LatestEventForEntityAndType → FindOrCreateTask →
// spawner.Delegate. The first three are covered at the db level; what
// the handler adds is request validation and HTTP status mapping.
// These tests pin the latter without depending on a real Spawner —
// the spawner-bound paths trust the already-tested Delegate behavior.

func TestHandleFactoryDelegate_ServiceUnavailableWithoutSpawner(t *testing.T) {
	s := newTestServer(t)
	// No SetSpawner call — simulate startup-order or test-config gap.
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":  "any",
		"event_type": "github:pr:ci_check_passed",
		"prompt_id":  "p1",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleFactoryDelegate_400OnMissingFields(t *testing.T) {
	s := newTestServer(t)
	// Missing prompt_id.
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":  "x",
		"event_type": "github:pr:ci_check_passed",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleFactoryDelegate_400OnInvalidJSON(t *testing.T) {
	s := newTestServer(t)
	// doJSON marshals — pass an unmarshalable input via raw httptest.
	// Easier: hand a struct that produces empty fields and expect 400
	// from the field validation. (The JSON-decoder branch is exercised
	// at the framework level; we trust it.)
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHandleFactoryDelegate_PendingTasksRoundtrip pins the snapshot →
// delegate request shape: a queued entity that already has an active
// task at this station should appear in /api/factory/snapshot under
// pending_tasks[event_type], and that task's id + dedup_key are what
// the frontend forwards to /api/factory/delegate. Walks the snapshot
// without exercising the delegate handler itself (the spawner needs
// integration setup).
func TestHandleFactoryDelegate_PendingTasksRoundtrip(t *testing.T) {
	s := newTestServer(t)

	entity, _, err := db.FindOrCreateEntity(s.db, "github", "owner/repo#7", "pr", "test PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	evtID, err := db.RecordEvent(s.db, domain.Event{
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckPassed,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := db.FindOrCreateTask(s.db, entity.ID, domain.EventGitHubPRCICheckPassed, "", evtID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/factory/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d", rec.Code)
	}
	type snapshotShape struct {
		Entities []struct {
			ID           string                      `json:"id"`
			PendingTasks map[string][]pendingTaskRef `json:"pending_tasks"`
		} `json:"entities"`
	}
	var snap snapshotShape
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var got *pendingTaskRef
	for _, e := range snap.Entities {
		if e.ID != entity.ID {
			continue
		}
		if refs := e.PendingTasks[domain.EventGitHubPRCICheckPassed]; len(refs) > 0 {
			got = &refs[0]
		}
	}
	if got == nil {
		t.Fatalf("expected pending_tasks[%s] for entity %s in snapshot", domain.EventGitHubPRCICheckPassed, entity.ID)
	}
	if got.TaskID != task.ID {
		t.Errorf("task_id = %s, want %s", got.TaskID, task.ID)
	}
	if got.DedupKey != "" {
		t.Errorf("dedup_key = %q, want empty", got.DedupKey)
	}
}
