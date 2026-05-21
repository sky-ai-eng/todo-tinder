package delegate

import (
	"os"
	"path/filepath"
)

// ResolveTakeoverDir normalizes a stored takeover directory string.
// The stored value comes from instance_config.server_takeover_dir;
// empty defaults to ~/.triagefactory/takeovers, and a leading "~/" is
// expanded against the user's home dir. Other forms (already-absolute,
// relative-with-no-tilde) are returned as-is — callers that need a
// canonical absolute path apply their own filepath.Abs / EvalSymlinks
// step (Release does this via canonicalizeForSafetyCheck before the
// prefix containment check). Centralized so callers
// (handleAgentTakeover, Release, uninstall, tests) don't each
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
