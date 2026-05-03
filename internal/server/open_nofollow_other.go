//go:build !unix

package server

import "os"

// openNoFollow on non-unix platforms falls back to plain os.Open.
// The Lstat check upstream still gates against symlinks, so this
// retains the (small) TOCTOU window on Windows — accepted because
// Windows symlinks require elevated privileges to create and the
// app is single-user. See open_nofollow_unix.go for the version
// that closes the race entirely.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
