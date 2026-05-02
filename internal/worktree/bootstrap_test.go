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
// real ~/.triagefactory.
func withTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
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
