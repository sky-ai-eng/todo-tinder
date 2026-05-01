package server

import (
	"encoding/json"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// factoryDelegateRequest is the body for POST /api/factory/delegate.
// All four fields are required; dedup_key may be empty for non-
// discriminator event types (the common case).
type factoryDelegateRequest struct {
	EntityID  string `json:"entity_id"`
	EventType string `json:"event_type"`
	DedupKey  string `json:"dedup_key"`
	PromptID  string `json:"prompt_id"`
}

type factoryDelegateResponse struct {
	TaskID string `json:"task_id"`
	RunID  string `json:"run_id"`
}

// handleFactoryDelegate is the drag-to-delegate endpoint behind the
// station drawer's drop-on-runs gesture. Find-or-create on the task
// keeps the UX uniform: every queued chip is draggable, and dropping
// either reuses the existing task at this station or synthesizes a
// new one anchored on the most recent matching event.
//
// Race-safe via the partial unique index on
// (entity_id, event_type, dedup_key) WHERE status NOT IN ('done',
// 'dismissed') — concurrent drops resolve to the same task.
func (s *Server) handleFactoryDelegate(w http.ResponseWriter, r *http.Request) {
	var req factoryDelegateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.EntityID == "" || req.EventType == "" || req.PromptID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity_id, event_type, and prompt_id are required"})
		return
	}
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "spawner not configured"})
		return
	}

	// Entity must exist — guard against stale snapshot keys.
	entity, err := db.GetEntity(s.db, req.EntityID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if entity == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
		return
	}

	// Anchor the (possibly synthesized) task on the most recent event
	// of this type for the entity. If none exists, the entity isn't
	// actually at this station — refuse rather than fabricate an
	// event row.
	primaryEvent, err := db.LatestEventForEntityAndType(s.db, req.EntityID, req.EventType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if primaryEvent == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no matching event for entity at this station"})
		return
	}

	// Default priority — mirrors internal/routing/router.go:210-215.
	// Use the highest enabled task_rule's default_priority for this
	// event type, or 0.5 if no rules match.
	defaultPriority := 0.5
	rules, err := db.GetEnabledRulesForEvent(s.db, req.EventType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, rule := range rules {
		if rule.DefaultPriority > defaultPriority {
			defaultPriority = rule.DefaultPriority
		}
	}

	task, _, err := db.FindOrCreateTask(s.db, req.EntityID, req.EventType, req.DedupKey, primaryEvent.ID, defaultPriority)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	runID, err := s.spawner.Delegate(*task, req.PromptID, "manual", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, factoryDelegateResponse{
		TaskID: task.ID,
		RunID:  runID,
	})
}
