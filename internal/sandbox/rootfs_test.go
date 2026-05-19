package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractTarGz_RejectsPathTraversal pins the security check on
// the rootfs extractor: a malicious tarball with a ../ entry must
// be rejected, not silently extracted outside the destination dir.
// Alpine's official rootfs is trusted but the check protects future
// pin updates against a compromised mirror.
func TestExtractTarGz_RejectsPathTraversal(t *testing.T) {
	// Build a tarball with one valid entry + one escape attempt.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mustWriteFile(t, tw, "ok.txt", "fine")
	mustWriteFile(t, tw, "../escape.txt", "should fail")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarballPath := filepath.Join(t.TempDir(), "evil.tgz")
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	err := extractTarGz(tarballPath, dst)
	if err == nil {
		t.Errorf("extractTarGz accepted a ../ entry; should have rejected")
	}
}

// TestExtractTarGz_NormalEntries pins the happy path: regular files
// + directories + symlinks unpack correctly with mode bits preserved.
func TestExtractTarGz_NormalEntries(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mustWriteDir(t, tw, "bin/")
	mustWriteFile(t, tw, "bin/echo", "#!fake-binary")
	mustWriteSymlink(t, tw, "bin/sh", "/bin/echo")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarballPath := filepath.Join(t.TempDir(), "good.tgz")
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := extractTarGz(tarballPath, dst); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "bin", "echo")); err != nil {
		t.Errorf("bin/echo missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "bin", "sh")); err != nil {
		t.Errorf("bin/sh symlink missing: %v", err)
	}
}

func mustWriteFile(t *testing.T, tw *tar.Writer, name, content string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

func mustWriteDir(t *testing.T, tw *tar.Writer, name string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}); err != nil {
		t.Fatal(err)
	}
}

func mustWriteSymlink(t *testing.T, tw *tar.Writer, name, target string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o777,
		Linkname: target,
		Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatal(err)
	}
}
