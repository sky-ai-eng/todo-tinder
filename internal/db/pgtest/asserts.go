package pgtest

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// AssertRLSViolation fails the test unless err is a Postgres
// insufficient_privilege error (SQLSTATE 42501) — the canonical signal
// that a row-level security policy denied a query. Postgres also raises
// 42501 for plain GRANT failures; in the test contexts that call this
// helper the role and grants are fixed by the harness, so 42501 in
// practice means RLS rejected the row.
//
// Use this in cross-org and cross-user test cases where the policy is
// expected to reject the write (or filter the read to a violation in
// the strict cases). Pin the code so a noisy error-message rewrite
// downstream doesn't silently weaken the assertion.
func AssertRLSViolation(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected RLS violation, got nil error (row was not blocked)")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "42501" {
		t.Fatalf("expected SQLSTATE 42501 (insufficient_privilege / RLS), got %s: %v", pgErr.Code, err)
	}
}
