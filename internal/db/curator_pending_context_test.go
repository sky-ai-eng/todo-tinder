package db_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	_ "modernc.org/sqlite"
)

func newPendingContextDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database
}

func seedProjectWithSession(t *testing.T, database *sql.DB) (projectID, sessionID string) {
	t.Helper()
	id, err := db.CreateProject(database, domain.Project{Name: "p"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.SetProjectCuratorSessionID(database, id, "session-1"); err != nil {
		t.Fatalf("set session id: %v", err)
	}
	return id, "session-1"
}

// TestInsertPendingContext_DedupesOnConflict pins the coalescing
// contract: a second PATCH between user messages must NOT overwrite
// the first row's baseline_value. The earliest-unconsumed snapshot is
// the correct anchor for diffing at consume time, so the row that
// already exists wins.
func TestInsertPendingContext_DedupesOnConflict(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)

	if err := db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["c/d"]`); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	rows, err := db.ListPendingContext(database, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after dedupe, got %d", len(rows))
	}
	if rows[0].BaselineValue != `["a/b"]` {
		t.Errorf("baseline_value = %q, want %q (first insert wins)", rows[0].BaselineValue, `["a/b"]`)
	}
}

// TestInsertPendingContext_DistinctChangeTypesCoexist checks that the
// partial unique index only constrains rows of the same change_type —
// pinned_repos and jira can both be queued for the same session
// simultaneously.
func TestInsertPendingContext_DistinctChangeTypesCoexist(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)

	for _, change := range []string{
		domain.ChangeTypePinnedRepos,
		domain.ChangeTypeJiraProjectKey,
		domain.ChangeTypeLinearProjectKey,
	} {
		if err := db.InsertPendingContext(database, projectID, sessionID, change, "null"); err != nil {
			t.Fatalf("insert %s: %v", change, err)
		}
	}

	rows, err := db.ListPendingContext(database, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows for 3 change types, got %d", len(rows))
	}
}

// TestConsumePendingContext_ClaimsAndReturnsRows verifies the consume
// half of the lifecycle: rows transition from pending to claimed
// atomically, are returned to the caller, and a follow-up consume on
// the same session sees no pending rows.
func TestConsumePendingContext_ClaimsAndReturnsRows(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)
	requestID, _ := db.CreateCuratorRequest(database, projectID, "hi")

	if err := db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	claimed, err := db.ConsumePendingContext(database, projectID, sessionID, requestID)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed row, got %d", len(claimed))
	}
	if claimed[0].ConsumedByRequestID != requestID {
		t.Errorf("ConsumedByRequestID = %q, want %q", claimed[0].ConsumedByRequestID, requestID)
	}
	if claimed[0].ConsumedAt == nil {
		t.Error("ConsumedAt was not stamped")
	}

	// Second consume sees no pending rows — first call drained them.
	requestID2, _ := db.CreateCuratorRequest(database, projectID, "again")
	again, err := db.ConsumePendingContext(database, projectID, sessionID, requestID2)
	if err != nil {
		t.Fatalf("second consume: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("expected 0 rows on re-consume, got %d", len(again))
	}
}

// TestConsumePendingContext_DifferentSessionIsNoop checks that consume
// scopes by session: a pending row for session A is not picked up by
// a dispatch on session B (which happens after a session reset).
func TestConsumePendingContext_DifferentSessionIsNoop(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)
	requestID, _ := db.CreateCuratorRequest(database, projectID, "hi")

	if err := db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	claimed, err := db.ConsumePendingContext(database, projectID, "different-session", requestID)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("session scoping broken: claimed %d rows from a different session", len(claimed))
	}

	// Original row should still be pending.
	rows, _ := db.ListPendingContext(database, projectID)
	if len(rows) != 1 || rows[0].ConsumedAt != nil {
		t.Errorf("original row should still be pending, got %+v", rows)
	}
}

// TestFinalizePendingContext_DeletesConsumedRows is the success path:
// a successful agentproc.Run leads to FinalizePendingContext, which
// must remove the consumed rows so they don't get re-rendered next
// turn.
func TestFinalizePendingContext_DeletesConsumedRows(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)
	requestID, _ := db.CreateCuratorRequest(database, projectID, "hi")

	_ = db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["a/b"]`)
	_ = db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypeJiraProjectKey, `null`)
	if _, err := db.ConsumePendingContext(database, projectID, sessionID, requestID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := db.FinalizePendingContext(database, requestID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	rows, _ := db.ListPendingContext(database, projectID)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after finalize, got %d (%+v)", len(rows), rows)
	}
}

// TestRevertPendingContext_RestoresClaimedRows is the failure path: a
// transient agentproc failure must not lose the user's deltas. After
// revert, a fresh consume picks the same rows up again.
func TestRevertPendingContext_RestoresClaimedRows(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)
	requestID, _ := db.CreateCuratorRequest(database, projectID, "hi")

	_ = db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["a/b"]`)
	if _, err := db.ConsumePendingContext(database, projectID, sessionID, requestID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := db.RevertPendingContext(database, requestID); err != nil {
		t.Fatalf("revert: %v", err)
	}

	requestID2, _ := db.CreateCuratorRequest(database, projectID, "retry")
	again, err := db.ConsumePendingContext(database, projectID, sessionID, requestID2)
	if err != nil {
		t.Fatalf("re-consume: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("expected 1 row on re-consume after revert, got %d", len(again))
	}
	if again[0].BaselineValue != `["a/b"]` {
		t.Errorf("baseline_value lost across revert/re-consume: %q", again[0].BaselineValue)
	}
}

// TestRevertPendingContext_MergesMidDispatchPATCH covers the race that
// motivated the two-phase consume design: a PATCH lands during
// dispatch (after consume but before terminal), inserting a new
// pending row alongside the consumed one. Revert must merge — keep
// the older (consumed) row's baseline since it's the truer "earliest
// unconsumed snapshot," and drop the newer pending row.
func TestRevertPendingContext_MergesMidDispatchPATCH(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)
	requestID, _ := db.CreateCuratorRequest(database, projectID, "hi")

	// Earliest baseline (oldest unconsumed).
	_ = db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["original"]`)
	if _, err := db.ConsumePendingContext(database, projectID, sessionID, requestID); err != nil {
		t.Fatalf("consume: %v", err)
	}

	// Mid-dispatch PATCH inserts a fresh pending row alongside the
	// consumed one (allowed by the partial unique index).
	if err := db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["mid-dispatch"]`); err != nil {
		t.Fatalf("mid-dispatch insert: %v", err)
	}

	// Revert: should drop the mid-dispatch row and resurrect the
	// consumed one. Without merge logic, the partial unique index
	// would reject this with a constraint violation.
	if err := db.RevertPendingContext(database, requestID); err != nil {
		t.Fatalf("revert: %v", err)
	}

	rows, _ := db.ListPendingContext(database, projectID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after merge+revert, got %d (%+v)", len(rows), rows)
	}
	if rows[0].BaselineValue != `["original"]` {
		t.Errorf("merge picked wrong baseline: %q (should keep original, not mid-dispatch)", rows[0].BaselineValue)
	}
	if rows[0].ConsumedAt != nil {
		t.Error("row should be unconsumed after revert")
	}
}

// TestDeletePendingContextForSession is the session-reset cleanup
// hook: rows tied to a dead session id are removed wholesale so the
// new session's envelope doesn't get cluttered with deltas describing
// transitions the new agent never witnessed.
func TestDeletePendingContextForSession(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)

	_ = db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["a/b"]`)
	_ = db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypeJiraProjectKey, `null`)
	// Row tied to a different session — should not be touched.
	_ = db.InsertPendingContext(database, projectID, "other-session", domain.ChangeTypePinnedRepos, `["x/y"]`)

	if err := db.DeletePendingContextForSession(database, projectID, sessionID); err != nil {
		t.Fatalf("delete-for-session: %v", err)
	}

	rows, _ := db.ListPendingContext(database, projectID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row left (the other session's), got %d (%+v)", len(rows), rows)
	}
	if rows[0].CuratorSessionID != "other-session" {
		t.Errorf("wrong row survived: %+v", rows[0])
	}
}

// TestPendingContext_CascadeOnProjectDelete covers the FK cascade —
// a deleted project must not leave orphan pending rows behind. The
// test uses raw db.Exec on projects rather than going through the
// HTTP handler so it stays focused on the constraint behavior.
func TestPendingContext_CascadeOnProjectDelete(t *testing.T) {
	database := newPendingContextDB(t)
	projectID, sessionID := seedProjectWithSession(t, database)
	_ = db.InsertPendingContext(database, projectID, sessionID, domain.ChangeTypePinnedRepos, `["a/b"]`)

	if _, err := database.Exec(`DELETE FROM projects WHERE id = ?`, projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	rows, _ := db.ListPendingContext(database, projectID)
	if len(rows) != 0 {
		t.Errorf("FK cascade missed pending rows: %+v", rows)
	}
}
