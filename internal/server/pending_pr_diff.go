package server

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// livePRDiffMaxBytes caps the diff payload returned to the frontend.
// react-diff-view's parseDiff is in-memory and slow on multi-MB
// diffs; capping here prevents a hostile-or-monstrous PR from
// jamming the browser. The cap matches the rough order-of-magnitude
// of what GitHub's PR diff endpoint serves before its own truncation.
const livePRDiffMaxBytes = 4 * 1024 * 1024

// livePRDiff computes the unified diff between baseBranch and
// headBranch for owner/repo using the bare clone we already
// maintain at ~/.triagefactory/repos/{owner}/{repo}.git/.
//
// The agent's `git push` updates the upstream's branch but the
// bare's local refs/heads/<head> doesn't auto-update — git push only
// affects the remote. Before we can ask `git diff` for the
// comparison, we have to fetch the head from origin into the bare's
// own ref so the symbolic comparison resolves.
//
// Errors fall into two buckets:
//   - the bare doesn't exist (repo not configured) → caller's 502
//   - the fetch fails (network, auth, branch missing on upstream)
//   - the diff itself fails (rare; git diff doesn't normally fail
//     after both refs are present)
//
// Output is capped at livePRDiffMaxBytes with a trailing marker so
// the agent / user can tell when truncation happened.
func livePRDiff(ctx context.Context, owner, repo, baseBranch, headBranch string) (string, error) {
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve bare dir: %w", err)
	}

	// Fetch the head branch from origin into the bare's local ref so
	// `git diff <base>...<head>` can resolve. Force update via "+" so
	// a force-pushed head ref still wins. Same pattern
	// EnsureCuratorWorktree uses.
	headRefspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", headBranch, headBranch)
	if err := gitCtx(ctx, bareDir, "fetch", "origin", headRefspec); err != nil {
		return "", fmt.Errorf("fetch %s from origin: %w", headBranch, err)
	}
	// Same for the base, in case it's drifted since the agent's run.
	baseRefspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", baseBranch, baseBranch)
	if err := gitCtx(ctx, bareDir, "fetch", "origin", baseRefspec); err != nil {
		return "", fmt.Errorf("fetch %s from origin: %w", baseBranch, err)
	}

	// Use the three-dot form (base...head) — show what's on head that
	// isn't on base, which is what GitHub's PR diff shows. The
	// two-dot form would also show unrelated changes on base since
	// the merge base, which isn't what users expect.
	diffOut, err := gitCtxOutput(ctx, bareDir, "diff",
		"refs/remotes/origin/"+baseBranch+"..."+"refs/remotes/origin/"+headBranch)
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}

	if len(diffOut) > livePRDiffMaxBytes {
		// Truncate at the cap and append a marker on its own line so
		// the frontend's parseDiff sees a complete final hunk and
		// the user sees the marker alongside the content.
		return string(diffOut[:livePRDiffMaxBytes]) + "\n\n... [diff truncated at " + humanBytes(livePRDiffMaxBytes) + "]\n", nil
	}
	return string(diffOut), nil
}

// gitCtx runs git in `dir` and discards stdout. Used for fetches
// where we only care about success/failure.
func gitCtx(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gitCtxOutput runs git in `dir` and returns stdout. Used for
// commands whose output we care about (e.g. diff).
func gitCtxOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// humanBytes formats a byte count as e.g. "4.0 MB". Cap-marker only
// — not used elsewhere — so we keep it tiny rather than pulling in
// a pretty-print dependency.
func humanBytes(n int) string {
	const (
		KB = 1024
		MB = KB * 1024
	)
	switch {
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// FormatHumanFeedbackPR builds the markdown block that lands in
// run_memory.human_content after the user approves a pending PR.
// Mirror of FormatHumanFeedback in review_diff.go but for the
// simpler PR shape: title and body only, no per-line comments.
//
// finalTitle / finalBody are the values at submit time (potentially
// edited by the user); pr.OriginalTitle / pr.OriginalBody are the
// agent's first draft (write-once snapshots from CreatePendingPR).
//
// Outcome flags:
//   - "submitted as drafted" — neither title nor body changed
//   - "submitted with edits" — at least one changed
//
// Per-field detail follows: "Title: edited from X to Y" / "Body:
// edited (blockquoted before/after)". When OriginalTitle /
// OriginalBody are nil (legacy rows from before the columns
// existed) we omit the per-field detail rather than synthesizing a
// false "no change" — the formatter has to know the difference.
func FormatHumanFeedbackPR(pr *domain.PendingPR, finalTitle, finalBody string) string {
	var b strings.Builder
	b.WriteString("## Human feedback (post-run)\n\n")

	titleChanged := pr.OriginalTitle != nil && *pr.OriginalTitle != finalTitle
	bodyChanged := pr.OriginalBody != nil && *pr.OriginalBody != finalBody

	if titleChanged || bodyChanged {
		b.WriteString("**Outcome:** Human submitted the PR with edits.\n\n")
	} else {
		b.WriteString("**Outcome:** Human submitted the PR as drafted.\n\n")
	}

	if titleChanged {
		fmt.Fprintf(&b, "**Title:** edited\n- Was: %s\n- Now: %s\n\n",
			*pr.OriginalTitle, finalTitle)
	}

	if bodyChanged {
		b.WriteString("**Body:** edited\n\n")
		b.WriteString("Originally drafted as:\n\n")
		writeBlockquote(&b, *pr.OriginalBody)
		b.WriteString("\nFinal:\n\n")
		writeBlockquote(&b, finalBody)
	}

	return b.String()
}
