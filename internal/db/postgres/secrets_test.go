package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestSecretStore_Postgres_RoundTrip exercises the full Put / Get /
// Delete cycle through the real public.vault_* SECURITY DEFINER
// functions. Runs inside a WithUser tx so the vault wrappers'
// p_org_id = tf.current_org_id() gate is satisfied — that gate is
// what makes the secret subsystem safe against a claims-less caller
// reading any org's data.
//
// We construct the store against the tx with pgstore.NewForTx(tx)
// so every call rides the same connection that has SET LOCAL ROLE
// tf_app + the JWT claim. Without this the vault function refuses
// with "missing org context" — the right failure mode, but not
// what this test covers.
func TestSecretStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)

		// Put a new secret.
		if err := stores.Secrets.Put(ctx, orgID, "github_pat", "ghp_alice_secret_v1", "primary GitHub token"); err != nil {
			return fmt.Errorf("Put: %w", err)
		}

		// Round-trip: Get returns the stored value.
		got, err := stores.Secrets.Get(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Get: %w", err)
		}
		if got != "ghp_alice_secret_v1" {
			t.Errorf("Get got=%q want ghp_alice_secret_v1", got)
		}

		// Rotation: Put on the same key overwrites.
		if err := stores.Secrets.Put(ctx, orgID, "github_pat", "ghp_alice_secret_v2", ""); err != nil {
			return fmt.Errorf("Put rotation: %w", err)
		}
		got, err = stores.Secrets.Get(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Get after rotation: %w", err)
		}
		if got != "ghp_alice_secret_v2" {
			t.Errorf("after rotation got=%q want ghp_alice_secret_v2", got)
		}

		// Missing key: Get returns "" without an error so callers can
		// distinguish "not configured" from "fetch failed."
		got, err = stores.Secrets.Get(ctx, orgID, "nonexistent_key")
		if err != nil {
			return fmt.Errorf("Get missing: %w", err)
		}
		if got != "" {
			t.Errorf("missing key got=%q want empty", got)
		}

		// Delete returns ok=true on a present key.
		ok, err := stores.Secrets.Delete(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Delete: %w", err)
		}
		if !ok {
			t.Errorf("Delete ok=false for present key; want true")
		}

		// Subsequent Get returns "" (the row is gone).
		got, err = stores.Secrets.Get(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Get after delete: %w", err)
		}
		if got != "" {
			t.Errorf("after Delete got=%q want empty", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestSecretStore_Postgres_MismatchedOrgIDRefused pins the vault
// wrapper's claim-vs-arg gate. Calling with an orgID that doesn't
// match the JWT claim's org_id must fail — otherwise a session
// for org A could read org B's secrets. The wrapper raises an
// exception (not a NULL); we just confirm the error propagates.
func TestSecretStore_Postgres_MismatchedOrgIDRefused(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA := seedPgOrgAndUserForSecrets(t, h)
	orgB, _ := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		// Caller has claims for orgA, but passes orgB as the param.
		// vault_put_org_secret must refuse — otherwise a stolen
		// session could write to any org.
		return stores.Secrets.Put(ctx, orgB, "github_pat", "stolen", "")
	})
	if err == nil {
		t.Fatalf("Put with mismatched orgID succeeded; vault gate broken")
	}
}

// TestSecretStore_Postgres_NonTfAppRoleRefused pins the grant matrix
// on the public.vault_* wrappers. They are REVOKE'd from
// anon/authenticated/service_role and GRANT'd only to tf_app — a
// session that connects via the authenticator → tf_app path but
// switches to a different role inside the tx must fail with
// permission-denied (SQLSTATE 42501).
//
// Why this matters separately from the cross-org test: the cross-org
// case proves the wrapper body enforces tenant isolation; this test
// proves the wrapper itself is unreachable from any role that isn't
// our app role. Together they bracket the surface: even if a future
// migration introduced a Supabase-default role that ends up with a
// pooled connection, the GRANT shape would still block the call.
func TestSecretStore_Postgres_NonTfAppRoleRefused(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// authenticator is granted both tf_app (custom) and the standard
	// Supabase roles (anon/authenticated/service_role). Switching to
	// `authenticated` for the wrapper call exercises the REVOKE branch
	// without needing a separate connection or password.
	for _, role := range []string{"anon", "authenticated", "service_role"} {
		role := role
		t.Run(role, func(t *testing.T) {
			tx, err := h.AppDB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+role); err != nil {
				t.Fatalf("set role %s: %v", role, err)
			}
			// Claims set so the wrapper would otherwise satisfy its
			// own gate — the permission denial must fire *before* the
			// body ever runs.
			claims := fmt.Sprintf(`{"sub":"%s","org_id":"%s"}`, userID, orgID)
			if _, err := tx.ExecContext(ctx, `SELECT set_config('request.jwt.claims', $1, true)`, claims); err != nil {
				t.Fatalf("set claims: %v", err)
			}

			// Each wrapper aborts the tx on permission denial. Wrap
			// every call in a savepoint so the next probe runs on a
			// clean slate and we can assert each wrapper independently.
			probe := func(label, query string) {
				t.Helper()
				if _, err := tx.ExecContext(ctx, `SAVEPOINT sp`); err != nil {
					t.Fatalf("%s: savepoint: %v", label, err)
				}
				_, err := tx.ExecContext(ctx, query, orgID)
				pgtest.AssertRLSViolation(t, err)
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp`); err != nil {
					t.Fatalf("%s: rollback to savepoint: %v", label, err)
				}
			}
			probe("put", `SELECT public.vault_put_org_secret($1::uuid, 'k', 'v', NULL)`)
			probe("get", `SELECT public.vault_get_org_secret($1::uuid, 'k')`)
			probe("delete", `SELECT public.vault_delete_org_secret($1::uuid, 'k')`)
		})
	}
}

func seedPgOrgAndUserForSecrets(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("secret-conf-%s@test.local", userID[:8])
	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, "Secret Conformance User"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Secret Conformance Org "+orgID[:8], "secret-"+orgID[:8], userID); err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID); err != nil {
		t.Fatalf("seed org_memberships: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}
