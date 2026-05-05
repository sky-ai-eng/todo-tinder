package repoprofile

import (
	"database/sql"
	"log"
	"sync"
)

// ProfileGate coordinates access to repo profiles. The scorer checks
// Ready() before running; the profiler calls Signal() when done.
// Invalidate() flips Ready back to false until the next Signal.
type ProfileGate struct {
	mu    sync.Mutex
	ready bool
}

// NewProfileGate creates a gate in the not-ready state. The database
// argument is preserved for callers that already pass it; the gate
// itself no longer touches the DB.
func NewProfileGate(_ *sql.DB) *ProfileGate {
	return &ProfileGate{}
}

// Ready returns true if profiling has completed and repo data is current.
func (g *ProfileGate) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ready
}

// Signal marks profiling as complete. The scorer can now run.
func (g *ProfileGate) Signal() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready = true
	log.Println("[repoprofile] gate: profiles ready")
}

// Invalidate marks profiling as stale (e.g., GitHub repos changed).
// The scorer's gate (Ready) flips back to false until Signal fires
// again post-reprofile. There's no per-task matched-repo column to
// clear anymore — repo selection is the agent's responsibility at
// delegation time, materialized lazily via `triagefactory exec
// workspace add`.
func (g *ProfileGate) Invalidate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready = false
	log.Println("[repoprofile] gate: invalidated; will re-signal after re-profile")
}
