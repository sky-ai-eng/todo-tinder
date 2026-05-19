package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// writeBundle constructs the per-run OCI bundle directory on disk:
//
//	$TMPDIR/tf-bundle-<runID>/
//	├── config.json     (the OCI spec)
//	├── resolv.conf     (1.1.1.1/8.8.8.8 — bind-mounted at /etc/resolv.conf)
//	└── rootfs/         (symlink to the cached alpine minirootfs)
//
// The rootfs is a SYMLINK (not a copy) to the cached extraction at
// ~/.triagefactory/sandbox/rootfs-<sha>/ — saves 5MB of file copies
// per run and keeps the bundle dir tiny. runsc handles symlinks
// fine.
//
// Returns the bundle dir path so the caller can pass it to
// `runsc run --bundle <dir>`. Caller must remove via cleanupBundle
// (called from Sandbox.Close) when the run finishes.
//
// Appends a synthesized resolv.conf mount to spec.Mounts before
// writing config.json — the resolv.conf path inside the bundle dir
// isn't knowable until writeBundle has run, so buildSpec leaves the
// mount slot empty and we fill it here.
func writeBundle(cfg Config, spec *specs.Spec, rootfsPath string) (string, error) {
	bundleDir := filepath.Join(os.TempDir(), "tf-bundle-"+cfg.RunID)

	// Remove any leftover bundle dir from a crashed prior run with
	// the same RunID — shouldn't happen because RunIDs are
	// per-invocation UUIDs, but be defensive.
	if err := os.RemoveAll(bundleDir); err != nil {
		return "", fmt.Errorf("bundle: clean stale dir: %w", err)
	}
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return "", fmt.Errorf("bundle: mkdir: %w", err)
	}

	// Symlink the cached rootfs into the bundle.
	rootfsLink := filepath.Join(bundleDir, "rootfs")
	if err := os.Symlink(rootfsPath, rootfsLink); err != nil {
		return "", fmt.Errorf("bundle: symlink rootfs: %w", err)
	}

	// Synthesize resolv.conf and append the bind mount to the spec.
	resolvPath, err := writeResolvConf(bundleDir)
	if err != nil {
		return "", fmt.Errorf("bundle: resolv.conf: %w", err)
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/etc/resolv.conf",
		Type:        "bind",
		Source:      resolvPath,
		Options:     []string{"rbind", "ro"},
	})

	// Write the spec last so it reflects all the mount appends.
	if err := specOnDisk(spec, bundleDir); err != nil {
		return "", fmt.Errorf("bundle: %w", err)
	}
	return bundleDir, nil
}

// cleanupBundle removes the per-run bundle dir. Idempotent.
func cleanupBundle(bundleDir string) error {
	if bundleDir == "" {
		return nil
	}
	return os.RemoveAll(bundleDir)
}
