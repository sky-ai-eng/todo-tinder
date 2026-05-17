package delegate

// Tests that pin the trigger-type-driven pool routing introduced to
// close the PR #193 review-bot P2 findings (resolvePrompt visibility,
// Takeover/Release preflight, Delegate.agentRuns.Create wrap). The
// behavior changes only become load-bearing under Postgres + RLS;
// these unit tests use recording wrappers around real SQLite stores
// to lock the routing contract so the manual-vs-event branch can't
// silently regress to "everything goes through GetSystem."
//
// The Delegate.agentRuns.Create wrap isn't tested in isolation here
// because Delegate spawns an async goroutine — testing the full
// synchronous path would require fixturing a GH client or
// restructuring the spawner. The wrap follows the same pattern as
// the IncrementUsage routing immediately above it in delegate.go;
// SQLite passes through identically for both, and the Postgres RLS
// matrix lands in D9-core's pgtest coverage per SKY-253's acceptance.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// synthCall records one SyntheticClaimsWithTx invocation. The userID
// is what proves the spawner forwarded the real creator (vs falling
// back to a sentinel or skipping the wrap entirely).
type synthCall struct {
	orgID, userID string
}

// recordingTxRunner wraps a real TxRunner and counts every
// SyntheticClaimsWithTx call. WithTx is left as a pass-through
// (embedded) because none of the fixes route through it.
type recordingTxRunner struct {
	db.TxRunner
	synthCalls []synthCall
}

func (r *recordingTxRunner) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	r.synthCalls = append(r.synthCalls, synthCall{orgID: orgID, userID: userID})
	return r.TxRunner.SyntheticClaimsWithTx(ctx, orgID, userID, fn)
}

// recordingPromptStore embeds the real store and overrides Get +
// GetSystem to count which one was reached. The non-spied methods
// pass through via the embedded interface.
type recordingPromptStore struct {
	db.PromptStore
	getCalls       int
	getSystemCalls int
}

func (r *recordingPromptStore) Get(ctx context.Context, orgID, id string) (*domain.Prompt, error) {
	r.getCalls++
	return r.PromptStore.Get(ctx, orgID, id)
}

func (r *recordingPromptStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Prompt, error) {
	r.getSystemCalls++
	return r.PromptStore.GetSystem(ctx, orgID, id)
}

// newRoutingTestSpawner spins up a Spawner whose tx + prompts stores
// are wrapped in recorders. The other stores are real SQLite-backed
// so resolvePrompt / Takeover / Release have the rows they need.
func newRoutingTestSpawner(t *testing.T) (*Spawner, *recordingTxRunner, *recordingPromptStore, *sql.DB) {
	t.Helper()
	database := newTakeoverTestDB(t)
	stores := sqlitestore.New(database)
	tx := &recordingTxRunner{TxRunner: stores.Tx}
	prompts := &recordingPromptStore{PromptStore: stores.Prompts}
	s := NewSpawner(database, prompts, nil, nil, stores.Tasks, stores.AgentRuns, stores.Entities, stores.Reviews, stores.PendingPRs, stores.Events, stores.TaskMemory, stores.RunWorktrees, tx, nil, nil, "claude-sonnet-4-6")
	return s, tx, prompts, database
}

// TestResolvePrompt_ManualBranchesThroughSyntheticClaims pins the
// visibility fix for fix #4. A manual delegation must load the
// prompt via the app pool under the requesting user's synthetic
// claims so prompts_select RLS filters to prompts the user can see.
// Without this branch, a caller could supply a guessed prompt_id
// for another user's private prompt and the agent would run under it.
func TestResolvePrompt_ManualBranchesThroughSyntheticClaims(t *testing.T) {
	s, tx, prompts, database := newRoutingTestSpawner(t)

	ensureTestPrompt(t, database, domain.Prompt{ID: "p-manual", Name: "Manual prompt", Body: "x", Source: "user"})

	got, err := s.resolvePrompt(runmode.LocalDefaultOrg, domain.Task{ID: "t"}, "p-manual", "manual", "00000000-0000-0000-0000-000000000aaa")
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if got == nil || got.ID != "p-manual" {
		t.Fatalf("resolvePrompt returned %+v; want p-manual", got)
	}

	if len(tx.synthCalls) != 1 {
		t.Fatalf("SyntheticClaimsWithTx called %d times; want exactly 1 (manual must route through synth claims)", len(tx.synthCalls))
	}
	if tx.synthCalls[0].userID != "00000000-0000-0000-0000-000000000aaa" {
		t.Errorf("synth call userID = %q; want the caller's creatorUserID", tx.synthCalls[0].userID)
	}
	// The actual Get inside SyntheticClaimsWithTx happens on the
	// TxStores' tx-local PromptStore (constructed inside runTx, not
	// the one we injected), so we can't assert getCalls here. The
	// observable fact that GetSystem was NOT called on the spied
	// store is the proof that the manual path didn't fall back to
	// the admin pool.
	if prompts.getSystemCalls != 0 {
		t.Errorf("PromptStore.GetSystem called %d times; want 0 (manual must NOT bypass the RLS-active app pool)", prompts.getSystemCalls)
	}
}

// TestResolvePrompt_EventStaysOnAdminPool pins the other half of
// fix #4: the router is a system actor with no user identity, so
// event-triggered runs must keep loading via the admin pool. If
// they were forced through SyntheticClaimsWithTx with an empty
// userID, the FK check in multi-mode would fail.
func TestResolvePrompt_EventStaysOnAdminPool(t *testing.T) {
	s, tx, prompts, database := newRoutingTestSpawner(t)

	ensureTestPrompt(t, database, domain.Prompt{ID: "p-event", Name: "Event prompt", Body: "x", Source: "system"})

	// Event-triggered runs carry creatorUserID="" — the router has
	// no user. The routing must NOT depend on creatorUserID being set.
	got, err := s.resolvePrompt(runmode.LocalDefaultOrg, domain.Task{ID: "t"}, "p-event", "event", "")
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if got == nil || got.ID != "p-event" {
		t.Fatalf("resolvePrompt returned %+v; want p-event", got)
	}

	if len(tx.synthCalls) != 0 {
		t.Errorf("SyntheticClaimsWithTx called %d times; want 0 (event must stay on admin pool)", len(tx.synthCalls))
	}
	if prompts.getSystemCalls != 1 {
		t.Errorf("PromptStore.GetSystem called %d times; want 1 (event must reach admin pool)", prompts.getSystemCalls)
	}
}

// TestTakeover_PreflightUsesSyntheticClaims pins fix #5 for Takeover.
// The preflight reads (run + task gates) must run under the
// requesting user's claims so an unauthorized runID surfaces as
// "not found" BEFORE any side effect (cancel handle drain, agent
// SIGKILL, worktree copy). The original code used GetSystem here,
// letting a user with a guessed runID destroy another user's run.
//
// In SQLite the synth-tx pass-through means Takeover still proceeds
// past the preflight; this test asserts only that the routing
// happened (the synth call was recorded with the right userID).
// Multi-mode RLS isolation lands in D9-core's pgtest matrix.
func TestTakeover_PreflightUsesSyntheticClaims(t *testing.T) {
	const runID = "run-takeover-spy"
	const callerID = "00000000-0000-0000-0000-000000000bbb"

	database := newTakeoverTestDB(t)
	seedRun(t, database, runID, "session-x", "/tmp/some-wt")

	stores := sqlitestore.New(database)
	tx := &recordingTxRunner{TxRunner: stores.Tx}
	s := NewSpawner(database, stores.Prompts, nil, nil, stores.Tasks, stores.AgentRuns, stores.Entities, stores.Reviews, stores.PendingPRs, stores.Events, stores.TaskMemory, stores.RunWorktrees, tx, nil, nil, "")

	// Takeover will fail downstream (no active cancel goroutine) but
	// that's after the preflight read; the spy capture is what we're
	// asserting.
	_, _ = s.Takeover(runmode.LocalDefaultOrg, runID, "/tmp/dest", callerID)

	if len(tx.synthCalls) != 1 {
		t.Fatalf("SyntheticClaimsWithTx called %d times; want exactly 1 (Takeover preflight must route through synth claims)", len(tx.synthCalls))
	}
	if tx.synthCalls[0].userID != callerID {
		t.Errorf("synth call userID = %q; want the calling user %q (preflight must check the caller's RLS scope, not a sentinel)", tx.synthCalls[0].userID, callerID)
	}
}

// TestRelease_PreflightUsesSyntheticClaims pins fix #5 for Release.
// Same shape as Takeover's preflight test: the run lookup that
// gates the teardown machinery must run under the caller's claims
// so a guessed runID belonging to another user surfaces as
// ErrReleaseNothingHeld before the path-safety + cleanup code runs.
func TestRelease_PreflightUsesSyntheticClaims(t *testing.T) {
	const runID = "run-release-spy"
	const callerID = "00000000-0000-0000-0000-000000000ccc"

	database := newTakeoverTestDB(t)
	seedRun(t, database, runID, "session-x", "/tmp/some-wt")
	// Release requires status='taken_over' to do anything past the
	// preflight; flip it so the seed row passes the post-read gate.
	if _, err := database.Exec(`UPDATE runs SET status = 'taken_over' WHERE id = ?`, runID); err != nil {
		t.Fatalf("flip status to taken_over: %v", err)
	}

	stores := sqlitestore.New(database)
	tx := &recordingTxRunner{TxRunner: stores.Tx}
	s := NewSpawner(database, stores.Prompts, nil, nil, stores.Tasks, stores.AgentRuns, stores.Entities, stores.Reviews, stores.PendingPRs, stores.Events, stores.TaskMemory, stores.RunWorktrees, tx, nil, nil, "")

	// Release will likely fail downstream on the path-safety check
	// (the seeded worktree_path is /tmp/some-wt which isn't under
	// the configured takeover base); that's after the preflight.
	_ = s.Release(runmode.LocalDefaultOrg, runID, callerID)

	if len(tx.synthCalls) != 1 {
		t.Fatalf("SyntheticClaimsWithTx called %d times; want exactly 1 (Release preflight must route through synth claims)", len(tx.synthCalls))
	}
	if tx.synthCalls[0].userID != callerID {
		t.Errorf("synth call userID = %q; want the calling user %q (preflight must check the caller's RLS scope, not a sentinel)", tx.synthCalls[0].userID, callerID)
	}
}

// Compile-time confirmation that the embedded-interface trick gives
// us full PromptStore + TxRunner satisfaction. If db adds a new
// method without a default impl, these break at compile time and
// flag the spy as needing an update.
var (
	_ db.PromptStore = (*recordingPromptStore)(nil)
	_ db.TxRunner    = (*recordingTxRunner)(nil)
)

// avoid unused-import warning when runmode isn't referenced directly
// (the constants are used implicitly via seedRun helpers).
var _ = runmode.LocalDefaultOrgID
