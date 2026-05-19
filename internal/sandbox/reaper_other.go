//go:build !linux

package sandbox

import "context"

// reapOrphansImpl on non-Linux is a no-op. The netns concept
// doesn't exist outside Linux; nothing to clean up.
func reapOrphansImpl(_ context.Context) error { return nil }
