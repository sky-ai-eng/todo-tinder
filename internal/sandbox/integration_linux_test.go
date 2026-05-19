//go:build linux && integration

// Integration tests for the sandbox package. Build-tagged so:
//
//   - Standard `go test ./...` skips them (no runsc needed)
//   - CI / dev with runsc installed runs via `go test -tags integration`
//
// Each test calls Wrap() with a busybox-shaped payload and asserts
// against the SKY-254 acceptance criteria (Property B env curation,
// filesystem isolation, loopback isolation, cleanup).
//
// Run prerequisites:
//   - Linux (build tag enforced)
//   - runsc on PATH (https://gvisor.dev/releases)
//   - iptables + iproute2
//   - root or CAP_NET_ADMIN + CAP_SYS_ADMIN (gVisor needs these)
//
// Tests SKIP gracefully when prerequisites are missing rather than
// failing — so the same suite can run on a stripped-down dev box.

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireRunsc(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc not installed; skipping integration test")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 not installed; skipping integration test")
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("iptables not installed; skipping integration test")
	}
	if _, err := exec.LookPath("node"); err != nil {
		// Tests use a busybox payload, not node — but Wrap() calls
		// exec.LookPath("node") to bind-mount it. Install a stub if
		// real node isn't around.
		t.Skip("node not installed; skipping integration test")
	}
}

// minimalConfig builds a Config for tests with sensible defaults +
// a unique RunID. Caller can mutate Argv to choose the payload.
func minimalConfig(t *testing.T) Config {
	t.Helper()
	worktree := t.TempDir()
	if err := os.Chown(worktree, WorktreeUID, WorktreeGID); err != nil {
		t.Skipf("can't chown worktree to UID %d (probably not root): %v", WorktreeUID, err)
	}
	sdkDir := t.TempDir() // empty stub for integration tests
	nodeBin, _ := exec.LookPath("node")
	if nodeBin == "" {
		nodeBin = "/usr/bin/node" // fallback; tests that need it will fail loudly
	}
	return Config{
		RunID:      "itest" + t.Name()[:min(len(t.Name()), 6)],
		Worktree:   worktree,
		SDKDir:     sdkDir,
		NodeBinary: nodeBin,
		Argv:       []string{"/bin/echo", "hello"},
		Env: []string{
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"HOME=/work",
		},
	}
}

// TestIntegration_BootBusyboxPayload — Wrap, Start+Wait an echo
// command, observe "hello" on stdout. Smoke test that the whole
// pipeline (subnet alloc → netns → veth → bundle → runsc) works.
func TestIntegration_BootBusyboxPayload(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd.CombinedOutput: %v (output: %s)", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("output missing 'hello': %s", out)
	}
}

// TestIntegration_PropertyB_NoCredentialsInEnv is the load-bearing
// security test. Sets sentinel env vars in the test process; the
// sandboxed `env` payload must NOT see them.
func TestIntegration_PropertyB_NoCredentialsInEnv(t *testing.T) {
	requireRunsc(t)

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-MUST-NOT-LEAK")
	t.Setenv("GITHUB_TOKEN", "ghp_MUST_NOT_LEAK")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "AWS-MUST-NOT-LEAK")

	cfg := minimalConfig(t)
	cfg.Argv = []string{"/usr/bin/env"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd.CombinedOutput: %v (output: %s)", err, out)
	}
	for _, sentinel := range []string{
		"sk-ant-MUST-NOT-LEAK",
		"ghp_MUST_NOT_LEAK",
		"AWS-MUST-NOT-LEAK",
	} {
		if strings.Contains(string(out), sentinel) {
			t.Errorf("Property B VIOLATED: sandbox env contains host sentinel %q\n--- env output ---\n%s",
				sentinel, out)
		}
	}
}

// TestIntegration_NonRootUID confirms the agent runs as UID 10000,
// not root. T3 hardening requires this.
func TestIntegration_NonRootUID(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)
	cfg.Argv = []string{"/usr/bin/id", "-u"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd.CombinedOutput: %v (output: %s)", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := "10000"
	if got != want {
		t.Errorf("sandbox UID = %q, want %q (non-root for T3 hardening)", got, want)
	}
}

// TestIntegration_WorktreeIsolation confirms host paths outside the
// worktree bind-mount are invisible to the sandbox.
func TestIntegration_WorktreeIsolation(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)
	// Stash a sentinel on the host that the sandbox shouldn't see.
	if err := os.WriteFile("/tmp/host-sentinel-"+t.Name(),
		[]byte("must-not-be-readable-from-sandbox"), 0o644); err != nil {
		t.Skipf("can't write host sentinel: %v", err)
	}
	defer os.Remove("/tmp/host-sentinel-" + t.Name())

	cfg.Argv = []string{"/bin/cat", "/tmp/host-sentinel-" + t.Name()}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()

	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "must-not-be-readable") {
		t.Errorf("filesystem isolation broken: sandbox read host sentinel\n--- output ---\n%s", out)
	}
}

// TestIntegration_CleanupRemovesNetns runs Wrap → Close and asserts
// the netns no longer exists at /var/run/netns/.
func TestIntegration_CleanupRemovesNetns(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	netnsPath := sb.NetnsPath
	_ = cmd.Run() // run + finish

	if _, err := os.Stat(netnsPath); err != nil {
		t.Errorf("netns gone before Close — that's wrong, runsc shouldn't auto-clean")
	}

	if err := sb.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(netnsPath); !os.IsNotExist(err) {
		t.Errorf("netns %s still exists after Close (stat err = %v)", netnsPath, err)
	}
}

// TestIntegration_ReapOrphans creates an orphan netns by skipping
// Close, then verifies ReapOrphans cleans it up.
func TestIntegration_ReapOrphans(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	netnsPath := sb.NetnsPath
	// Skip Close — leave an orphan.

	if err := ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if _, err := os.Stat(netnsPath); !os.IsNotExist(err) {
		t.Errorf("orphan netns %s not reaped", netnsPath)
	}
}

// Ensure filepath is used (defensive; gofmt would otherwise drop it
// if the only reference goes away during a future edit).
var _ = filepath.Join
