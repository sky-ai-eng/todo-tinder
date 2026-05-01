package domain

// PendingReview is a locally-managed review that hasn't been submitted to GitHub yet.
// DiffLines stores a JSON map of file -> line numbers that are valid comment targets.
// When ReviewEvent is set, the review has been "submitted" locally but is awaiting
// user approval before posting to GitHub.
//
// OriginalReviewBody / OriginalReviewEvent are write-once snapshots of the
// agent's first draft, captured by SetPendingReviewSubmission's COALESCE
// pattern. They survive any user edits via handleReviewUpdate and are read
// by the human-verdict writer (SKY-205) to compose the post-run
// `## Human feedback (post-run)` block. Empty string means no snapshot exists
// (legacy rows pre-dating SKY-204 / SKY-205 columns); the writer degrades
// gracefully without trying to invent an original.
type PendingReview struct {
	ID                  string
	PRNumber            int
	Owner               string
	Repo                string
	CommitSHA           string
	DiffLines           string // JSON: {"file.go": [1,2,3,...], ...}
	RunID               string // agent run that created this review (empty for standalone CLI)
	ReviewBody          string // deferred review body (set when awaiting approval)
	ReviewEvent         string // deferred review event: APPROVE, COMMENT, REQUEST_CHANGES
	OriginalReviewBody  string // agent's first draft body, write-once
	OriginalReviewEvent string // agent's first draft event, write-once
}

// PendingReviewComment is a comment attached to a local pending review.
//
// OriginalBody is the write-once snapshot of the agent's drafted comment
// body, populated at INSERT in AddPendingReviewComment. UpdatePendingReviewComment
// mutates Body but never OriginalBody, giving SKY-205's writer a stable
// before/after pair for diff formatting. Empty string on legacy rows that
// pre-date SKY-204.
type PendingReviewComment struct {
	ID           string
	ReviewID     string
	Path         string
	Line         int
	StartLine    *int
	Body         string
	OriginalBody string
}
