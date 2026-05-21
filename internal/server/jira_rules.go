package server

import (
	"context"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// lookupJiraRuleForTask resolves the per-team Jira status rule that
// governs the given task's source project. Returns nil when the task
// is not Jira-backed, when default-team lookup fails, or when the
// team has no configured rule for the ticket's project. Centralized
// so the swipe/claim/undo/requeue paths share one read shape (this
// replaces the per-call `cfg.Jira.RuleForProject` lookup that lived
// in the deleted internal/config package).
func (s *Server) lookupJiraRuleForTask(ctx context.Context, orgID string, task *domain.Task) *domain.JiraProjectStatusRules {
	if task == nil || task.EntitySource != "jira" {
		return nil
	}
	teamID, err := s.teams.GetDefaultForOrgSystem(ctx, orgID)
	if err != nil || teamID == "" {
		if err != nil {
			log.Printf("[jira-rule] resolve default team for org %s: %v", orgID, err)
		}
		return nil
	}
	rules, err := s.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		log.Printf("[jira-rule] list rules for team %s: %v", teamID, err)
		return nil
	}
	return domain.RuleForProject(rules, projectFromKey(task.EntitySourceID))
}

// containsStatus reports whether status is a member of the slice.
// Inlined so callers don't have to construct a domain rule just to
// run a membership check.
func containsStatus(members []string, status string) bool {
	for _, m := range members {
		if m == status {
			return true
		}
	}
	return false
}
