package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ---- pure helpers (no DB) ----

func TestSlugify(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Aidan Allchin", "aidan-allchin"},
		{"  Aidan  Allchin  ", "aidan-allchin"},
		{"O'Brien", "o-brien"},
		{"hello world!!!", "hello-world"},
		{"アイダン", ""}, // no [a-z0-9] survives — caller falls back
		{"aidan@allchin.com", "aidan-allchin-com"},
		{"", ""},
		{"   ", ""},
		{strings.Repeat("a", 100), strings.Repeat("a", 48)},
		{"a-b-c", "a-b-c"},
		{"A_B_C", "a-b-c"},
	}
	for _, tc := range cases {
		got := slugify(tc.in)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPersonalOrgNameAndSlug_DisplayNamePreferred(t *testing.T) {
	c := &verify.Claims{
		Email: "aidan@allchin.com",
		UserMetadata: map[string]any{
			"full_name": "Aidan Allchin",
		},
	}
	name, slug := personalOrgNameAndSlug(c)
	if name != "Aidan Allchin's Personal" {
		t.Errorf("name = %q, want %q", name, "Aidan Allchin's Personal")
	}
	if slug != "aidan-allchin" {
		t.Errorf("slug = %q, want %q", slug, "aidan-allchin")
	}
}

func TestPersonalOrgNameAndSlug_FallsBackToName(t *testing.T) {
	c := &verify.Claims{
		Email: "aidan@allchin.com",
		UserMetadata: map[string]any{
			"name": "Aidan",
		},
	}
	name, slug := personalOrgNameAndSlug(c)
	if name != "Aidan's Personal" {
		t.Errorf("name = %q, want %q", name, "Aidan's Personal")
	}
	if slug != "aidan" {
		t.Errorf("slug = %q, want %q", slug, "aidan")
	}
}

func TestPersonalOrgNameAndSlug_FallsBackToEmailLocalPart(t *testing.T) {
	c := &verify.Claims{Email: "aidan@allchin.com"}
	name, slug := personalOrgNameAndSlug(c)
	if name != "aidan" {
		t.Errorf("name = %q, want %q", name, "aidan")
	}
	if slug != "aidan" {
		t.Errorf("slug = %q, want %q", slug, "aidan")
	}
}

func TestPersonalOrgNameAndSlug_LastResort(t *testing.T) {
	c := &verify.Claims{}
	name, slug := personalOrgNameAndSlug(c)
	if name != "Personal" {
		t.Errorf("name = %q, want %q", name, "Personal")
	}
	if slug != "personal" {
		t.Errorf("slug = %q, want %q", slug, "personal")
	}
}

func TestPersonalOrgNameAndSlug_NonASCIIDisplayNameFallsThrough(t *testing.T) {
	// Display name has nothing slugifiable — we fall through to the
	// email local-part instead of returning a "personal" slug.
	c := &verify.Claims{
		Email: "u@x.com",
		UserMetadata: map[string]any{
			"full_name": "アイダン",
		},
	}
	name, slug := personalOrgNameAndSlug(c)
	if slug != "u" {
		t.Errorf("slug fallback path: got %q, want %q", slug, "u")
	}
	// Name should NOT carry the broken display — caller would render
	// a "'s Personal" suffix on an unhelpful base. The fallback uses
	// the local-part as both name and slug seed.
	if name != "u" {
		t.Errorf("name = %q, want %q (email-local fallback)", name, "u")
	}
}

// userLockKey: same input → same hash; different inputs → different
// hashes. The latter is probabilistic but FNV-1a on a UUID byte array
// has plenty of entropy for our purposes.
func TestUserLockKey_Deterministic(t *testing.T) {
	u := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	if got1, got2 := userLockKey(u), userLockKey(u); got1 != got2 {
		t.Errorf("userLockKey non-deterministic: %d vs %d", got1, got2)
	}
}

func TestUserLockKey_DifferentUsersDifferentKeys(t *testing.T) {
	a := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	b := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	if userLockKey(a) == userLockKey(b) {
		t.Errorf("userLockKey collided across distinct UUIDs")
	}
}

// ---- pg-integration: one-off per policy + concurrent-race idempotency ----

func TestProvisionUserOrgs_PersonalOrgOnSignup(t *testing.T) {
	r := newAuthRig(t)
	runmode.SetJoinPolicyForTest(t, runmode.JoinPolicyPersonalOrgOnSignup)

	userID := r.seedUser()
	claims := &verify.Claims{
		Email: "aidan@allchin.com",
		UserMetadata: map[string]any{
			"full_name": "Aidan Allchin",
		},
	}
	result, err := r.srv.provisionUserOrgs(context.Background(), userID, claims)
	if err != nil {
		t.Fatalf("provisionUserOrgs: %v", err)
	}
	if !result.activeOrgID.Valid {
		t.Fatalf("activeOrgID not valid after personal-org-on-signup")
	}

	// Verify the org row exists with is_personal=true.
	var name, slug string
	var isPersonal bool
	var ownerID string
	if err := r.h.AdminDB.QueryRow(`
		SELECT name, slug, is_personal, owner_user_id::text
		  FROM public.orgs WHERE id = $1
	`, result.activeOrgID.UUID).Scan(&name, &slug, &isPersonal, &ownerID); err != nil {
		t.Fatalf("read provisioned org: %v", err)
	}
	if !isPersonal {
		t.Errorf("orgs.is_personal = false, want true")
	}
	if want := "Aidan Allchin's Personal"; name != want {
		t.Errorf("orgs.name = %q, want %q", name, want)
	}
	if slug != "aidan-allchin" {
		t.Errorf("orgs.slug = %q, want %q", slug, "aidan-allchin")
	}
	if ownerID != userID.String() {
		t.Errorf("orgs.owner_user_id = %q, want %q", ownerID, userID.String())
	}

	// Default team exists.
	var teamSlug string
	if err := r.h.AdminDB.QueryRow(`
		SELECT slug FROM public.teams WHERE org_id = $1
	`, result.activeOrgID.UUID).Scan(&teamSlug); err != nil {
		t.Fatalf("read team: %v", err)
	}
	if teamSlug != "default" {
		t.Errorf("teams.slug = %q, want %q", teamSlug, "default")
	}

	// Memberships present.
	var orgRole, teamRole string
	if err := r.h.AdminDB.QueryRow(`
		SELECT role FROM public.org_memberships WHERE user_id = $1
	`, userID).Scan(&orgRole); err != nil {
		t.Fatalf("read org_membership: %v", err)
	}
	if orgRole != "owner" {
		t.Errorf("org_memberships.role = %q, want %q", orgRole, "owner")
	}
	if err := r.h.AdminDB.QueryRow(`
		SELECT role FROM public.memberships WHERE user_id = $1
	`, userID).Scan(&teamRole); err != nil {
		t.Fatalf("read membership: %v", err)
	}
	if teamRole != "admin" {
		t.Errorf("memberships.role = %q, want %q", teamRole, "admin")
	}
}

func TestProvisionUserOrgs_AutoJoinDefault_CreatesOnFirstSignup(t *testing.T) {
	r := newAuthRig(t)
	runmode.SetJoinPolicyForTest(t, runmode.JoinPolicyAutoJoinDefault)

	userID := r.seedUser()
	claims := &verify.Claims{Email: "first@self-host.example"}

	result, err := r.srv.provisionUserOrgs(context.Background(), userID, claims)
	if err != nil {
		t.Fatalf("provisionUserOrgs: %v", err)
	}
	if !result.activeOrgID.Valid {
		t.Fatalf("activeOrgID not valid")
	}
	if result.activeOrgID.UUID.String() != runmode.LocalDefaultOrgID {
		t.Errorf("activeOrgID = %s, want sentinel %s",
			result.activeOrgID.UUID, runmode.LocalDefaultOrgID)
	}

	// First user is admin of the Default org.
	var role string
	if err := r.h.AdminDB.QueryRow(`
		SELECT role FROM public.org_memberships WHERE user_id = $1
	`, userID).Scan(&role); err != nil {
		t.Fatalf("read org_membership: %v", err)
	}
	if role != "owner" {
		t.Errorf("first user's org_memberships.role = %q, want %q", role, "owner")
	}
}

func TestProvisionUserOrgs_AutoJoinDefault_SecondUserJoinsAsMember(t *testing.T) {
	r := newAuthRig(t)
	runmode.SetJoinPolicyForTest(t, runmode.JoinPolicyAutoJoinDefault)

	first := r.seedUser()
	if _, err := r.srv.provisionUserOrgs(context.Background(), first, &verify.Claims{Email: "f@x"}); err != nil {
		t.Fatalf("first provisionUserOrgs: %v", err)
	}

	second := r.seedUser()
	result, err := r.srv.provisionUserOrgs(context.Background(), second, &verify.Claims{Email: "s@x"})
	if err != nil {
		t.Fatalf("second provisionUserOrgs: %v", err)
	}
	if !result.activeOrgID.Valid || result.activeOrgID.UUID.String() != runmode.LocalDefaultOrgID {
		t.Fatalf("second user landed on the wrong org: %v", result.activeOrgID)
	}

	var role string
	if err := r.h.AdminDB.QueryRow(`
		SELECT role FROM public.org_memberships WHERE user_id = $1
	`, second).Scan(&role); err != nil {
		t.Fatalf("read second user's org_membership: %v", err)
	}
	if role != "member" {
		t.Errorf("second user's role = %q, want %q", role, "member")
	}

	// And the Default org still has exactly one row in orgs.
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM public.orgs`).Scan(&n); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if n != 1 {
		t.Errorf("orgs row count = %d, want 1", n)
	}
}

func TestProvisionUserOrgs_InviteOnly_LeavesUserWithoutOrg(t *testing.T) {
	r := newAuthRig(t)
	runmode.SetJoinPolicyForTest(t, runmode.JoinPolicyInviteOnly)

	userID := r.seedUser()
	result, err := r.srv.provisionUserOrgs(context.Background(), userID, &verify.Claims{Email: "x@y"})
	if err != nil {
		t.Fatalf("provisionUserOrgs: %v", err)
	}
	if result.activeOrgID.Valid {
		t.Errorf("activeOrgID should be invalid under invite-only; got %v", result.activeOrgID)
	}

	// No org rows for this user.
	var n int
	if err := r.h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM public.org_memberships WHERE user_id = $1
	`, userID).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if n != 0 {
		t.Errorf("org_memberships count for invite-only user = %d, want 0", n)
	}
}

func TestProvisionUserOrgs_AlreadyHasMembership_ShortCircuits(t *testing.T) {
	r := newAuthRig(t)
	runmode.SetJoinPolicyForTest(t, runmode.JoinPolicyPersonalOrgOnSignup)

	// Pre-seed user with a real org + membership.
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	result, err := r.srv.provisionUserOrgs(context.Background(), userID, &verify.Claims{
		Email: "x@y", UserMetadata: map[string]any{"full_name": "X"},
	})
	if err != nil {
		t.Fatalf("provisionUserOrgs: %v", err)
	}
	if !result.activeOrgID.Valid {
		t.Fatalf("activeOrgID not valid for already-provisioned user")
	}
	if result.activeOrgID.UUID != orgID {
		t.Errorf("activeOrgID = %s, want %s (the existing membership)", result.activeOrgID.UUID, orgID)
	}

	// No personal org was created.
	var n int
	if err := r.h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM public.orgs WHERE owner_user_id = $1 AND is_personal = true
	`, userID).Scan(&n); err != nil {
		t.Fatalf("count personal orgs: %v", err)
	}
	if n != 0 {
		t.Errorf("personal org count = %d, want 0 (user already had a membership)", n)
	}
}

// TestProvisionUserOrgs_ConcurrentCallbacks_NoDuplicate fires the
// provisioning path twice in parallel for the same user. The
// advisory-lock + inside-tx zero-membership check must ensure exactly
// one personal org gets created — the other call should observe the
// row and short-circuit. SKY-345 race-safety acceptance bullet.
func TestProvisionUserOrgs_ConcurrentCallbacks_NoDuplicate(t *testing.T) {
	r := newAuthRig(t)
	runmode.SetJoinPolicyForTest(t, runmode.JoinPolicyPersonalOrgOnSignup)

	userID := r.seedUser()
	claims := &verify.Claims{
		Email: "race@test",
		UserMetadata: map[string]any{
			"full_name": "Race Test",
		},
	}

	const N = 4
	var wg sync.WaitGroup
	results := make([]provisionResult, N)
	errs := make([]error, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = r.srv.provisionUserOrgs(context.Background(), userID, claims)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d errored: %v", i, err)
		}
	}

	// All callers must agree on the same activeOrgID — the one and
	// only personal org.
	var first uuid.UUID
	for i, res := range results {
		if errs[i] != nil {
			continue
		}
		if !res.activeOrgID.Valid {
			t.Errorf("goroutine %d returned invalid activeOrgID", i)
			continue
		}
		if first == uuid.Nil {
			first = res.activeOrgID.UUID
			continue
		}
		if res.activeOrgID.UUID != first {
			t.Errorf("goroutine %d activeOrgID = %s, want %s (consensus)",
				i, res.activeOrgID.UUID, first)
		}
	}

	// Exactly one personal org row for this user.
	var n int
	if err := r.h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM public.orgs WHERE owner_user_id = $1 AND is_personal = true
	`, userID).Scan(&n); err != nil {
		t.Fatalf("count personal orgs: %v", err)
	}
	if n != 1 {
		t.Errorf("personal org count after %d concurrent provisions = %d, want 1", N, n)
	}

	// Exactly one org_memberships row.
	var m int
	if err := r.h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM public.org_memberships WHERE user_id = $1
	`, userID).Scan(&m); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if m != 1 {
		t.Errorf("org_memberships count = %d, want 1", m)
	}
}

// TestProvisionUserOrgs_SlugCollision verifies that two users with
// the same display name get distinct slugs ("aidan", "aidan-2").
func TestProvisionUserOrgs_SlugCollision(t *testing.T) {
	r := newAuthRig(t)
	runmode.SetJoinPolicyForTest(t, runmode.JoinPolicyPersonalOrgOnSignup)

	a := r.seedUser()
	if _, err := r.srv.provisionUserOrgs(context.Background(), a, &verify.Claims{
		UserMetadata: map[string]any{"full_name": "Aidan"},
	}); err != nil {
		t.Fatalf("first provision: %v", err)
	}

	b := r.seedUser()
	if _, err := r.srv.provisionUserOrgs(context.Background(), b, &verify.Claims{
		UserMetadata: map[string]any{"full_name": "Aidan"},
	}); err != nil {
		t.Fatalf("second provision: %v", err)
	}

	slugs := []string{}
	rows, err := r.h.AdminDB.Query(`SELECT slug FROM public.orgs ORDER BY created_at`)
	if err != nil {
		t.Fatalf("read slugs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		slugs = append(slugs, s)
	}
	if len(slugs) != 2 {
		t.Fatalf("expected 2 org rows, got %d: %v", len(slugs), slugs)
	}
	if slugs[0] != "aidan" {
		t.Errorf("first slug = %q, want %q", slugs[0], "aidan")
	}
	if slugs[1] != "aidan-2" {
		t.Errorf("second slug = %q, want %q", slugs[1], "aidan-2")
	}
}
