//go:build unix

package server

import (
	"os"
	"syscall"
)

// openNoFollow opens a file for reading with O_NOFOLLOW so the
// kernel itself refuses to traverse a final-component symlink. This
// closes the TOCTOU window between an Lstat that says "regular file"
// and an os.Open that follows a symlink installed in the
// intervening microseconds — the verification and the open are now
// effectively atomic from our perspective.
//
// Unix-only: syscall.O_NOFOLLOW is not defined on Windows. The
// non-unix build sees a different file that falls back to plain
// os.Open. Windows symlinks require elevated privileges to create
// by default, so the fallback is acceptable for the single-user
// local-first deployment this code targets.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
