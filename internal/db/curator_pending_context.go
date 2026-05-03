package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InsertPendingContext queues a context-change delta for the next
// curator dispatch on (projectID, sessionID, changeType). Coalescing
// is enforced by the partial unique index on
// (project_id, curator_session_id, change_type) WHERE consumed_at IS
// NULL: a second PATCH between user messages hits ON CONFLICT DO
// NOTHING and the *earliest* baseline_value wins, which is the
// correct "snapshot before the first unconsumed change" anchor for
// diffing at consume time. baselineJSON must be a JSON-encoded
// representation of the value before this PATCH applied (an array
// for pinned_repos, a scalar string or JSON null for tracker keys).
//
// Caller is responsible for ensuring sessionID is non-empty — there
// is no point queueing pending rows for a project whose Curator has
// never been spun up, since the next session's static envelope will
// render fresh values directly.
func InsertPendingContext(database *sql.DB, projectID, sessionID, changeType, baselineJSON string) error {
	_, err := database.Exec(`
		INSERT INTO curator_pending_context
			(project_id, curator_session_id, change_type, baseline_value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, projectID, sessionID, changeType, baselineJSON)
	return err
}

// ConsumePendingContext atomically claims every unconsumed row for
// (projectID, sessionID), tagging them with requestID + the current
// timestamp, and returns them ordered by created_at so the caller can
// render them in the order they were queued.
//
// The UPDATE-then-SELECT runs in a single immediate transaction so a
// PATCH that lands during the window cannot be claimed twice, and the
// caller observes exactly the rows it owns. Returns an empty slice
// (not nil) when there is nothing pending — callers can range over
// the result without a nil-check.
func ConsumePendingContext(database *sql.DB, projectID, sessionID, requestID string) ([]domain.CuratorPendingContext, error) {
	tx, err := database.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		UPDATE curator_pending_context
		   SET consumed_at = ?, consumed_by_request_id = ?
		 WHERE project_id = ?
		   AND curator_session_id = ?
		   AND consumed_at IS NULL
	`, now, requestID, projectID, sessionID); err != nil {
		return nil, fmt.Errorf("claim pending rows: %w", err)
	}

	rows, err := tx.Query(`
		SELECT id, project_id, curator_session_id, change_type, baseline_value,
		       consumed_at, consumed_by_request_id, created_at
		  FROM curator_pending_context
		 WHERE consumed_by_request_id = ?
		 ORDER BY created_at ASC, id ASC
	`, requestID)
	if err != nil {
		return nil, fmt.Errorf("read claimed rows: %w", err)
	}
	defer rows.Close()

	out := []domain.CuratorPendingContext{}
	for rows.Next() {
		row, err := scanPendingContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// FinalizePendingContext deletes every row consumed by requestID. Called
// from the per-project goroutine after agentproc returns a `done`
// terminal — the agent has seen the deltas, so they can be retired.
// Idempotent: a request that consumed zero rows produces a zero-row
// delete, which is fine.
func FinalizePendingContext(database *sql.DB, requestID string) error {
	_, err := database.Exec(`
		DELETE FROM curator_pending_context
		 WHERE consumed_by_request_id = ?
	`, requestID)
	return err
}

// RevertPendingContext un-consumes the rows claimed by requestID so
// the next user message picks them up again. Used on terminal
// `cancelled` or `failed` so a transient agentproc failure (model auth
// error, network blip, user cancel) doesn't silently lose the user's
// deltas.
//
// Merge: a NEW PATCH may have landed during dispatch (the partial
// unique index excludes consumed rows, so a fresh pending row could be
// inserted alongside the consumed one). Two unconsumed rows for the
// same (session, change_type) would violate the unique constraint on
// revert. The older (consumed-but-being-reverted) row's baseline is
// the truer "earliest unconsumed snapshot" — it covers the entire
// window from the original PATCH through the new one — so we drop
// the newer row in its favor.
func RevertPendingContext(database *sql.DB, requestID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		DELETE FROM curator_pending_context
		 WHERE consumed_at IS NULL
		   AND (project_id, curator_session_id, change_type) IN (
		       SELECT project_id, curator_session_id, change_type
		         FROM curator_pending_context
		        WHERE consumed_by_request_id = ?
		   )
	`, requestID); err != nil {
		return fmt.Errorf("merge pending rows: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE curator_pending_context
		   SET consumed_at = NULL, consumed_by_request_id = NULL
		 WHERE consumed_by_request_id = ?
	`, requestID); err != nil {
		return fmt.Errorf("revert pending rows: %w", err)
	}

	return tx.Commit()
}

// DeletePendingContextForSession removes every pending or consumed row
// for a given (projectID, sessionID) — used when the session is reset
// (orphan-cleanup, future user-driven "fresh chat" action). The new
// session's static envelope renders current values directly, so any
// pending diff against the dead session would just be noise.
//
// Project deletion is handled by the FK CASCADE; this helper is for
// the session-only reset case where the project row stays but its
// curator_session_id flips.
func DeletePendingContextForSession(database *sql.DB, projectID, sessionID string) error {
	_, err := database.Exec(`
		DELETE FROM curator_pending_context
		 WHERE project_id = ? AND curator_session_id = ?
	`, projectID, sessionID)
	return err
}

// ListPendingContext returns every row for a project regardless of
// session or consumption state. Test-only / debugging surface — the
// curator runtime never needs this. Lives in db so tests can assert
// on the raw table shape without poking sql directly.
func ListPendingContext(database *sql.DB, projectID string) ([]domain.CuratorPendingContext, error) {
	rows, err := database.Query(`
		SELECT id, project_id, curator_session_id, change_type, baseline_value,
		       consumed_at, consumed_by_request_id, created_at
		  FROM curator_pending_context
		 WHERE project_id = ?
		 ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorPendingContext{}
	for rows.Next() {
		row, err := scanPendingContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func scanPendingContext(scanner interface {
	Scan(dest ...any) error
}) (domain.CuratorPendingContext, error) {
	var (
		row        domain.CuratorPendingContext
		consumedAt sql.NullTime
		consumedBy sql.NullString
	)
	if err := scanner.Scan(
		&row.ID, &row.ProjectID, &row.CuratorSessionID, &row.ChangeType,
		&row.BaselineValue, &consumedAt, &consumedBy, &row.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.CuratorPendingContext{}, nil
		}
		return domain.CuratorPendingContext{}, err
	}
	if consumedAt.Valid {
		t := consumedAt.Time
		row.ConsumedAt = &t
	}
	row.ConsumedByRequestID = consumedBy.String
	return row, nil
}
