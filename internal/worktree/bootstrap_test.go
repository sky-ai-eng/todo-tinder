package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestUpstream creates a minimal bare git repository at a tempdir
// path that EnsureBareClone can use as an "origin" URL. The bare gets
// one commit on main so there's a ref to fetch.
func makeTestUpstream(t *testing.T) string {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", upstream).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}

	// Build a commit elsewhere and push it so the bare has a ref.
	work := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "Test"},
		{"-C", work, "commit", "--allow-empty", "-m", "initial"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "push", "origin", "main"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	return upstream
}

// withTestHome points $HOME at a tempdir for the duration of the test
// so repoDir() returns paths under it instead of touching the user's
// real ~/.triagefactory. Also overrides $TMPDIR so worktrees created
// via runDir() land under a tempdir that t.TempDir() will auto-clean.
// os.TempDir() honors $TMPDIR on Unix, so this isolation works without
// any worktree-package changes.
func withTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	return home
}

// readFetchRefspecs returns every value of remote.origin.fetch in the
// bare. Tolerates the "key not found" exit-1 from `git config
// --get-all` — `git clone --bare` doesn't configure a fetch refspec
// at all (just remote.origin.url), so a freshly-cloned bare with no
// post-processing returns nothing. That's a valid state we still want
// to inspect from tests.
func readFetchRefspecs(t *testing.T, bareDir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", bareDir, "config", "--get-all", "remote.origin.fetch").Output()
	if err != nil {
		// Exit 1 when the key has zero values is fine; surface other failures.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil
		}
		t.Fatalf("read fetch refspecs: %v", err)
	}
	var refs []string
	for _, line := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			refs = append(refs, s)
		}
	}
	return refs
}

func TestEnsureBareClone_FreshCloneSetsPRRefspec(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	bareDir, err := EnsureBareClone(context.Background(), "owner1", "repo1", upstream)
	if err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}
	if _, err := os.Stat(bareDir); err != nil {
		t.Fatalf("bare dir not created: %v", err)
	}

	refspecs := readFetchRefspecs(t, bareDir)
	found := false
	for _, r := range refspecs {
		if r == prFetchRefspec {
			found = true
		}
	}
	if !found {
		t.Errorf("PR refspec not configured on fresh clone. Got: %v", refspecs)
	}
}

func TestEnsureBareClone_Idempotent(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	for i := 0; i < 3; i++ {
		if _, err := EnsureBareClone(context.Background(), "owner2", "repo2", upstream); err != nil {
			t.Fatalf("EnsureBareClone iteration %d: %v", i, err)
		}
	}

	bareDir, _ := repoDir("owner2", "repo2")
	refspecs := readFetchRefspecs(t, bareDir)

	prCount := 0
	for _, r := range refspecs {
		if r == prFetchRefspec {
			prCount++
		}
	}
	if prCount != 1 {
		t.Errorf("expected exactly 1 PR refspec after 3 calls, got %d. Refspecs: %v", prCount, refspecs)
	}
}

func TestEnsureBareClone_RepairsOriginURL(t *testing.T) {
	withTestHome(t)
	upstream1 := makeTestUpstream(t)
	upstream2 := makeTestUpstream(t)

	bareDir, err := EnsureBareClone(context.Background(), "owner3", "repo3", upstream1)
	if err != nil {
		t.Fatalf("first EnsureBareClone: %v", err)
	}
	out, err := exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		t.Fatalf("read origin url: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != upstream1 {
		t.Fatalf("setup: expected origin %q, got %q", upstream1, got)
	}

	if _, err := EnsureBareClone(context.Background(), "owner3", "repo3", upstream2); err != nil {
		t.Fatalf("second EnsureBareClone: %v", err)
	}
	out, err = exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		t.Fatalf("read origin url after repair: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != upstream2 {
		t.Errorf("expected origin repaired to %q, got %q", upstream2, got)
	}
}

// TestEnsureBareClone_AddsRefspecToExistingBare covers the case where
// a bare already exists on disk from before this code shipped — it
// won't have the PR refspec configured. EnsureBareClone must upgrade
// it in place rather than only configuring fresh clones, otherwise
// existing users wouldn't get the fork-PR fix.
func TestEnsureBareClone_AddsRefspecToExistingBare(t *testing.T) {
	home := withTestHome(t)
	upstream := makeTestUpstream(t)

	// Manually create a bare without the PR refspec, mirroring how the
	// pre-SKY-214 code cloned (--bare --filter=blob:none, no PR refspec).
	// The blob filter is what forces git to set up a real
	// remote.origin.fetch entry — a plain `git clone --bare` from a
	// local path uses the alternates mechanism and skips the remote
	// config entirely, which doesn't match what real users have on disk.
	bareDir := filepath.Join(home, ".triagefactory", "repos", "owner4", "repo4.git")
	if err := os.MkdirAll(filepath.Dir(bareDir), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if out, err := exec.Command("git", "clone", "--bare", "--filter=blob:none", upstream, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("manual bare clone: %v: %s", err, out)
	}
	for _, r := range readFetchRefspecs(t, bareDir) {
		if r == prFetchRefspec {
			t.Fatalf("setup: PR refspec already present on plain --bare clone, test premise is wrong")
		}
	}

	if _, err := EnsureBareClone(context.Background(), "owner4", "repo4", upstream); err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}
	found := false
	for _, r := range readFetchRefspecs(t, bareDir) {
		if r == prFetchRefspec {
			found = true
		}
	}
	if !found {
		t.Errorf("PR refspec not added to existing bare. Got: %v", readFetchRefspecs(t, bareDir))
	}
}

func TestBootstrapBareClones_SkipsEmptyCloneURL(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	BootstrapBareClones(context.Background(), []BootstrapTarget{
		{Owner: "with", Repo: "url", CloneURL: upstream},
		{Owner: "without", Repo: "url", CloneURL: ""},
	})

	withDir, _ := repoDir("with", "url")
	if _, err := os.Stat(withDir); err != nil {
		t.Errorf("expected bare for non-empty URL: %v", err)
	}
	withoutDir, _ := repoDir("without", "url")
	if _, err := os.Stat(withoutDir); !os.IsNotExist(err) {
		t.Errorf("expected no bare for empty URL, got err=%v", err)
	}
}

func TestBootstrapBareClones_EmptyTargets(t *testing.T) {
	BootstrapBareClones(context.Background(), nil)
	BootstrapBareClones(context.Background(), []BootstrapTarget{})
}

// TestCreateForPR_ForkPR_FetchesViaPullRef is the regression test for
// the fork-PR fetch path. It mirrors GitHub's actual setup: the PR's
// head commit lives ONLY at refs/pull/<n>/head on the upstream — the
// branch refs/heads/<headBranch> does NOT exist on origin (it lives
// in the fork, which we deliberately don't clone). Pre-fix CreateForPR
// fetched refs/heads/<headBranch> from origin, which fails outright
// for fork PRs.
//
// Test setup builds a fake "fork PR" by making a commit elsewhere
// and pushing it to upstream's refs/pull/42/head — exactly what
// GitHub does server-side when a PR is opened from a fork. We do NOT
// push to refs/heads/feature-branch on upstream, so any code path
// that tries to fetch refs/heads/feature-branch will fail.
func TestCreateForPR_ForkPR_FetchesViaPullRef(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// Build a "fork" working tree, make a commit, push it to upstream
	// as refs/pull/42/head ONLY (no refs/heads/feature-branch on
	// upstream). This is exactly the state GitHub sets up for a fork PR.
	fork := filepath.Join(t.TempDir(), "fork-work")
	if out, err := exec.Command("git", "init", "-b", "main", fork).CombinedOutput(); err != nil {
		t.Fatalf("git init fork: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", fork, "config", "user.email", "fork@example.com"},
		{"-C", fork, "config", "user.name", "Forker"},
		{"-C", fork, "remote", "add", "origin", upstream},
		{"-C", fork, "fetch", "origin", "main"},
		{"-C", fork, "checkout", "-b", "feature-branch", "FETCH_HEAD"},
		{"-C", fork, "commit", "--allow-empty", "-m", "fork PR commit"},
		{"-C", fork, "push", "origin", "HEAD:refs/pull/42/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err := exec.Command("git", "-C", fork, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse fork HEAD: %v", err)
	}
	forkCommit := strings.TrimSpace(string(out))

	// Sanity: the upstream does NOT have refs/heads/feature-branch.
	// If this assertion ever fails, the test setup is wrong and we
	// wouldn't be exercising the fork-PR path at all.
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/feature-branch").CombinedOutput(); err == nil {
		t.Fatalf("test setup: upstream unexpectedly has refs/heads/feature-branch: %s", out)
	}

	wtPath, err := CreateForPR(context.Background(), "owner-fork-test", "repo-fork-test", upstream, "feature-branch", 42, "fork-pr-test-run")
	if err != nil {
		t.Fatalf("CreateForPR for fork PR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "fork-pr-test-run") })

	// Worktree HEAD must point at the fork's PR commit — the same
	// commit refs/pull/42/head pointed to on upstream. Anything else
	// means the fetch landed wrong content.
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != forkCommit {
		t.Errorf("worktree HEAD = %q, want %q (the fork PR commit)", got, forkCommit)
	}

	// Worktree should be on the local feature-branch ref so the agent
	// can push commits and have them flow back up to a sensible target
	// (though for fork PRs that's still wrong — see CreateForPR doc).
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse abbrev: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "feature-branch" {
		t.Errorf("worktree branch = %q, want %q", got, "feature-branch")
	}
}

// TestCreateForPR_OwnRepoPR_FetchesViaPullRef confirms the
// refs/pull/<n>/head fetch path is also correct for the common case
// where the PR is from a branch on the upstream itself. GitHub
// maintains refs/pull/<n>/head for every PR regardless of fork
// status, so the same code path should work uniformly.
func TestCreateForPR_OwnRepoPR_FetchesViaPullRef(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// Push a feature branch directly to upstream AND mirror it as
	// refs/pull/7/head — the state GitHub sets up for an own-repo PR.
	work := filepath.Join(t.TempDir(), "work-own")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "me@example.com"},
		{"-C", work, "config", "user.name", "Me"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "fetch", "origin", "main"},
		{"-C", work, "checkout", "-b", "my-feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "own-repo PR commit"},
		{"-C", work, "push", "origin", "my-feature:refs/heads/my-feature"},
		{"-C", work, "push", "origin", "my-feature:refs/pull/7/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse work HEAD: %v", err)
	}
	expected := strings.TrimSpace(string(out))

	wtPath, err := CreateForPR(context.Background(), "owner-own-test", "repo-own-test", upstream, "my-feature", 7, "own-pr-test-run")
	if err != nil {
		t.Fatalf("CreateForPR for own-repo PR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "own-pr-test-run") })

	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != expected {
		t.Errorf("worktree HEAD = %q, want %q", got, expected)
	}
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse abbrev: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "my-feature" {
		t.Errorf("worktree branch = %q, want %q (git push relies on attached branch, not detached HEAD)", got, "my-feature")
	}
}
