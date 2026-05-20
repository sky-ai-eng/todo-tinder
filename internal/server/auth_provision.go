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
	// Fast path: every repeat login lands here with the user already
	// having memberships. Doing this lookup outside the tx + advisory
	// lock keeps the common path to a single round-trip with no lock
	// contention. The locked re-check below preserves race safety for
	// the genuinely-first-signup case.
	if existing, err := s.lookupEarliestMembership(ctx, s.db, userID); err != nil {
		return provisionResult{}, fmt.Errorf("membership pre-check: %w", err)
	} else if existing.Valid {
		return provisionResult{activeOrgID: existing}, nil
	}

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

	// Re-check inside the lock. The fast path above may have observed
	// zero memberships against a snapshot taken before a concurrent
	// provisioning tx committed; this read sees that tx's writes once
	// we hold the lock.
	existing, err := s.lookupEarliestMembership(ctx, tx, userID)
	if err != nil {
		return provisionResult{}, fmt.Errorf("membership re-check: %w", err)
	}
	if existing.Valid {
		// Concurrent callback won the race — commit the lock release
		// and return without provisioning.
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

// membershipQueryer is the slice of *sql.DB / *sql.Tx that
// lookupEarliestMembership needs. Defined locally so the helper can
// run against either the connection pool (fast-path pre-check) or
// the locked transaction (race-safe re-check).
type membershipQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// lookupEarliestMembership returns the user's earliest org membership
// (created_at ASC, org_id ASC as deterministic tiebreak), or
// uuid.NullUUID{Valid: false} if the user has zero memberships.
// sql.ErrNoRows is folded into the Valid=false return so callers
// only branch on Valid.
func (s *Server) lookupEarliestMembership(ctx context.Context, q membershipQueryer, userID uuid.UUID) (uuid.NullUUID, error) {
	var existing uuid.NullUUID
	err := q.QueryRowContext(ctx, `
		SELECT org_id
		  FROM public.org_memberships
		 WHERE user_id = $1
		 ORDER BY created_at ASC, org_id ASC
		 LIMIT 1
	`, userID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return uuid.NullUUID{}, err
	}
	return existing, nil
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

	// Insert with `ON CONFLICT (slug) DO NOTHING RETURNING id`,
	// retrying on a no-rows return (= slug already taken). This
	// closes the cross-user race that a separate "SELECT EXISTS
	// then INSERT" pair leaves open: two concurrent signups for
	// users sharing a display name would both see EXISTS=false on
	// the same candidate, then one's INSERT blocks until the
	// other commits and crashes with unique_violation. With ON
	// CONFLICT DO NOTHING, the loser cleanly observes zero rows
	// affected and we advance to the next candidate.
	//
	// The orgs INSERT itself is RLS-checked (`owner_user_id =
	// tf.current_user_id()`), but the auth callback runs against
	// the admin pool which is BYPASSRLS for tf_app's policies —
	// so we rely on the owner_user_id FK invariant by setting it
	// explicitly here.
	var orgID uuid.UUID
	var chosenSlug string
	for i := 0; i < 64; i++ {
		candidate := slugBase
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", slugBase, i+1)
		}
		var got uuid.NullUUID
		err := tx.QueryRowContext(ctx, `
			INSERT INTO public.orgs (slug, name, owner_user_id, is_personal)
			VALUES ($1, $2, $3, true)
			ON CONFLICT (slug) DO NOTHING
			RETURNING id
		`, candidate, name, userID).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			// Slug taken by a concurrent or prior signup — try next.
			continue
		}
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert orgs: %w", err)
		}
		if !got.Valid {
			continue
		}
		orgID = got.UUID
		chosenSlug = candidate
		break
	}
	if orgID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("could not allocate slug starting from %q after 64 tries", slugBase)
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

	log.Printf("[auth] provisioned personal org=%s team=%s user=%s slug=%s", orgID, teamID, userID, chosenSlug)
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
			// recoverable: create the default team. The user then falls
			// through to the same role='member' inserts below — the
			// existing-Default-org branch is by definition a non-first
			// signup, so 'member' is the correct role regardless of
			// whether we had to backfill the missing team here.
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
// the same user serialize on the same key. We hash the raw 16-byte
// UUID array (`userID[:]`), not the string form, so the keying is
// independent of any future change in how we render UUIDs at call
// sites. Different users get different keys with overwhelming
// probability (FNV-1a on 16 bytes has ample entropy).
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

// slugifyAllowed matches the closed set of bytes allowed in a slug:
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
