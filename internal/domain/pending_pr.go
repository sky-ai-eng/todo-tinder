package domain

import "time"

// PendingPR is a locally-queued pull request the agent has drafted but
// not yet opened on GitHub. Mirrors the shape of PendingReview: agent
// drafts → queues here → user reviews/edits → user approves → server
// actually opens the PR.
//
// OriginalTitle / OriginalBody are write-once snapshots of the agent's
// first draft, captured by UpdatePendingPRTitleBody's COALESCE pattern.
// They survive any user edits and are read by the human-verdict writer
// to compose the post-submit `## Human feedback (post-run)` block —
// same contract as PendingReview's originals.
//
// Pointer (rather than string + sentinel) so the formatter can tell
// apart "no snapshot exists" (nil — legacy row from before the
// columns were added) from "snapshot was a legitimate empty value"
// (non-nil pointer to ""). The body case matters because PR bodies
// are commonly empty when the title alone explains the change.
//
// Locked is the agent-side anti-retry sentinel. Set true by
// LockPendingPR's `WHERE locked = 0` gate. Cleaner than reviews'
// accidental "review_event != ”" sentinel — there's no era-dependent
// migration drift to navigate.
//
// SubmittedAt is the concurrent-submit guard. Two browser tabs
// clicking Submit can't both call CreatePR on GitHub; the
// `UPDATE ... WHERE submitted_at IS NULL` only matches once. Reset to
// nil on submit failure so the user can retry.
type PendingPR struct {
	ID            string
	RunID         string
	Owner         string
	Repo          string
	HeadBranch    string
	HeadSHA       string // captured at queue time so the UI can flag drift
	BaseBranch    string
	Title         string
	Body          string
	OriginalTitle *string // agent's first-draft title, write-once; nil = no snapshot
	OriginalBody  *string // agent's first-draft body, write-once; nil = no snapshot
	Draft         bool    // agent's queue-time --draft hint; user can override at approval time
	Locked        bool
	SubmittedAt   *time.Time
	CreatedAt     time.Time
}
