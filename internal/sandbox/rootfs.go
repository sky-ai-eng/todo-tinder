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
