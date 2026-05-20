package config_test

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
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

	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := config.Load()
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
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var anthRef, bedRef, maxTier sql.NullString
	err := conn.QueryRow(`
		SELECT anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier
		FROM org_settings WHERE org_id = ?
	`, "00000000-0000-0000-0000-000000000001").Scan(&anthRef, &bedRef, &maxTier)
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

// TestMaxLLMModelTierCheckConstraint pins the CHECK constraint at the
// SQLite layer: rows with an invalid tier are rejected at write time.
// The Postgres mirror is exercised in pgtest (skipped when Docker is
// unavailable) — this test guarantees the check still bites in local
// mode when an admin path bypasses Save().
func TestMaxLLMModelTierCheckConstraint(t *testing.T) {
	conn := openDB(t)
	// Need an org_settings row first; seed via Save() through the sentinel.
	if err := config.Init(conn); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := config.Save(config.Default()); err != nil {
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
