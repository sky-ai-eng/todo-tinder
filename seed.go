package main

import (
	"database/sql"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func seedDefaultPrompts(database *sql.DB) {
	// Default PR review prompt — manual only (user chooses when to review)
	err := db.SeedPrompt(database, domain.Prompt{
		ID:     "system-pr-review",
		Name:   "PR Code Review",
		Body:   ai.PRReviewPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed PR review prompt: %v", err)
	}

	// Merge conflict resolution prompt — auto-fired on merge conflicts on
	// the user's own PRs via the matching trigger below.
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-conflict-resolution",
		Name:   "Merge Conflict Resolution",
		Body:   ai.ConflictResolutionPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed conflict resolution prompt: %v", err)
	}

	// CI fix prompt — auto-fired on CI failures via prompt_trigger.
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-ci-fix",
		Name:   "CI Fix",
		Body:   ai.CIFixPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed CI fix prompt: %v", err)
	}

	// Jira implementation prompt — auto-fired on issues assigned to the
	// user via the matching trigger below.
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-jira-implement",
		Name:   "Jira Issue Implementation",
		Body:   ai.JiraImplementPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed Jira implement prompt: %v", err)
	}

	// --- Self-review loop prompts ------------------------------------------
	// Three prompts that together form the flagship self-review flow: on
	// each new push to a draft PR, review it; on receiving the self-review,
	// address the comments; on receiving an external reviewer's feedback
	// after manually going ready-for-review, respond to it. All shipped
	// disabled by default — users opt in per trigger.

	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-self-review-draft-pr",
		Name:   "Self-Review Draft PR",
		Body:   ai.SelfReviewDraftPRPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed self-review draft PR prompt: %v", err)
	}

	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-address-self-review",
		Name:   "Address Self-Review Comments",
		Body:   ai.AddressSelfReviewPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed address self-review prompt: %v", err)
	}

	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-respond-to-external-review",
		Name:   "Respond to External Review",
		Body:   ai.RespondToExternalReviewPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed respond-to-external-review prompt: %v", err)
	}

	// --- Default triggers --------------------------------------------------
	// All shipped disabled. System triggers are reference examples users
	// opt into (or disable and replace with their own variations). Keep
	// predicates conservative — cooldowns and breaker thresholds tuned for
	// "probably safe to leave on" rather than "aggressive automation".

	authorIsSelf := `{"author_is_self":true}`
	assigneeIsSelf := `{"assignee_is_self":true}`
	// Self-review trigger: only fire on draft PRs authored by the user.
	// is_draft=true filters out PRs already in external review.
	selfReviewDraft := `{"author_is_self":true,"is_draft":true}`
	// Address self-review: fires when the reviewer IS the user — the
	// self-review prompt above posts a commented review, landing here.
	// is_draft=true keeps the loop scoped to pre-human-review iterations.
	addressSelfReview := `{"reviewer_is_self":true,"is_draft":true}`
	// External-review response: reviewer is NOT the user, and the PR has
	// been manually marked ready (is_draft=false). Covers changes_requested
	// — a real ask to modify the PR. Commented-only reviews from externals
	// are left for user judgment since they're often non-blocking chatter.
	externalChangesRequested := `{"reviewer_is_self":false,"is_draft":false}`

	// Trigger: CI fix on own PRs.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-ci-fix",
		PromptID:           "system-ci-fix",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRCICheckFailed,
		ScopePredicateJSON: &authorIsSelf,
		BreakerThreshold:   3,
		CooldownSeconds:    60,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed CI fix trigger: %v", err)
	}

	// Trigger: merge conflict resolution on own PRs.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-conflict-resolution",
		PromptID:           "system-conflict-resolution",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRConflicts,
		ScopePredicateJSON: &authorIsSelf,
		BreakerThreshold:   2,
		CooldownSeconds:    300,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed conflict resolution trigger: %v", err)
	}

	// Trigger: Jira issue implementation on tickets assigned to the user.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-jira-implement",
		PromptID:           "system-jira-implement",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventJiraIssueAssigned,
		ScopePredicateJSON: &assigneeIsSelf,
		BreakerThreshold:   2,
		CooldownSeconds:    600,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed Jira implement trigger: %v", err)
	}

	// Trigger: self-review on each new push to an own draft PR. Cooldown
	// is generous so rapid push-fix-push cycles don't stack runs.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-self-review-draft-pr",
		PromptID:           "system-self-review-draft-pr",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRNewCommits,
		ScopePredicateJSON: &selfReviewDraft,
		BreakerThreshold:   3,
		CooldownSeconds:    300,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed self-review draft PR trigger: %v", err)
	}

	// Trigger: address self-review comments when the reviewer is the user.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-address-self-review",
		PromptID:           "system-address-self-review",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRReviewCommented,
		ScopePredicateJSON: &addressSelfReview,
		BreakerThreshold:   3,
		CooldownSeconds:    180,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed address self-review trigger: %v", err)
	}

	// Trigger: respond to external reviewers requesting changes on a
	// non-draft PR. Higher cooldown — real reviews rarely land back-to-back.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-respond-to-external-review",
		PromptID:           "system-respond-to-external-review",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRReviewChangesRequested,
		ScopePredicateJSON: &externalChangesRequested,
		BreakerThreshold:   2,
		CooldownSeconds:    600,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed respond-to-external-review trigger: %v", err)
	}
}
