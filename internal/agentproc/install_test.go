package agentproc

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSDKAlreadyInstalled_HitsPinnedVersion pins the load-bearing
// short-circuit: when node_modules already contains the pinned SDK
// version, doInstall must return without invoking checkNode/checkNpm.
// This is what makes the Docker image's pre-installed SDK work in a
// runtime layer with no node binary on PATH.
func TestSDKAlreadyInstalled_HitsPinnedVersion(t *testing.T) {
	sdkDir := t.TempDir()
	pkgDir := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgJSON := []byte(`{"name":"@anthropic-ai/claude-agent-sdk","version":"` + sdkVersion + `"}`)
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), pkgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if !sdkAlreadyInstalled(sdkDir) {
		t.Error("expected sdkAlreadyInstalled=true for the pinned version, got false")
	}
}

// TestSDKAlreadyInstalled_MissingReturnsFalse pins the negative case:
// an empty sdk dir must return false so doInstall falls through to
// the node/npm checks + npm ci path.
func TestSDKAlreadyInstalled_MissingReturnsFalse(t *testing.T) {
	sdkDir := t.TempDir()
	if sdkAlreadyInstalled(sdkDir) {
		t.Error("expected false for empty sdk dir, got true")
	}
}

// TestSDKAlreadyInstalled_WrongVersionReturnsFalse pins the version-
// gating part of the check: a stale install from a previous TF
// version must NOT short-circuit, so a sdkVersion bump triggers a
// re-install via the existing installSDKIfNeeded path.
func TestSDKAlreadyInstalled_WrongVersionReturnsFalse(t *testing.T) {
	sdkDir := t.TempDir()
	pkgDir := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgJSON := []byte(`{"name":"@anthropic-ai/claude-agent-sdk","version":"0.0.1-stale"}`)
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), pkgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if sdkAlreadyInstalled(sdkDir) {
		t.Error("expected false for stale version, got true (would skip needed re-install)")
	}
}
