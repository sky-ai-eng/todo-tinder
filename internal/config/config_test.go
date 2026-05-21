package config_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// openDB returns an in-memory SQLite handle with the v1.11.0 baseline
// schema applied — same path tests in internal/db/sqlite take.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// TestDefault_PopulatesAllSubStructs pins the defaults for the fields
// the ticket touches: AI policy lives at the team scope (Model +
// AutoDelegateEnabled), and the new OrgLLM sub-struct exists with empty
// vault refs + no model-tier cap on a fresh install.
func TestDefault_PopulatesAllSubStructs(t *testing.T) {
	cfg := config.Default()

	if cfg.AI.Model != "sonnet" {
		t.Errorf("AI.Model = %q; want sonnet", cfg.AI.Model)
	}
	if !cfg.AI.AutoDelegateEnabled {
		t.Errorf("AI.AutoDelegateEnabled = false; local-mode default should be true (preserves pre-move behavior)")
	}
	if cfg.AI.ReprioritizeThreshold != 5 || cfg.AI.PreferenceUpdateInterval != 20 {
		t.Errorf("AI thresholds = (%d,%d); want (5,20)",
			cfg.AI.ReprioritizeThreshold, cfg.AI.PreferenceUpdateInterval)
	}

	if cfg.OrgLLM.AnthropicAPIKeyRef != "" || cfg.OrgLLM.BedrockCredentialsRef != "" {
		t.Errorf("OrgLLM vault refs = (%q,%q); want both empty by default",
			cfg.OrgLLM.AnthropicAPIKeyRef, cfg.OrgLLM.BedrockCredentialsRef)
	}
	if cfg.OrgLLM.MaxModelTier != "" {
		t.Errorf("OrgLLM.MaxModelTier = %q; want empty (no cap by default)", cfg.OrgLLM.MaxModelTier)
	}
}

// TestSaveLoad_RoundTrip exercises the full settings round-trip after
// the SKY-354 column moves: Anthropic/Bedrock vault refs + max model
// tier on org_settings, default_model + auto_delegate_enabled on
// team_settings. user_settings no longer holds AI fields — the AI
// model that Load() returns must come from team_settings.
func TestSaveLoad_RoundTrip(t *testing.T) {
	conn := openDB(t)
	if err := config.Init(conn); err != nil {
		t.Fatalf("Init: %v", err)
	}

	cfg := config.Default()
	cfg.GitHub.BaseURL = "https://github.example.com"
	cfg.GitHub.PollInterval = 7 * time.Minute
	cfg.Jira.BaseURL = "https://jira.example.com"
	cfg.Jira.PollInterval = 11 * time.Minute
	cfg.AI.Model = "opus"
	cfg.AI.AutoDelegateEnabled = false
	cfg.AI.ReprioritizeThreshold = 13
	cfg.AI.PreferenceUpdateInterval = 17
	cfg.OrgLLM.AnthropicAPIKeyRef = "vault://orgs/acme/anthropic"
	cfg.OrgLLM.BedrockCredentialsRef = "vault://orgs/acme/bedrock"
	cfg.OrgLLM.MaxModelTier = "sonnet"

	if err := config.SaveLocal(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := config.LoadLocal()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.AI.Model != "opus" {
		t.Errorf("Load AI.Model = %q; want opus (came from team_settings.default_model)", got.AI.Model)
	}
	if got.AI.AutoDelegateEnabled {
		t.Errorf("Load AI.AutoDelegateEnabled = true; want false (from team_settings.auto_delegate_enabled)")
	}
	if got.OrgLLM.AnthropicAPIKeyRef != "vault://orgs/acme/anthropic" {
		t.Errorf("Load OrgLLM.AnthropicAPIKeyRef = %q; want round-tripped value", got.OrgLLM.AnthropicAPIKeyRef)
	}
	if got.OrgLLM.BedrockCredentialsRef != "vault://orgs/acme/bedrock" {
		t.Errorf("Load OrgLLM.BedrockCredentialsRef = %q; want round-tripped value", got.OrgLLM.BedrockCredentialsRef)
	}
	if got.OrgLLM.MaxModelTier != "sonnet" {
		t.Errorf("Load OrgLLM.MaxModelTier = %q; want sonnet", got.OrgLLM.MaxModelTier)
	}
}

// TestSave_EmptyOrgLLMRefsPersistAsNull guards the nullable-by-design
// semantics: an empty AnthropicAPIKeyRef must store as SQL NULL so the
// "use deployment default" / "not configured" semantics survive the
// round-trip. Storing empty-string would be indistinguishable from a
// configured ref in downstream resolvers.
func TestSave_EmptyOrgLLMRefsPersistAsNull(t *testing.T) {
	conn := openDB(t)
	if err := config.Init(conn); err != nil {
		t.Fatalf("Init: %v", err)
	}

	cfg := config.Default()
	if err := config.SaveLocal(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var anthRef, bedRef, maxTier sql.NullString
	err := conn.QueryRow(`
		SELECT anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier
		FROM org_settings WHERE org_id = ?
	`, runmode.LocalDefaultOrgID).Scan(&anthRef, &bedRef, &maxTier)
	if err != nil {
		t.Fatalf("read org_settings: %v", err)
	}
	if anthRef.Valid {
		t.Errorf("anthropic_api_key_ref = %q; want NULL on empty Save", anthRef.String)
	}
	if bedRef.Valid {
		t.Errorf("bedrock_credentials_ref = %q; want NULL on empty Save", bedRef.String)
	}
	if maxTier.Valid {
		t.Errorf("max_llm_model_tier = %q; want NULL on empty Save", maxTier.String)
	}
}

// TestLoad_PerOrgIsolation pins the multi-mode invariant the package
// gained when Load/Save stopped reading the local sentinel directly:
// settings saved under one orgID/teamID must not bleed into a read
// scoped to a different orgID/teamID. Exercised against the SQLite
// schema so it runs in CI without Docker; the same code paths (just a
// different driver) carry the same isolation in the Postgres baseline.
//
// This is the regression test the de-scope ticket calls for —
// pre-change, Load() hardcoded the sentinels, so this test was
// physically impossible to write. Now it is, and it should stay green.
func TestLoad_PerOrgIsolation(t *testing.T) {
	conn := openDB(t)
	if err := config.Init(conn); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const (
		orgA  = "00000000-0000-0000-0000-00000000000a"
		orgB  = "00000000-0000-0000-0000-00000000000b"
		teamA = "00000000-0000-0000-0000-0000000000a1"
		teamB = "00000000-0000-0000-0000-0000000000b1"
	)
	for _, q := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES (?, ?, ?)`,
	} {
		if _, err := conn.Exec(q, orgA, "a", "A"); err != nil {
			t.Fatalf("seed orgA: %v", err)
		}
		if _, err := conn.Exec(q, orgB, "b", "B"); err != nil {
			t.Fatalf("seed orgB: %v", err)
		}
	}
	if _, err := conn.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'default', 'Default')`,
		teamA, orgA,
	); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'default', 'Default')`,
		teamB, orgB,
	); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	cfgA := config.Default()
	cfgA.GitHub.BaseURL = "https://github.a.example.com"
	cfgA.Jira.BaseURL = "https://jira.a.example.com"
	cfgA.AI.Model = "opus"
	cfgA.OrgLLM.MaxModelTier = "opus"

	cfgB := config.Default()
	cfgB.GitHub.BaseURL = "https://github.b.example.com"
	cfgB.Jira.BaseURL = "https://jira.b.example.com"
	cfgB.AI.Model = "haiku"
	cfgB.OrgLLM.MaxModelTier = "sonnet"

	ctx := context.Background()
	if err := config.Save(ctx, orgA, teamA, "", cfgA); err != nil {
		t.Fatalf("Save A: %v", err)
	}
	if err := config.Save(ctx, orgB, teamB, "", cfgB); err != nil {
		t.Fatalf("Save B: %v", err)
	}

	gotA, err := config.Load(ctx, orgA, teamA, "")
	if err != nil {
		t.Fatalf("Load A: %v", err)
	}
	gotB, err := config.Load(ctx, orgB, teamB, "")
	if err != nil {
		t.Fatalf("Load B: %v", err)
	}

	if gotA.GitHub.BaseURL != "https://github.a.example.com" {
		t.Errorf("Load A GitHub.BaseURL = %q; want a-side value (bleed from B?)", gotA.GitHub.BaseURL)
	}
	if gotB.GitHub.BaseURL != "https://github.b.example.com" {
		t.Errorf("Load B GitHub.BaseURL = %q; want b-side value (bleed from A?)", gotB.GitHub.BaseURL)
	}
	if gotA.AI.Model != "opus" || gotB.AI.Model != "haiku" {
		t.Errorf("AI.Model bleed: A=%q B=%q (want opus/haiku)", gotA.AI.Model, gotB.AI.Model)
	}
	if gotA.OrgLLM.MaxModelTier != "opus" || gotB.OrgLLM.MaxModelTier != "sonnet" {
		t.Errorf("OrgLLM.MaxModelTier bleed: A=%q B=%q (want opus/sonnet)",
			gotA.OrgLLM.MaxModelTier, gotB.OrgLLM.MaxModelTier)
	}

	// Cross-scope read: orgA + teamB must NOT return A's team settings nor B's
	// org settings paired together. With the cross IDs, A's team_settings row
	// doesn't match (teamB lookup) so AI.Model falls back to Default("sonnet"),
	// and B's org_settings row doesn't match (orgA lookup) so GitHub.BaseURL is
	// A's. The mismatched pair is what proves the queries are scope-correct
	// rather than coincidentally returning the right row by some other key.
	cross, err := config.Load(ctx, orgA, teamB, "")
	if err != nil {
		t.Fatalf("Load cross: %v", err)
	}
	if cross.GitHub.BaseURL != "https://github.a.example.com" {
		t.Errorf("Cross load GitHub.BaseURL = %q; want orgA value", cross.GitHub.BaseURL)
	}
	if cross.AI.Model != "haiku" {
		t.Errorf("Cross load AI.Model = %q; want teamB value haiku", cross.AI.Model)
	}
}

// TestMaxLLMModelTierCheckConstraint pins the CHECK constraint at the
// SQLite layer: rows with an invalid tier are rejected at write time,
// so local-mode admin paths that bypass Save() can't slip a bad value
// into org_settings. The Postgres mirror is pinned by
// TestBaseline_OrgSettingsMaxLLMTier_CHECKFires in internal/db/pgtest
// (skipped when Docker is unavailable).
func TestMaxLLMModelTierCheckConstraint(t *testing.T) {
	conn := openDB(t)
	// Need an org_settings row first; seed via Save() through the sentinel.
	if err := config.Init(conn); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := config.SaveLocal(config.Default()); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	_, err := conn.Exec(`UPDATE org_settings SET max_llm_model_tier = 'invalid'`)
	if err == nil {
		t.Fatalf("UPDATE with max_llm_model_tier='invalid' succeeded; want CHECK failure")
	}

	for _, ok := range []string{"haiku", "sonnet", "opus"} {
		if _, err := conn.Exec(`UPDATE org_settings SET max_llm_model_tier = ?`, ok); err != nil {
			t.Errorf("UPDATE with max_llm_model_tier=%q: %v (want allowed by CHECK)", ok, err)
		}
	}
}
