//go:build linux

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// alpineRootfsURL pins the alpine minirootfs the sandbox uses. Same
// tarball as the validation probe (probe.sh line 64) so the runtime
// behavior matches what was tested. Bumping requires re-running the
// probe to verify nothing changed in alpine's syscall surface that
// the SDK depends on.
const (
	alpineRootfsURL    = "https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.3-x86_64.tar.gz"
	alpineRootfsSHA256 = "d4e6fd67dcf75e40c451560ac7265166c2b72a0f38ddc9aae756a7de3d1efa0c"
)

// rootfsCacheMu serializes ensureRootfs across concurrent Wrap calls.
// We use a mutex rather than sync.Once because sync.Once permanently
// caches the first call's result — a transient failure (CDN flap,
// disk full at extract time) would then poison every subsequent
// Wrap for the lifetime of the process. The mutex guards a check of
// the on-disk sentinel file, so successful prior calls return fast
// without re-extracting, while failures don't lock anyone out of
// retrying.
var rootfsCacheMu sync.Mutex

// ensureRootfs idempotently downloads + verifies + extracts the
// alpine minirootfs to ~/.triagefactory/sandbox/rootfs-<sha>/ and
// returns the path. Concurrency-safe via a mutex; success cached
// via the sentinel file inside doEnsureRootfs (so re-entry after
// a successful first call is just one os.Stat).
func ensureRootfs() (string, error) {
	rootfsCacheMu.Lock()
	defer rootfsCacheMu.Unlock()
	return doEnsureRootfs()
}

func doEnsureRootfs() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("rootfs: resolve home: %w", err)
	}
	// Embed the sha in the directory name so a pin change forces a
	// fresh extraction without us having to detect drift manually.
	cacheDir := filepath.Join(home, ".triagefactory", "sandbox",
		"rootfs-"+alpineRootfsSHA256[:12])

	// Sentinel file marking a successful extraction. Lets us
	// distinguish "fully populated cache" from "extraction crashed
	// halfway, leaving partial files."
	sentinel := filepath.Join(cacheDir, ".extracted-ok")
	if _, err := os.Stat(sentinel); err == nil {
		return cacheDir, nil
	}

	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return "", fmt.Errorf("rootfs: mkdir parent: %w", err)
	}

	// Clean any partial extraction from a previous crash before
	// starting fresh.
	if err := os.RemoveAll(cacheDir); err != nil {
		return "", fmt.Errorf("rootfs: clean partial cache: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("rootfs: mkdir cache: %w", err)
	}

	// Download to a temp file, sha256-verify, then extract. Failing
	// the verify means upstream alpine got tampered with or our pin
	// is stale — refuse loudly rather than silently extract.
	tmpTarball := filepath.Join(cacheDir, ".alpine.tgz.partial")
	if err := downloadFile(alpineRootfsURL, tmpTarball); err != nil {
		return "", fmt.Errorf("rootfs: download: %w", err)
	}
	defer func() { _ = os.Remove(tmpTarball) }()

	if err := verifySHA256(tmpTarball, alpineRootfsSHA256); err != nil {
		return "", fmt.Errorf("rootfs: verify sha256: %w", err)
	}

	if err := extractTarGz(tmpTarball, cacheDir); err != nil {
		return "", fmt.Errorf("rootfs: extract: %w", err)
	}

	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil {
		return "", fmt.Errorf("rootfs: write sentinel: %w", err)
	}
	return cacheDir, nil
}

func downloadFile(url, dst string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return out.Sync()
}

func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}
