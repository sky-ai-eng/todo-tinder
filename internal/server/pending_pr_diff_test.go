package server

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Reuses strPtr from review_diff_test.go (same package).

// --- truncateDiffAtFileBoundary ---

func TestTruncateDiffAtFileBoundary_UnderCap(t *testing.T) {
	in := []byte("diff --git a/foo b/foo\n@@ -1 +1 @@\n-old\n+new\n")
	out, note := truncateDiffAtFileBoundary(in, 4096)
	if out != string(in) {
		t.Errorf("under-cap: expected pass-through; got %q", out)
	}
	if note != "" {
		t.Errorf("under-cap: expected empty note; got %q", note)
	}
}

// TestTruncateDiffAtFileBoundary_FileBoundaryCut covers the common
// case: the cap lands somewhere inside file B, but file A's complete
// block fits. The truncated text must end at file A's boundary so
// parseDiff doesn't see a half-cut hunk for file B.
func TestTruncateDiffAtFileBoundary_FileBoundaryCut(t *testing.T) {
	fileA := "diff --git a/a b/a\n@@ -1 +1 @@\n-x\n+y\n"
	fileB := "diff --git a/b b/b\n@@ -1,5 +1,5 @@\n" + strings.Repeat("-line\n+line\n", 100)
	in := []byte(fileA + "\n" + fileB)
	// Cap small enough to land inside fileB's hunk but past fileA.
	cap := len(fileA) + 50

	out, note := truncateDiffAtFileBoundary(in, cap)

	if note == "" {
		t.Fatalf("expected truncation note; got empty")
	}
	if !strings.HasPrefix(out, fileA) {
		t.Errorf("expected output to start with fileA; got %q", out[:min(len(out), 80)])
	}
	if strings.Contains(out, "diff --git a/b b/b") {
		t.Errorf("expected fileB to be cut entirely; output contained its header")
	}
	// Ensure no stray partial hunk header from fileB landed.
	if strings.Contains(out, "@@ -1,5 +1,5 @@") {
		t.Errorf("expected fileB's hunk header to be omitted; output:\n%s", out)
	}
}

// TestTruncateDiffAtFileBoundary_FirstFileTooBig is the case the
// X-Diff-Truncated banner is most important for: the very first
// file's diff exceeds the cap, so there's no earlier "diff --git "
// boundary to fall back to. truncateDiff returns empty + a note;
// the handler stamps the note into the header so the overlay can
// render a banner instead of a (misleading) "No diff available."
func TestTruncateDiffAtFileBoundary_FirstFileTooBig(t *testing.T) {
	huge := []byte("diff --git a/big b/big\n@@ -1,1000 +1,1000 @@\n" +
		strings.Repeat("-line\n+line\n", 10000))
	out, note := truncateDiffAtFileBoundary(huge, 1024)
	if out != "" {
		t.Errorf("expected empty output for first-file-too-big; got %d bytes", len(out))
	}
	if note == "" {
		t.Fatal("expected truncation note; got empty")
	}
	if !strings.Contains(note, "first file") {
		t.Errorf("expected note to mention 'first file'; got %q", note)
	}
}

// TestTruncateDiffAtFileBoundary_ExactlyAtCap pins the boundary
// behavior: a buffer at exactly maxBytes passes through unchanged.
// Off-by-one in the comparison would either truncate cleanly-fitting
// diffs or fail to truncate ones that don't fit.
func TestTruncateDiffAtFileBoundary_ExactlyAtCap(t *testing.T) {
	in := []byte(strings.Repeat("x", 1024))
	out, note := truncateDiffAtFileBoundary(in, 1024)
	if out != string(in) {
		t.Errorf("expected exact-fit pass-through; got %d bytes", len(out))
	}
	if note != "" {
		t.Errorf("expected empty note at exact cap; got %q", note)
	}
}

// --- FormatHumanFeedbackPR ---

// TestFormatHumanFeedbackPR_NoEdits is the no-edit collapse case:
// the human submitted the agent's draft verbatim. Stored shape is
// just the outcome line — the materialization layer prepends the
// "## Human feedback (post-run)" heading.
func TestFormatHumanFeedbackPR_NoEdits(t *testing.T) {
	pr := &domain.PendingPR{
		OriginalTitle: strPtr("feat: thing"),
		OriginalBody:  strPtr("Body."),
		Title:         "feat: thing",
		Body:          "Body.",
	}
	got := FormatHumanFeedbackPR(pr, "feat: thing", "Body.")
	if strings.Contains(got, "## Human feedback (post-run)") {
		t.Errorf("formatter must NOT emit the heading (materializer prepends it); got:\n%s", got)
	}
	if !strings.Contains(got, "submitted the PR as drafted") {
		t.Errorf("expected 'as drafted' outcome; got:\n%s", got)
	}
	if strings.Contains(got, "Title:") || strings.Contains(got, "Body:") {
		t.Errorf("no-edits should not emit per-field detail; got:\n%s", got)
	}
}

// TestFormatHumanFeedbackPR_TitleOnly covers the "human renamed the
// PR but kept the body" case. Body section should be absent.
func TestFormatHumanFeedbackPR_TitleOnly(t *testing.T) {
	pr := &domain.PendingPR{
		OriginalTitle: strPtr("feat: thing"),
		OriginalBody:  strPtr("Body."),
	}
	got := FormatHumanFeedbackPR(pr, "feat(scope): thing", "Body.")
	if !strings.Contains(got, "with edits") {
		t.Errorf("expected 'with edits' outcome; got:\n%s", got)
	}
	if !strings.Contains(got, "**Title:** edited") {
		t.Errorf("expected title-edited line; got:\n%s", got)
	}
	if !strings.Contains(got, "Was: feat: thing") || !strings.Contains(got, "Now: feat(scope): thing") {
		t.Errorf("expected was/now lines; got:\n%s", got)
	}
	if strings.Contains(got, "**Body:**") {
		t.Errorf("body unchanged: should not emit Body section; got:\n%s", got)
	}
}

// TestFormatHumanFeedbackPR_BodyOnly is the inverse: title kept,
// body rewritten. Title section absent, body diff present as
// blockquoted before/after.
func TestFormatHumanFeedbackPR_BodyOnly(t *testing.T) {
	pr := &domain.PendingPR{
		OriginalTitle: strPtr("feat: thing"),
		OriginalBody:  strPtr("Original body."),
	}
	got := FormatHumanFeedbackPR(pr, "feat: thing", "Reworded body.")
	if !strings.Contains(got, "with edits") {
		t.Errorf("expected 'with edits' outcome; got:\n%s", got)
	}
	if strings.Contains(got, "**Title:**") {
		t.Errorf("title unchanged: should not emit Title section; got:\n%s", got)
	}
	if !strings.Contains(got, "**Body:** edited") {
		t.Errorf("expected Body-edited section; got:\n%s", got)
	}
	if !strings.Contains(got, "Original body.") || !strings.Contains(got, "Reworded body.") {
		t.Errorf("expected both versions blockquoted; got:\n%s", got)
	}
}

// TestFormatHumanFeedbackPR_BothEdited verifies title and body
// sections compose without interfering.
func TestFormatHumanFeedbackPR_BothEdited(t *testing.T) {
	pr := &domain.PendingPR{
		OriginalTitle: strPtr("WIP"),
		OriginalBody:  strPtr("draft."),
	}
	got := FormatHumanFeedbackPR(pr, "feat: ship it", "Polished body.")
	if !strings.Contains(got, "with edits") {
		t.Errorf("expected 'with edits'; got:\n%s", got)
	}
	if !strings.Contains(got, "**Title:** edited") {
		t.Errorf("expected Title section; got:\n%s", got)
	}
	if !strings.Contains(got, "**Body:** edited") {
		t.Errorf("expected Body section; got:\n%s", got)
	}
}

// TestFormatHumanFeedbackPR_LegacyOriginalsNil pins the legacy-row
// behavior: when OriginalTitle/OriginalBody are nil (pre-snapshot
// rows), the formatter must NOT synthesize a "no change" claim — it
// has no baseline to compare against. Outcome falls to "as drafted"
// and per-field detail is omitted.
func TestFormatHumanFeedbackPR_LegacyOriginalsNil(t *testing.T) {
	pr := &domain.PendingPR{
		OriginalTitle: nil,
		OriginalBody:  nil,
	}
	got := FormatHumanFeedbackPR(pr, "feat: x", "Body.")
	if !strings.Contains(got, "as drafted") {
		t.Errorf("legacy-nil-originals: expected 'as drafted' outcome (no baseline); got:\n%s", got)
	}
	if strings.Contains(got, "**Title:**") || strings.Contains(got, "**Body:**") {
		t.Errorf("legacy-nil-originals: must omit per-field detail; got:\n%s", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
