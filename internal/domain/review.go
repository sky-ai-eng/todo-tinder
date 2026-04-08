package domain

// PendingReview is a locally-managed review that hasn't been submitted to GitHub yet.
// DiffLines stores a JSON map of file -> line numbers that are valid comment targets.
// When ReviewEvent is set, the review has been "submitted" locally but is awaiting
// user approval before posting to GitHub.
type PendingReview struct {
	ID          string
	PRNumber    int
	Owner       string
	Repo        string
	CommitSHA   string
	DiffLines   string // JSON: {"file.go": [1,2,3,...], ...}
	RunID       string // agent run that created this review (empty for standalone CLI)
	ReviewBody  string // deferred review body (set when awaiting approval)
	ReviewEvent string // deferred review event: APPROVE, COMMENT, REQUEST_CHANGES
}

// PendingReviewComment is a comment attached to a local pending review.
type PendingReviewComment struct {
	ID        string
	ReviewID  string
	Path      string
	Line      int
	StartLine *int
	Body      string
}
