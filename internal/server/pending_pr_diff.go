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
// Wrapped in worktree.WithRepoLock because curator refresh,
// bootstrap, and worktree creation all mutate the same bare and
// concurrent fetches can fail on ref locks. Without the lock,
// opening the pending-PR overlay during a curator cycle racing on
// the same repo intermittently 502s.
//
// Errors fall into two buckets:
//   - the bare doesn't exist (repo not configured) → caller's 502
//   - the fetch fails (network, auth, branch missing on upstream)
//   - the diff itself fails (rare; git diff doesn't normally fail
//     after both refs are present)
//
// Returns (diff, truncationNote, err). truncationNote is non-empty
// when the diff was capped at livePRDiffMaxBytes — the handler
// surfaces it via an X-Diff-Truncated header so the overlay can
// render a banner above the file list (parseDiff alone would just
// drop the marker and leave the user thinking the PR is empty).
func livePRDiff(ctx context.Context, owner, repo, baseBranch, headBranch string) (string, string, error) {
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		return "", "", fmt.Errorf("resolve bare dir: %w", err)
	}

	var diffOut []byte
	lockErr := worktree.WithRepoLock(owner, repo, func() error {
		// Fetch the head branch from origin into the bare's local ref so
		// `git diff <base>...<head>` can resolve. Force update via "+" so
		// a force-pushed head ref still wins. Same pattern
		// EnsureCuratorWorktree uses.
		headRefspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", headBranch, headBranch)
		if err := gitCtx(ctx, bareDir, "fetch", "origin", headRefspec); err != nil {
			return fmt.Errorf("fetch %s from origin: %w", headBranch, err)
		}
		// Same for the base, in case it's drifted since the agent's run.
		baseRefspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", baseBranch, baseBranch)
		if err := gitCtx(ctx, bareDir, "fetch", "origin", baseRefspec); err != nil {
			return fmt.Errorf("fetch %s from origin: %w", baseBranch, err)
		}

		// Use the three-dot form (base...head) — show what's on head that
		// isn't on base, which is what GitHub's PR diff shows. The
		// two-dot form would also show unrelated changes on base since
		// the merge base, which isn't what users expect.
		out, err := gitCtxOutput(ctx, bareDir, "diff",
			"refs/remotes/origin/"+baseBranch+"..."+"refs/remotes/origin/"+headBranch)
		if err != nil {
			return fmt.Errorf("git diff: %w", err)
		}
		diffOut = out
		return nil
	})
	if lockErr != nil {
		return "", "", lockErr
	}

	out, note := truncateDiffAtFileBoundary(diffOut, livePRDiffMaxBytes)
	return out, note, nil
}

// truncateDiffAtFileBoundary caps a unified-diff payload at maxBytes
// without leaving parseDiff with a half-cut hunk. react-diff-view's
// parser is unforgiving — a "@@ -1,5 +1,5 @@" header followed by 3
// of 5 expected lines fails the parse and the overlay goes blank,
// exactly the multi-MB case the cap is meant to handle.
//
// Returns (text, truncationNote). truncationNote is empty when
// no truncation happened, otherwise human-readable text the handler
// stamps into an X-Diff-Truncated response header.
//
// Three cases:
//   - len(buf) <= maxBytes: return as-is, no note.
//   - len(buf) > maxBytes and a "\ndiff --git " boundary exists in
//     buf[:maxBytes]: cut at that boundary so only intact per-file
//     blocks survive.
//   - len(buf) > maxBytes but no boundary in the window (the first
//     file alone overruns): return empty text. parseDiff will see no
//     blocks but won't crash, and the handler's X-Diff-Truncated
//     header tells the overlay to render a "too big to show" banner
//     instead of "No diff available."
func truncateDiffAtFileBoundary(buf []byte, maxBytes int) (string, string) {
	if len(buf) <= maxBytes {
		return string(buf), ""
	}
	cut := bytes.LastIndex(buf[:maxBytes], []byte("\ndiff --git "))
	if cut <= 0 {
		return "", "first file alone exceeds " + humanBytes(maxBytes) + " — diff hidden to avoid crashing the overlay"
	}
	// +1 to keep the leading newline of the boundary as part of the
	// preserved content so the diff stays well-formed.
	return string(buf[:cut+1]), "diff truncated at " + humanBytes(maxBytes) + "; later files omitted"
}

// gitCtx runs git in `dir` and discards stdout. Used for fetches
// where we only care about success/failure. The error includes the
// full argv and the working dir so server-side log lines identify
// exactly which invocation failed — diagnosing "diff failed" 502s
// without that context means re-running by hand to figure out
// which step broke.
func gitCtx(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s (in %s): %w: %s",
			strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gitCtxOutput runs git in `dir` and returns stdout. Used for
// commands whose output we care about (e.g. diff). Same argv +
// dir context as gitCtx so log lines stay actionable.
func gitCtxOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s (in %s): %w: %s",
			strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
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
//
// No leading "## Human feedback (post-run)" heading: db.materializeMemory
// prepends it via humanFeedbackHeader when joining agent_content
// + human_content, so baking it in here would double the heading
// in the agent-readable file. Mirrors FormatHumanFeedback in
// review_diff.go.
func FormatHumanFeedbackPR(pr *domain.PendingPR, finalTitle, finalBody string) string {
	var b strings.Builder

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
