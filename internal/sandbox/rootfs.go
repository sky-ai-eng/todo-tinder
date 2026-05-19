package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractTarGz unpacks a gzip-compressed tar archive into dst.
// Filters paths through filepath.Clean + symlink-target validation
// to defend against malicious tarballs with ../ traversal entries
// (zip-slip equivalent). Alpine's official rootfs is trusted, but
// the defensive code costs us little and protects against future
// pin updates pulling a compromised mirror.
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

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
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
			// Validate the link target stays within dst too — a
			// malicious symlink pointing at /etc/shadow would make
			// later writes to "target" overwrite the host's file.
			linkClean := filepath.Clean(hdr.Linkname)
			if !filepath.IsAbs(linkClean) && strings.HasPrefix(linkClean, "..") {
				// Relative symlinks must stay inside the rootfs;
				// alpine doesn't ship escaping symlinks in its
				// minirootfs but pin updates could.
				resolved := filepath.Join(filepath.Dir(target), linkClean)
				rel, err := filepath.Rel(dstAbs, resolved)
				if err != nil || strings.HasPrefix(rel, "..") {
					return fmt.Errorf("symlink %q points outside rootfs", hdr.Name)
				}
			}
			// Absolute symlinks inside the rootfs (e.g.
			// /usr/bin/sh → /bin/sh) are permitted; the sandbox
			// interprets them relative to its own root.
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Remove existing target (some tarballs have duplicate
			// entries).
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Skip char/block devices, fifos, etc. — alpine
			// minirootfs ships only files/dirs/symlinks; anything
			// else is suspicious and skipping is safer than handling.
		}
	}
}
