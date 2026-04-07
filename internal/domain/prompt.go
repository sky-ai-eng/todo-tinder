package domain

import "time"

// Prompt is a user- or system-defined delegation prompt template.
// The body contains the "mission" — what the agent should do.
// The system envelope (tool guidance, completion format, repo scoping) is always
// injected by the spawner and is not part of the prompt body.
type Prompt struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	Source     string    `json:"source"`      // "system", "user", "imported"
	UsageCount int       `json:"usage_count"` // how many agent runs have used this prompt
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// PromptBinding links a prompt to an event type. Multiple prompts can bind to
// the same event type; one is marked as the default for automatic delegation.
type PromptBinding struct {
	PromptID  string `json:"prompt_id"`
	EventType string `json:"event_type"` // FK to event_types.id, or a prefix like "github:" for broad matching
	IsDefault bool   `json:"is_default"` // only one default per event_type
}
