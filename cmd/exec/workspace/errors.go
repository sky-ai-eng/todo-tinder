package workspace

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
)

// translateLookupErr maps the runident error sentinels into the
// workspace-specific sentinels callers (tests + the CLI body) expect.
// Errors that don't match runident's typed sentinels pass through
// wrapped so the message carries "workspace add:" context.
func translateLookupErr(runID string, err error) error {
	switch {
	case errors.Is(err, runident.ErrRunIdentityMissing):
		return errMissingRunID
	case errors.Is(err, runident.ErrRunIdentityNotFound):
		return fmt.Errorf("%w: %s", errRunNotFound, runID)
	default:
		return fmt.Errorf("workspace add: %w", err)
	}
}
