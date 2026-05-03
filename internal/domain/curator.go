package domain

import "time"

// CuratorRequest is one user→agent exchange with a project's Curator.
// One row per posted message: status flips from queued → running →
// terminal; cost / duration / num_turns are stamped at termination
// (mirrors what AgentRun does for delegated runs).
//
// The user's own input lives on UserInput here rather than as a row
// in curator_messages — that table holds only the agent's side of the
// exchange, same way run_messages does.
type CuratorRequest struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Status     string     `json:"status"` // queued | running | done | cancelled | failed
	UserInput  string     `json:"user_input"`
	ErrorMsg   string     `json:"error_msg,omitempty"`
	CostUSD    float64    `json:"cost_usd"`
	DurationMs int        `json:"duration_ms"`
	NumTurns   int        `json:"num_turns"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// IsTerminal reports whether the request has reached a final status.
// Used by the cancel endpoint to 404 when the in-flight slot is empty
// and by the per-project goroutine to skip already-finalized rows.
func (r *CuratorRequest) IsTerminal() bool {
	switch r.Status {
	case "done", "cancelled", "failed":
		return true
	}
	return false
}

// CuratorMessage mirrors run_messages but is keyed by request_id
// instead of run_id. The on-the-wire shape is otherwise identical so
// the frontend's existing message-rendering can be reused without
// branching on which table the row came from.
type CuratorMessage struct {
	ID                  int            `json:"id"`
	RequestID           string         `json:"request_id"`
	Role                string         `json:"role"`
	Subtype             string         `json:"subtype"`
	Content             string         `json:"content"`
	ToolCalls           []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID          string         `json:"tool_call_id,omitempty"`
	IsError             bool           `json:"is_error,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	Model               string         `json:"model,omitempty"`
	InputTokens         *int           `json:"input_tokens,omitempty"`
	OutputTokens        *int           `json:"output_tokens,omitempty"`
	CacheReadTokens     *int           `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int           `json:"cache_creation_tokens,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
}
