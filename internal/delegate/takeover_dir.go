package delegate

import (
	"os"
	"path/filepath"
)

// ResolveTakeoverDir expands a stored takeover directory string into an
// absolute filesystem path. The stored value comes from instance_config.
// server_takeover_dir; empty defaults to ~/.triagefactory/takeovers and a
// leading "~/" is expanded against the user's home dir. Centralized so
// callers (handleAgentTakeover, Release, uninstall, tests) don't each
// re-implement the home-dir math.
func ResolveTakeoverDir(stored string) (string, error) {
	dir := stored
	if dir == "" {
		dir = "~/.triagefactory/takeovers"
	}
	if len(dir) >= 2 && dir[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, dir[2:])
	}
	return dir, nil
}
