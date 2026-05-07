package projectclassify

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"
)

// DefaultWaitTimeout is the spawner's deadline for a fresh
// classification before a delegation proceeds without project KB.
// 90 seconds gives generous headroom for headless `claude` cold-start
// (~5-15s) plus Stage 1 (~3-8s) and a single Stage 2 escalation
// (~10-30s) with margin. Pathological cases still resolve via the
// timeout rather than hanging the spawner indefinitely.
const DefaultWaitTimeout = 90 * time.Second

// pollInterval is how often WaitFor checks classified_at. SQLite
// reads are sub-millisecond so 1s is essentially free; finer
// granularity wouldn't materially change behavior given that
// classifications take seconds.
const pollInterval = 1 * time.Second

// WaitFor blocks until the entity has been classified (classified_at
// IS NOT NULL), the entity row vanishes, ctx is cancelled, or the
// timeout elapses — whichever fires first. Triggers the runner once
// on entry to ensure the classifier wakes up even if no post-poll
// trigger has fired for this entity yet.
//
// Always returns — never propagates error to the caller. The caller
// (typically the spawner's setup path) proceeds with whatever
// project_id is currently on the row.
//
// Intended call site: spawner setup, just before reading
// entity.project_id to inject project knowledge into the worktree.
func WaitFor(ctx context.Context, database *sql.DB, runner *Runner, entityID string, timeout time.Duration) {
	if entityID == "" || database == nil || runner == nil {
		return
	}
	done, exists := classificationStatus(database, entityID)
	if !exists {
		log.Printf("[classify] WaitFor: entity %s not found — returning early", entityID)
		return
	}
	if done {
		return
	}
	runner.Trigger()

	// NewTimer + NewTicker rather than time.After in a loop so the
	// timers stop cleanly on ctx-cancel (no garbage timers firing
	// later) and we don't allocate a fresh timer per iteration. Hot
	// path — called once per delegated run setup.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			log.Printf("[classify] WaitFor timed out for entity %s after %s — proceeding without project context", entityID, timeout)
			return
		case <-ticker.C:
			done, exists := classificationStatus(database, entityID)
			if !exists {
				log.Printf("[classify] WaitFor: entity %s vanished mid-wait — returning early", entityID)
				return
			}
			if done {
				return
			}
		}
	}
}

// classificationStatus returns (classified, exists). A missing row
// (sql.ErrNoRows) returns (false, false) so WaitFor can stop polling
// early — an entity that was deleted will never be classified. Other
// errors return (false, true), treating the row as still-pending so
// transient DB blips don't short-circuit the wait.
func classificationStatus(database *sql.DB, entityID string) (classified, exists bool) {
	var ts sql.NullTime
	err := database.QueryRow(`SELECT classified_at FROM entities WHERE id = ?`, entityID).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false
	}
	if err != nil {
		log.Printf("[classify] WaitFor: transient read error for entity %s: %v", entityID, err)
		return false, true
	}
	return ts.Valid, true
}
