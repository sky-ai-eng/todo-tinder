package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SKY-345 — auto-provisioning of org/team/memberships on first
// signup in multi mode. Called from handleOAuthCallback after the
// public.users row exists and before the session is created. The
// outcome is encoded in provisionResult so the caller can branch on
// whether to redirect into the app or to surface the rare-state
// /no-orgs page.
//
// All three policies share the same race-safety pattern:
//
//  1. BEGIN
//  2. pg_advisory_xact_lock(hash(user_id)) — serializes concurrent
//     callbacks for the same user. Released automatically on commit
//     or rollback.
//  3. SELECT 1 FROM org_memberships WHERE user_id=$1 — if any row
//     exists, the user already has memberships; skip provisioning.
//  4. Apply the policy.
//  5. COMMIT.
//
// We deliberately don't rely on a unique constraint on
// (owner_user_id, is_personal) — the data model allows a user to own
// multiple personal orgs in edge cases (legitimate even if rare). The
// advisory lock + inside-tx zero-membership check is the canonical
// enforcement point.

// provisionResult tells the caller what happened. Lets the callback
// decide whether to redirect into the app (provisioned or already had
// memberships) or to land the user at /no-orgs (invite-only with no
// pending invite).
type provisionResult struct {
	// activeOrgID is the org the session should default to. Set to a
	// real UUID when the user has at least one membership after this
	// call; uuid.Nil when invite-only didn't auto-join anywhere.
	activeOrgID uuid.NullUUID
}

// provisionUserOrgs applies the configured join policy when the user
// has zero memberships. Idempotent against concurrent callbacks for
// the same user (advisory lock).
//
// Errors here are fatal to the signup callback — better to fail
// loudly than to ship a half-provisioned user.
func (s *Server) provisionUserOrgs(ctx context.Context, userID uuid.UUID, claims *verify.Claims) (provisionResult, error) {
	policy := runmode.JoinPolicyCurrent()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return provisionResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Per-user advisory lock. Two concurrent callbacks for the same
	// user serialize here; one provisions, the other sees the
	// org_memberships row inside the same tx-visible state on its
	// subsequent SELECT and falls through to the "already has
	// memberships" branch.
	lockKey := userLockKey(userID)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return provisionResult{}, fmt.Errorf("acquire advisory lock: %w", err)
	}

	// Re-check inside the lock: did a concurrent tx already provision
	// for this user? If so, surface their earliest org as the active
	// one and short-circuit.
	var existing uuid.NullUUID
	if err := tx.QueryRowContext(ctx, `
		SELECT org_id
		  FROM public.org_memberships
		 WHERE user_id = $1
		 ORDER BY created_at ASC, org_id ASC
		 LIMIT 1
	`, userID).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return provisionResult{}, fmt.Errorf("membership re-check: %w", err)
	}
	if existing.Valid {
		// Either a prior signup callback (retry) or a concurrent
		// callback won the race — commit the lock release and return
		// without provisioning.
		if err := tx.Commit(); err != nil {
			return provisionResult{}, fmt.Errorf("commit: %w", err)
		}
		return provisionResult{activeOrgID: existing}, nil
	}

	var activeOrg uuid.NullUUID
	switch policy {
	case runmode.JoinPolicyPersonalOrgOnSignup:
		orgID, perr := provisionPersonalOrg(ctx, tx, userID, claims)
		if perr != nil {
			return provisionResult{}, perr
		}
		activeOrg = uuid.NullUUID{UUID: orgID, Valid: true}
	case runmode.JoinPolicyAutoJoinDefault:
		orgID, perr := provisionAutoJoinDefault(ctx, tx, userID)
		if perr != nil {
			return provisionResult{}, perr
		}
		activeOrg = uuid.NullUUID{UUID: orgID, Valid: true}
	case runmode.JoinPolicyInviteOnly:
		// Nothing to do — the user stays at zero memberships and the
		// frontend AuthGate routes to /no-orgs. The rare-state page
		// reads /api/me's join_policy to render the admin-gated copy.
	default:
		return provisionResult{}, fmt.Errorf("unknown join policy %q", policy)
	}

	if err := tx.Commit(); err != nil {
		return provisionResult{}, fmt.Errorf("commit: %w", err)
	}
	return provisionResult{activeOrgID: activeOrg}, nil
}

// provisionPersonalOrg creates the user's personal org + default team
// + memberships in a single transaction. The caller owns the BEGIN/
// COMMIT so an error here propagates as a rollback of the whole
// signup callback.
//
// Name derivation prefers the GoTrue user_metadata's display name
// (`{display}'s Personal`), falling back to the email local-part
// (`{local-part}`). Slug is the kebab-cased + ASCII-normalized form
// of the name, with a counter suffix on collision.
func provisionPersonalOrg(ctx context.Context, tx *sql.Tx, userID uuid.UUID, claims *verify.Claims) (uuid.UUID, error) {
	name, slugBase := personalOrgNameAndSlug(claims)
	slug, err := allocateSlug(ctx, tx, slugBase)
	if err != nil {
		return uuid.Nil, fmt.Errorf("allocate slug: %w", err)
	}

	// The orgs INSERT triggers an RLS check `owner_user_id =
	// tf.current_user_id()` (see baseline). The auth callback path
	// uses the admin pool which sets request.jwt.claims via the
	// transaction's local SET — but we're operating on the admin pool
	// without claims context here. Admin pool is BYPASSRLS-equivalent
	// for tf_app's policies because we connect as supabase_admin, so
	// the policy check is moot. The orgs.owner_user_id FK to users(id)
	// is the load-bearing invariant; we set it explicitly to userID.
	var orgID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO public.orgs (slug, name, owner_user_id, is_personal)
		VALUES ($1, $2, $3, true)
		RETURNING id
	`, slug, name, userID).Scan(&orgID); err != nil {
		return uuid.Nil, fmt.Errorf("insert orgs: %w", err)
	}

	var teamID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO public.teams (org_id, slug, name, created_by_user_id)
		VALUES ($1, 'default', 'Default', $2)
		RETURNING id
	`, orgID, userID).Scan(&teamID); err != nil {
		return uuid.Nil, fmt.Errorf("insert teams: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.org_memberships (user_id, org_id, role)
		VALUES ($1, $2, 'owner')
	`, userID, orgID); err != nil {
		return uuid.Nil, fmt.Errorf("insert org_memberships: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.memberships (user_id, team_id, role)
		VALUES ($1, $2, 'admin')
	`, userID, teamID); err != nil {
		return uuid.Nil, fmt.Errorf("insert memberships: %w", err)
	}

	log.Printf("[auth] provisioned personal org=%s team=%s user=%s slug=%s", orgID, teamID, userID, slug)
	return orgID, nil
}

// provisionAutoJoinDefault adds the user to the instance's existing
// "Default" org (id = runmode.LocalDefaultOrgID) as a member on its
// default team. If the Default org doesn't exist yet (the first
// signup on a fresh self-host), it's created with this user as admin
// — matching the "first signup → admin of Default org" pattern from
// the ticket's deployment-shape table.
func provisionAutoJoinDefault(ctx context.Context, tx *sql.Tx, userID uuid.UUID) (uuid.UUID, error) {
	defaultOrgID := uuid.MustParse(runmode.LocalDefaultOrgID)

	// Try to read the existing default org's id.
	var existingOrg uuid.NullUUID
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM public.orgs WHERE id = $1
	`, defaultOrgID).Scan(&existingOrg); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("read default org: %w", err)
	}

	if !existingOrg.Valid {
		// First user on a fresh `auto-join-default` install. Create
		// the Default org with this user as owner+admin. The pinned
		// id matches the local-mode sentinel so a multi-mode install
		// that later swaps modes (or operators that script against
		// the known id) see consistent data.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO public.orgs (id, slug, name, owner_user_id)
			VALUES ($1, 'default', 'Default', $2)
		`, defaultOrgID, userID); err != nil {
			return uuid.Nil, fmt.Errorf("insert default org: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO public.teams (id, org_id, slug, name, created_by_user_id)
			VALUES ($1, $2, 'default', 'Default', $3)
		`, uuid.MustParse(runmode.LocalDefaultTeamID), defaultOrgID, userID); err != nil {
			return uuid.Nil, fmt.Errorf("insert default team: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO public.org_memberships (user_id, org_id, role)
			VALUES ($1, $2, 'owner')
		`, userID, defaultOrgID); err != nil {
			return uuid.Nil, fmt.Errorf("insert org_memberships: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO public.memberships (user_id, team_id, role)
			VALUES ($1, $2, 'admin')
		`, userID, uuid.MustParse(runmode.LocalDefaultTeamID)); err != nil {
			return uuid.Nil, fmt.Errorf("insert memberships: %w", err)
		}
		log.Printf("[auth] auto-join-default: created Default org for first user=%s", userID)
		return defaultOrgID, nil
	}

	// Default org exists — find its default team (oldest by
	// created_at, with id-asc tiebreak) and add the user as a
	// member.
	var teamID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT id
		  FROM public.teams
		 WHERE org_id = $1
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1
	`, defaultOrgID).Scan(&teamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Default org exists but has no team — bootstrap bug, but
			// recoverable: create the default team and add the user as
			// admin (member of the team's first row gets a leg up).
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO public.teams (org_id, slug, name, created_by_user_id)
				VALUES ($1, 'default', 'Default', $2)
				RETURNING id
			`, defaultOrgID, userID).Scan(&teamID); err != nil {
				return uuid.Nil, fmt.Errorf("create missing default team: %w", err)
			}
		} else {
			return uuid.Nil, fmt.Errorf("read default team: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.org_memberships (user_id, org_id, role)
		VALUES ($1, $2, 'member')
	`, userID, defaultOrgID); err != nil {
		return uuid.Nil, fmt.Errorf("insert org_memberships: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.memberships (user_id, team_id, role)
		VALUES ($1, $2, 'member')
	`, userID, teamID); err != nil {
		return uuid.Nil, fmt.Errorf("insert memberships: %w", err)
	}

	log.Printf("[auth] auto-join-default: added user=%s to Default org team=%s", userID, teamID)
	return defaultOrgID, nil
}

// userLockKey hashes a user UUID into a 64-bit signed integer for
// pg_advisory_xact_lock. The hash is stable per-process — Go's hash/fnv
// is deterministic on the same input — so two concurrent callbacks for
// the same user serialize on the same key. Different users get
// different keys with overwhelming probability (FNV-1a on a 36-char
// UUID has ample entropy).
func userLockKey(userID uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write(userID[:])
	// Cast to int64 — pg_advisory_xact_lock takes bigint and tolerates
	// negative values; sign bit doesn't change collision behavior.
	return int64(h.Sum64())
}

// personalOrgNameAndSlug derives the display name + slug seed for a
// fresh personal org. Prefers the GoTrue user_metadata display name
// ("Aidan Allchin" → "Aidan Allchin's Personal" / "aidan-allchin");
// falls back to the email local-part ("aidan@allchin.com" →
// "aidan" / "aidan"). The returned slug is a seed — collisions get
// resolved by allocateSlug appending a counter.
func personalOrgNameAndSlug(claims *verify.Claims) (name, slugSeed string) {
	display := ""
	if claims != nil && claims.UserMetadata != nil {
		if v, ok := claims.UserMetadata["full_name"].(string); ok {
			display = strings.TrimSpace(v)
		}
		if display == "" {
			if v, ok := claims.UserMetadata["name"].(string); ok {
				display = strings.TrimSpace(v)
			}
		}
	}

	if display != "" {
		// Strip a trailing "'s" or "s" if present in the display
		// (rare; defensive against weird OAuth payloads), then
		// suffix with "'s Personal" for the human label.
		name = display + "'s Personal"
		slugSeed = slugify(display)
		if slugSeed != "" {
			return name, slugSeed
		}
	}

	// Fall back to the email local-part.
	local := ""
	if claims != nil {
		if i := strings.Index(claims.Email, "@"); i > 0 {
			local = claims.Email[:i]
		}
	}
	if local == "" {
		// Last resort: a stable but opaque label. The signup callback
		// caller would have validated the JWT before this; a
		// JWT-claims-without-email-or-display payload is unusual but
		// not fatal — we want SOME orientation in the data, not a
		// blank row.
		name = "Personal"
		slugSeed = "personal"
		return
	}

	name = local
	slugSeed = slugify(local)
	if slugSeed == "" {
		slugSeed = "personal"
	}
	return
}

// slugifyKeep is the closed set of bytes allowed in a slug:
// ASCII lowercase letters, digits, and hyphens. Everything else
// becomes a hyphen; runs of hyphens collapse; leading/trailing
// hyphens get trimmed.
var slugifyAllowed = regexp.MustCompile(`[a-z0-9]+`)

// slugify converts an arbitrary string into a URL-safe kebab-case
// ASCII slug. Empty input or input that contains no [a-z0-9]
// characters returns "" — callers can substitute a default ("personal"
// in our case).
func slugify(in string) string {
	low := strings.ToLower(strings.TrimSpace(in))
	parts := slugifyAllowed.FindAllString(low, -1)
	if len(parts) == 0 {
		return ""
	}
	out := strings.Join(parts, "-")
	// Cap length at 48 chars — slugs land in URLs and we don't want
	// unbounded growth from a weirdly-long display name.
	if len(out) > 48 {
		out = strings.TrimRight(out[:48], "-")
	}
	return out
}

// allocateSlug returns a slug that doesn't collide with any existing
// orgs.slug, starting from seed and appending "-2", "-3", ... as
// needed. Runs inside the caller's transaction; the orgs.slug UNIQUE
// constraint prevents two callers from racing to the same slug at
// COMMIT time (the second would fail with a unique violation).
//
// We try up to 64 candidates before giving up — far beyond any
// plausible user-name collision rate. After that, callers should
// consider it a misconfiguration (the seed is too generic) and the
// error propagates as a signup-callback failure.
func allocateSlug(ctx context.Context, tx *sql.Tx, seed string) (string, error) {
	if seed == "" {
		seed = "personal"
	}
	for i := 0; i < 64; i++ {
		candidate := seed
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", seed, i+1)
		}
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM public.orgs WHERE slug = $1)`,
			candidate,
		).Scan(&exists); err != nil {
			return "", fmt.Errorf("check slug %q: %w", candidate, err)
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not allocate slug starting from %q after 64 tries", seed)
}
