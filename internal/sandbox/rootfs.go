package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// apkPackages is the toolchain installed into the cached alpine
// rootfs after extraction. Any change here invalidates the rootfs
// cache key (rootfsCacheKey hashes this slice alongside the alpine
// tarball sha) so a fresh extraction picks up the new package set
// on the next sandbox launch.
//
//   - nodejs/npm — agent SDK runtime; corepack ships with npm so
//     pnpm/yarn are available on demand without a separate apk add.
//   - git — every delegate/curator flow does status/diff/commit/push.
//   - ripgrep — agent's primary code-search tool; faster than grep
//     on large repos.
//   - bash — alpine ships ash by default; many shell scripts and
//     agent invocations assume bash.
//   - ca-certificates — outbound TLS from in-sandbox tools (git over
//     HTTPS, npm registry, the in-host proxy a follow-up ticket
//     wires in).
//   - python3 — common in build scripts and tooling glue.
//   - go — Go projects need go build/test/mod tidy to verify changes.
//   - make — ubiquitous in build flows.
//   - curl — ubiquitous, tiny, expected.
//   - openssh-client — git over SSH and any tooling that calls ssh.
//   - build-base — gcc + make + musl-dev; required for native deps
//     (node-gyp, cgo, anything that compiles at install time).
var apkPackages = []string{
	"nodejs", "npm", "git", "ripgrep", "bash", "ca-certificates",
	"python3", "go", "make", "curl", "openssh-client", "build-base",
}

// rootfsCacheKey returns the 12-hex-char cache key for the current
// rootfs build inputs.
func rootfsCacheKey() string {
	return rootfsCacheKeyFor(alpineRootfsSHA256, apkPackages)
}

// rootfsCacheKeyFor hashes (alpine_sha, sorted-package-set) and
// returns the first 12 hex chars. Sorting before hashing means the
// key depends on the package set, not on slice ordering — a future
// maintainer reshuffling apkPackages won't silently invalidate a
// cache that's actually still correct.
//
// The cache directory is derived from this key, so adding a package
// produces a new key, forces a fresh extraction, and avoids the
// failure mode where the on-disk cache contains the old toolchain
// while the running binary expects the new one.
func rootfsCacheKeyFor(alpineSha string, packages []string) string {
	sorted := append([]string(nil), packages...)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(alpineSha))
	h.Write([]byte{0})
	for _, p := range sorted {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// extractTarGz unpacks a gzip-compressed tar archive into dst.
// Two-pass for security:
//
//  1. First pass writes directories + regular files only. Defers
//     all symlink entries to a slice.
//  2. Second pass creates the deferred symlinks.
//
// Why two-pass: the obvious single-pass approach is vulnerable to a
// "symlink-then-write-through-it" attack. A malicious tarball can
// emit `link -> /etc` followed by `link/file`; in the single-pass
// path, the symlink write completes first, then `link/file`'s
// MkdirAll/OpenFile silently traverses the symlink and writes to
// `/etc/file` on the host. Two-pass extraction defers symlink
// creation until after every regular file has been written, so no
// subsequent open() can traverse a symlink we just created.
//
// Alpine's official rootfs is sha256-pinned and trusted (the
// production source), so this attack isn't reachable today. The
// two-pass approach defends future pin updates against compromised
// mirrors and makes the extractor reusable for less-trusted
// tarballs (e.g. user-supplied custom rootfs in a future feature).
//
// Path sanitization on each entry rejects ../ traversal regardless
// of which pass it falls into.
//
// Cross-platform so the security-sensitive extractor is covered by
// CI tests on every dev box, not just Linux.
func extractTarGz(tarballPath, dst string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	// Deferred symlink entries — name + linkname, after path
	// sanitization. Created in a second pass after all regular
	// files are written.
	type deferredLink struct {
		name     string // sanitized absolute target path under dst
		linkname string // verbatim from tar header
	}
	var symlinks []deferredLink

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Sanitize the path: reject anything that escapes dst.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
			return fmt.Errorf("tarball entry escapes dst: %q", hdr.Name)
		}
		target := filepath.Join(dstAbs, clean)
		relTarget, err := filepath.Rel(dstAbs, target)
		if err != nil || strings.HasPrefix(relTarget, "..") {
			return fmt.Errorf("tarball entry resolves outside dst: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(target,
				os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
				os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Validate the link target — relative symlinks must
			// stay inside the rootfs. Absolute symlinks (e.g.
			// /usr/bin/sh → /bin/sh) are permitted; the sandbox
			// interprets them relative to its own root, so they
			// can't escape the rootfs even if they look absolute.
			linkClean := filepath.Clean(hdr.Linkname)
			if !filepath.IsAbs(linkClean) && strings.HasPrefix(linkClean, "..") {
				resolved := filepath.Join(filepath.Dir(target), linkClean)
				rel, err := filepath.Rel(dstAbs, resolved)
				if err != nil || strings.HasPrefix(rel, "..") {
					return fmt.Errorf("symlink %q points outside rootfs", hdr.Name)
				}
			}
			// Defer creation to the second pass so no later regular
			// file write can be funneled through this symlink.
			symlinks = append(symlinks, deferredLink{
				name:     target,
				linkname: hdr.Linkname,
			})
		default:
			// Skip char/block devices, fifos, etc. — alpine
			// minirootfs ships only files/dirs/symlinks; anything
			// else is suspicious and skipping is safer than handling.
		}
	}

	// Second pass: create symlinks now that every regular file is
	// in place. No subsequent open() runs after this, so creating
	// the symlinks last means nothing we write can traverse one.
	for _, l := range symlinks {
		if err := os.MkdirAll(filepath.Dir(l.name), 0o755); err != nil {
			return err
		}
		// Remove an existing target (tar duplicates are rare but
		// alpine's rootfs has a few).
		_ = os.Remove(l.name)
		if err := os.Symlink(l.linkname, l.name); err != nil {
			return err
		}
	}
	return nil
}
