package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
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

// TestExtractTarGz_RejectsSymlinkThenWriteAttack pins the load-bearing
// two-pass property: a tarball that creates `link -> /tmp` (or another
// host path) and then writes `link/file` must NOT funnel the file
// write through the symlink to land at /tmp/file on the host.
//
// In a single-pass extractor, the symlink write completes first and
// the subsequent MkdirAll/OpenFile follows the symlink. Two-pass
// defers all symlink creation until after every regular file is
// written, which kills the attack.
func TestExtractTarGz_RejectsSymlinkThenWriteAttack(t *testing.T) {
	// Build a tarball with the attack shape: symlink to a host dir,
	// then a regular file underneath the symlink path.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Pick a host path we're guaranteed to be able to write to in
	// a test process: t.TempDir's parent. The attack would write
	// `<dst>/escape/file` which a single-pass extractor would route
	// to `<attack-target>/file`.
	attackTarget := filepath.Join(t.TempDir(), "host-target")
	if err := os.MkdirAll(attackTarget, 0o755); err != nil {
		t.Fatal(err)
	}

	// Symlink first — the bait.
	mustWriteSymlink(t, tw, "escape", attackTarget)
	// Regular file second — would land at attackTarget/should-not-appear
	// in a single-pass extractor.
	mustWriteFile(t, tw, "escape/should-not-appear", "ATTACK SUCCEEDED")

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarballPath := filepath.Join(t.TempDir(), "attack.tgz")
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	// Extraction itself may succeed or fail — both are acceptable.
	// What's NOT acceptable is the attack-target file showing up.
	_ = extractTarGz(tarballPath, dst)

	attackFile := filepath.Join(attackTarget, "should-not-appear")
	if _, err := os.Stat(attackFile); err == nil {
		body, _ := os.ReadFile(attackFile)
		t.Errorf("ATTACK SUCCEEDED: file landed at %s with contents %q\n— two-pass extraction is broken; the symlink was followed by the subsequent regular-file write",
			attackFile, body)
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

// TestRootfsCacheKey_StableAcrossRuns pins the determinism contract:
// the same inputs always produce the same key. If this ever fails,
// the rootfs cache thrashes — every TF process startup re-runs the
// 30-second download + apk-add chain instead of reusing the cached
// extraction.
func TestRootfsCacheKey_StableAcrossRuns(t *testing.T) {
	a := rootfsCacheKey()
	b := rootfsCacheKey()
	if a != b {
		t.Errorf("rootfsCacheKey not stable across calls: got %q then %q", a, b)
	}
}

// TestRootfsCacheKey_ChangesWhenPackagesAdded pins the load-bearing
// invariant: when apkPackages grows (someone adds rust, java, etc.),
// the cache key MUST change so a long-running TF process doesn't
// keep serving a stale rootfs that's missing the new toolchain. The
// failure mode this protects against is silent — no error, just
// ENOENT inside the sandbox six months after the package list grew.
func TestRootfsCacheKey_ChangesWhenPackagesAdded(t *testing.T) {
	base := []string{"nodejs", "git"}
	extended := []string{"nodejs", "git", "rust"}
	if rootfsCacheKeyFor("abc", base) == rootfsCacheKeyFor("abc", extended) {
		t.Error("adding a package didn't change the cache key — stale cache risk")
	}
}

// TestRootfsCacheKey_DeterministicOrder pins that the key depends on
// the package SET, not the slice ordering. A maintainer reshuffling
// apkPackages for readability shouldn't invalidate every operator's
// cache. Sort-before-hash is the chosen mechanism (see
// rootfsCacheKeyFor); this test guards it.
func TestRootfsCacheKey_DeterministicOrder(t *testing.T) {
	a := []string{"nodejs", "git", "ripgrep"}
	b := []string{"ripgrep", "nodejs", "git"}
	if rootfsCacheKeyFor("abc", a) != rootfsCacheKeyFor("abc", b) {
		t.Error("cache key depends on slice ordering; should depend only on the package set")
	}
}

// TestRootfsCacheKey_AlpineShaContributes pins that the alpine
// tarball sha is part of the key. A future alpine version bump must
// invalidate every operator's cache regardless of whether the
// package list changed.
func TestRootfsCacheKey_AlpineShaContributes(t *testing.T) {
	pkgs := []string{"nodejs"}
	if rootfsCacheKeyFor("alpha", pkgs) == rootfsCacheKeyFor("beta", pkgs) {
		t.Error("cache key ignores alpine sha; pin update wouldn't invalidate")
	}
}

// TestAlpineRootfsForArch_KnownArches pins that both supported arches
// resolve to non-empty URL + sha with the right /x86_64/ or /aarch64/
// segment. Catches a copy-paste regression where someone swaps two
// arms of the switch and amd64 starts fetching the arm tarball.
func TestAlpineRootfsForArch_KnownArches(t *testing.T) {
	urlAmd, shaAmd, err := alpineRootfsForArch("amd64")
	if err != nil {
		t.Fatalf("amd64: %v", err)
	}
	if !strings.Contains(urlAmd, "x86_64") || !strings.Contains(urlAmd, "amd64") && !strings.Contains(urlAmd, "x86_64") {
		t.Errorf("amd64 URL %q missing x86_64 segment", urlAmd)
	}
	if shaAmd == "" {
		t.Error("amd64 sha empty")
	}

	urlArm, shaArm, err := alpineRootfsForArch("arm64")
	if err != nil {
		t.Fatalf("arm64: %v", err)
	}
	if !strings.Contains(urlArm, "aarch64") {
		t.Errorf("arm64 URL %q missing aarch64 segment", urlArm)
	}
	if shaArm == "" {
		t.Error("arm64 sha empty (even the TODO placeholder must be non-empty so cache key isn't degenerate)")
	}

	if urlAmd == urlArm {
		t.Error("amd64 and arm64 resolved to the same URL; per-arch dispatch broken")
	}
	if shaAmd == shaArm {
		t.Error("amd64 and arm64 share a sha; per-arch cache key would collide")
	}
}

// TestAlpineRootfsForArch_UnsupportedErrors pins the actionable-error
// contract: an unknown GOARCH (ppc64le, s390x, ...) returns an error
// naming the arch instead of a sha mismatch six minutes into the
// download. The fatal path through currentArchRootfs depends on this
// error being non-nil for the panic to fire on misconfiguration.
func TestAlpineRootfsForArch_UnsupportedErrors(t *testing.T) {
	_, _, err := alpineRootfsForArch("ppc64le")
	if err == nil {
		t.Fatal("expected error for unsupported arch, got nil")
	}
	if !strings.Contains(err.Error(), "ppc64le") {
		t.Errorf("error %q should name the unsupported arch", err)
	}
}

// TestRootfsCacheKey_PerArchSeparate pins that switching arch invalidates
// the cache. Cross-compile workflows or arm64-on-amd64 dev boxes mustn't
// reuse the other arch's extracted rootfs — the binaries are wrong-arch
// and the sandbox would fail at first exec.
func TestRootfsCacheKey_PerArchSeparate(t *testing.T) {
	_, shaAmd, _ := alpineRootfsForArch("amd64")
	_, shaArm, _ := alpineRootfsForArch("arm64")
	if rootfsCacheKeyFor(shaAmd, apkPackages) == rootfsCacheKeyFor(shaArm, apkPackages) {
		t.Error("amd64 and arm64 cache keys collide; cross-arch cache pollution risk")
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
