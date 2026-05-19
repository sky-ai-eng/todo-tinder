package workspace

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
)

// translateLookupErr maps the runident error sentinels into the
// workspace-specific sentinels callers (tests + the CLI body) expect.
// Errors that don't match runident's typed sentinels pass through
// wrapped with cmdPrefix ("workspace add:" / "workspace list:") so
// the agent's stderr message correctly identifies the source command.
//
// runID is the env-derived run id when the caller has it (the
// ErrRunIdentityNotFound branch uses it in the message); pass "" for
// LookupRun call sites that don't know it yet because LookupRun
// itself is what would have produced it.
func translateLookupErr(cmdPrefix, runID string, err error) error {
	switch {
	case errors.Is(err, runident.ErrRunIdentityMissing):
		return errMissingRunID
	case errors.Is(err, runident.ErrRunIdentityNotFound):
		return fmt.Errorf("%w: %s", errRunNotFound, runID)
	default:
		return fmt.Errorf("%s: %w", cmdPrefix, err)
	}
}
