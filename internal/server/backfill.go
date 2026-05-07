package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// backfillCandidate is the per-row payload returned by
// GET /api/projects/{id}/backfill-candidates. The caller renders these
// as checkboxes in the create-flow popup; current_project_id +
// current_project_name surface so the user knows when they're
// reclaiming an entity from another project.
type backfillCandidate struct {
	ID                 string `json:"id"`
	Source             string `json:"source"`
	SourceID           string `json:"source_id"`
	Kind               string `json:"kind"`
	Title              string `json:"title"`
	URL                string `json:"url"`
	State              string `json:"state"`
	CurrentProjectID   string `json:"current_project_id"`
	CurrentProjectName string `json:"current_project_name"`
}

// handleBackfillCandidates returns the list of non-terminal entities
// that the create-flow popup should show for the given project.
//
// Scope rules:
//   - pinned_repos non-empty → GitHub entities scoped to those repos.
//   - pinned_repos empty → ALL GitHub entities (no filter on that source).
//   - jira_project_key non-empty → Jira entities matching that key.
//   - jira_project_key empty → ALL Jira entities (no filter on that source).
//   - Both empty → every non-terminal entity across sources.
//
// Empty filter == "no filter" rather than "exclude this source." A
// project that scopes only one tracker still wants to see candidates
// from the other source so the user can claim them manually.
//
// Entities already assigned to this project are excluded — there's
// nothing to backfill for them.
func (s *Server) handleBackfillCandidates(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	project, err := db.GetProject(s.db, projectID)
	if err != nil {
		log.Printf("[backfill] candidates: get project %s: %v", projectID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	pinnedSet := make(map[string]struct{}, len(project.PinnedRepos))
	for _, repo := range project.PinnedRepos {
		pinnedSet[repo] = struct{}{}
	}

	var collected []domain.Entity

	github, err := db.ListActiveEntities(s.db, "github")
	if err != nil {
		log.Printf("[backfill] candidates: list github entities: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load github entities"})
		return
	}
	for _, e := range github {
		if len(pinnedSet) > 0 {
			if _, ok := pinnedSet[githubRepoFromSourceID(e.SourceID)]; !ok {
				continue
			}
		}
		collected = append(collected, e)
	}

	jira, err := db.ListActiveEntities(s.db, "jira")
	if err != nil {
		log.Printf("[backfill] candidates: list jira entities: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load jira entities"})
		return
	}
	for _, e := range jira {
		if project.JiraProjectKey != "" {
			if jiraKeyFromSourceID(e.SourceID) != project.JiraProjectKey {
				continue
			}
		}
		collected = append(collected, e)
	}

	// Resolve current_project_name once per distinct project_id rather
	// than per row — the same other-project may sponsor many candidates.
	nameCache := map[string]string{}
	out := make([]backfillCandidate, 0, len(collected))
	for _, e := range collected {
		// Already in this project — no work to do; skip from candidates.
		if e.ProjectID != nil && *e.ProjectID == projectID {
			continue
		}
		c := backfillCandidate{
			ID:       e.ID,
			Source:   e.Source,
			SourceID: e.SourceID,
			Kind:     e.Kind,
			Title:    e.Title,
			URL:      e.URL,
			State:    e.State,
		}
		if e.ProjectID != nil && *e.ProjectID != "" {
			c.CurrentProjectID = *e.ProjectID
			name, ok := nameCache[*e.ProjectID]
			if !ok {
				if p, err := db.GetProject(s.db, *e.ProjectID); err == nil && p != nil {
					name = p.Name
				}
				nameCache[*e.ProjectID] = name
			}
			c.CurrentProjectName = name
		}
		out = append(out, c)
	}

	writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
}

type backfillRequest struct {
	EntityIDs []string `json:"entity_ids"`
}

type backfillFailure struct {
	EntityID string `json:"entity_id"`
	Error    string `json:"error"`
}

// handleBackfill bulk-assigns the named entities to the project. Reuses
// db.AssignEntityProject so each row gets its classified_at stamped —
// popup-claimed entities stay sticky against the auto-classifier.
//
// Partial-success result shape mirrors handleJiraStockPost
// (internal/server/stock.go): per-entity failures are collected into
// `failed: [{entity_id, error}]` and the call returns 200 with the
// applied count rather than failing the whole batch on a single row.
func (s *Server) handleBackfill(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	project, err := db.GetProject(s.db, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	var req backfillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if len(req.EntityIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"applied": 0, "failed": []backfillFailure{}})
		return
	}

	applied := 0
	var failures []backfillFailure
	var assigned []string
	for _, eid := range req.EntityIDs {
		eid = strings.TrimSpace(eid)
		if eid == "" {
			continue
		}
		// Empty rationale: this is a human-driven assignment, not a
		// model-driven one. The classifier's rationale (if any) was set
		// by the post-poll runner; we don't overwrite with an empty
		// string when reclaiming.
		if assignErr := db.AssignEntityProject(s.db, eid, &projectID, ""); assignErr != nil {
			if errors.Is(assignErr, sql.ErrNoRows) {
				failures = append(failures, backfillFailure{EntityID: eid, Error: "entity not found"})
			} else {
				failures = append(failures, backfillFailure{EntityID: eid, Error: assignErr.Error()})
			}
			continue
		}
		applied++
		assigned = append(assigned, eid)
	}

	if len(assigned) > 0 && s.ws != nil {
		s.ws.Broadcast(websocket.Event{
			Type:      "entities_assigned_to_project",
			ProjectID: projectID,
			Data:      map[string]any{"entity_ids": assigned},
		})
	}

	if failures == nil {
		failures = []backfillFailure{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied, "failed": failures})
}

// githubRepoFromSourceID extracts "owner/repo" from a GitHub entity's
// source_id, which is shaped "owner/repo#NNN".
func githubRepoFromSourceID(sourceID string) string {
	if idx := strings.LastIndex(sourceID, "#"); idx >= 0 {
		return sourceID[:idx]
	}
	return sourceID
}

// jiraKeyFromSourceID extracts the project key from a Jira entity's
// source_id, which is shaped "PROJ-123".
func jiraKeyFromSourceID(sourceID string) string {
	if idx := strings.Index(sourceID, "-"); idx >= 0 {
		return sourceID[:idx]
	}
	return sourceID
}
