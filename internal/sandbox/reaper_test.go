package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReapBundleOrphans cleans tf-bundle-* dirs and leaves
// non-matching dirs alone. Cross-platform: doesn't touch netns.
func TestReapBundleOrphans(t *testing.T) {
	// Use a t.TempDir as a fake $TMPDIR — but reapBundleOrphans
	// reads os.TempDir() directly, which we don't override.
	// Workaround: create some marker dirs in os.TempDir() with
	// recognizable names, then clean up by hand.
	tmp := os.TempDir()
	mine := filepath.Join(tmp, "tf-bundle-"+t.Name())
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(mine) })

	if err := reapBundleOrphans(context.Background()); err != nil {
		t.Fatalf("reapBundleOrphans: %v", err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Errorf("tf-bundle-* dir not reaped: stat err = %v", err)
	}
}

// TestReapBundleOrphans_LeavesNonMatchingAlone proves the prefix
// filter is strict — a dir named "tf-something-else" (no -bundle-)
// must NOT be touched by the reaper, only "tf-bundle-*".
func TestReapBundleOrphans_LeavesNonMatchingAlone(t *testing.T) {
	tmp := os.TempDir()
	// Use a name that starts with "tf-" but isn't "tf-bundle-".
	// This is the collateral-damage guard.
	innocent := filepath.Join(tmp, "tf-someoneelses-"+t.Name())
	if err := os.MkdirAll(innocent, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(innocent) })

	if err := reapBundleOrphans(context.Background()); err != nil {
		t.Fatalf("reapBundleOrphans: %v", err)
	}
	if _, err := os.Stat(innocent); err != nil {
		t.Errorf("non-bundle tf-* dir was touched (stat err = %v); reaper prefix filter too loose", err)
	}
}

// TestSubnetIdxFromNetnsName_FullRegression is the load-bearing test
// for reaper safety. The reaper iterates /var/run/netns/ and tears
// down anything whose name matches our pattern; the regex MUST NOT
// match names a different process might have created.
func TestSubnetIdxFromNetnsName_FullRegression(t *testing.T) {
	for _, name := range []string{
		"tf-someoneelses-app", // user-named, wrong shape
		"tf",                  // bare prefix
		"tf-",                 // truncated
		"tf-12345",            // no idx suffix
		"TF-abc-0",            // wrong case
		"docker-1234",         // common other-tool prefix
		"k8s-pod-net-7",       // another platform's pattern
	} {
		if _, ok := subnetIdxFromNetnsName(name); ok {
			t.Errorf("reaper regex matched %q — would have torn down unrelated netns", name)
		}
	}

	// Positives — must continue to match our own names.
	for _, name := range []string{
		"tf-abc12345-0",
		"tf-deadbeef-255",
		"tf-aaaa-7",
	} {
		if _, ok := subnetIdxFromNetnsName(name); !ok {
			t.Errorf("reaper regex did NOT match our own pattern %q", name)
		}
	}
}

// TestReaper_ExportedAPI is a smoke test that ReapOrphans (the
// exported entry point) doesn't panic on a clean system. The actual
// netns reaping logic is Linux-only and tested via the integration
// suite; this test just verifies the cross-platform entry point.
func TestReaper_ExportedAPI(t *testing.T) {
	if err := ReapOrphans(context.Background()); err != nil {
		// On macOS dev the impl is a no-op + returns nil. On Linux
		// it may fail if /var/run/netns has perms issues, but
		// shouldn't panic.
		t.Logf("ReapOrphans returned: %v (acceptable; tested for non-panic)", err)
	}
	// Sanity that we're testing the right thing: the bundle reaper
	// component should be cross-platform.
	if err := reapBundleOrphans(context.Background()); err != nil {
		t.Errorf("reapBundleOrphans: %v", err)
	}
}

// Suppress unused-warning if a future change drops one of the names.
var _ = strings.HasPrefix
