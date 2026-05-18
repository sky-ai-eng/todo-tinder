package agentproc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeSecrets backs the resolver tests with an in-memory map. Returns
// ("", nil) for missing keys — matches the real SecretStore.Get
// contract.
type fakeSecrets struct {
	values map[string]map[string]string // orgID → key → value
	err    error
}

func (f *fakeSecrets) Get(_ context.Context, orgID, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.values[orgID][key], nil
}

func newFakeSecrets(orgID string, kv map[string]string) *fakeSecrets {
	return &fakeSecrets{values: map[string]map[string]string{orgID: kv}}
}

func TestResolveCredentials_AnthropicConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "11111111-1111-1111-1111-111111111111"
	secrets := newFakeSecrets(orgID, map[string]string{
		"anthropic_api_key": "sk-ant-org1",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-ant-org1" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant-org1", got)
	}
	// Bedrock vars must NOT appear when Anthropic is configured —
	// mutually exclusive code path.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "CLAUDE_CODE_USE_BEDROCK"} {
		if _, ok := env[k]; ok {
			t.Errorf("env[%s] set when Anthropic configured; should be Anthropic-only", k)
		}
	}
}

func TestResolveCredentials_BedrockConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "22222222-2222-2222-2222-222222222222"
	secrets := newFakeSecrets(orgID, map[string]string{
		"aws_access_key_id":     "AKIA-test",
		"aws_secret_access_key": "secret-test",
		"aws_region":            "us-west-2",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["AWS_ACCESS_KEY_ID"] != "AKIA-test" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want AKIA-test", env["AWS_ACCESS_KEY_ID"])
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "secret-test" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want secret-test", env["AWS_SECRET_ACCESS_KEY"])
	}
	if env["AWS_REGION"] != "us-west-2" {
		t.Errorf("AWS_REGION = %q, want us-west-2", env["AWS_REGION"])
	}
	if env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q, want 1 (auto-flagged when AWS triple set)", env["CLAUDE_CODE_USE_BEDROCK"])
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY set when only Bedrock configured")
	}
}

// TestResolveCredentials_AnthropicWinsOverBedrock pins resolver
// precedence: when both paths are populated (malformed config that
// slipped past the admin-UI exclusivity gate), Anthropic wins. Picks
// one rather than erroring so a half-configured org can still run.
func TestResolveCredentials_AnthropicWinsOverBedrock(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "33333333-3333-3333-3333-333333333333"
	secrets := newFakeSecrets(orgID, map[string]string{
		"anthropic_api_key":     "sk-ant-both",
		"aws_access_key_id":     "AKIA-both",
		"aws_secret_access_key": "secret-both",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-ant-both" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant-both", env["ANTHROPIC_API_KEY"])
	}
	if _, ok := env["AWS_ACCESS_KEY_ID"]; ok {
		t.Errorf("AWS_ACCESS_KEY_ID set; Anthropic should win")
	}
}

func TestResolveCredentials_PartialBedrockIsNotConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "44444444-4444-4444-4444-444444444444"
	// Access key set, secret key missing — malformed config. Treat
	// as not-configured rather than half-injecting (which would
	// surface as an AWS-SDK error inside the Node subprocess, much
	// harder to debug than a typed Go error at the call site).
	secrets := newFakeSecrets(orgID, map[string]string{
		"aws_access_key_id": "AKIA-partial",
	})

	_, err := resolveCredentials(context.Background(), secrets, orgID)
	if !errors.Is(err, ErrNoCredentialsConfigured) {
		t.Fatalf("err = %v, want ErrNoCredentialsConfigured wrap", err)
	}
}

func TestResolveCredentials_EmptyOrgIDLocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	env, err := resolveCredentials(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("resolveCredentials(local, empty): %v", err)
	}
	if len(env) != 0 {
		t.Errorf("env = %v, want empty (subscription fallback)", env)
	}
}

func TestResolveCredentials_EmptyOrgIDMultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	_, err := resolveCredentials(context.Background(), nil, "")
	if !errors.Is(err, ErrNoCredentialsConfigured) {
		t.Fatalf("err = %v, want ErrNoCredentialsConfigured wrap (multi mode refuses empty orgID)", err)
	}
}

func TestResolveCredentials_MultiModeOrgWithNoKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "55555555-5555-5555-5555-555555555555"
	secrets := newFakeSecrets(orgID, map[string]string{}) // empty vault row

	_, err := resolveCredentials(context.Background(), secrets, orgID)
	if !errors.Is(err, ErrNoCredentialsConfigured) {
		t.Fatalf("err = %v, want ErrNoCredentialsConfigured wrap", err)
	}
}

// TestResolveCredentials_LocalModeOrgWithKey pins the local-mode +
// per-org key path: the resolver returns the key so mergeEnv strips
// credential vars from os.Environ and uses the keychain value. This
// is the seam that lets the future per-org credential UI work
// identically in local-as-N=1 and multi mode.
func TestResolveCredentials_LocalModeOrgWithKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	secrets := newFakeSecrets(runmode.LocalDefaultOrg, map[string]string{
		"anthropic_api_key": "sk-ant-local-configured",
	})

	env, err := resolveCredentials(context.Background(), secrets, runmode.LocalDefaultOrg)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-ant-local-configured" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant-local-configured", env["ANTHROPIC_API_KEY"])
	}
}

func TestResolveCredentials_SecretReadErrorPropagates(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "66666666-6666-6666-6666-666666666666"
	want := errors.New("vault unavailable")
	secrets := &fakeSecrets{err: want}

	_, err := resolveCredentials(context.Background(), secrets, orgID)
	if err == nil {
		t.Fatal("err = nil, want propagated secret-read failure")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want errors.Is(_, want)", err)
	}
	if errors.Is(err, ErrNoCredentialsConfigured) {
		t.Errorf("err mis-classified as ErrNoCredentialsConfigured; want raw vault failure (caller may retry)")
	}
}

// TestMergeEnv_StripsParentCredsWhenResolved is the load-bearing
// parent-env-leak guard. A misconfigured operator-level
// ANTHROPIC_API_KEY at the systemd-unit level would otherwise win
// over the resolver's per-org key via last-entry-wins semantics on
// some platforms; this test pins that the parent-env credential
// keys are filtered out before the resolved creds are appended.
func TestMergeEnv_StripsParentCredsWhenResolved(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/test",
		"ANTHROPIC_API_KEY=parent-leak",
		"AWS_ACCESS_KEY_ID=parent-aws-leak",
		"NPM_CONFIG_CACHE=/cache",
	}
	extra := []string{"TRIAGE_FACTORY_RUN_ID=run-123"}
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-org-resolved"}

	merged := mergeEnv(parent, extra, creds)

	if containsKey(merged, "ANTHROPIC_API_KEY=parent-leak") {
		t.Errorf("parent ANTHROPIC_API_KEY survived merge — leak guard broken")
	}
	if containsKey(merged, "AWS_ACCESS_KEY_ID=parent-aws-leak") {
		t.Errorf("parent AWS_ACCESS_KEY_ID survived merge — leak guard broken")
	}
	if !containsKey(merged, "ANTHROPIC_API_KEY=sk-ant-org-resolved") {
		t.Errorf("resolved ANTHROPIC_API_KEY missing from merged env: %v", merged)
	}
	// Non-credential parent vars must pass through (PATH, NPM cache,
	// etc — the SDK + Node toolchain depend on these).
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/test", "NPM_CONFIG_CACHE=/cache"} {
		if !containsKey(merged, want) {
			t.Errorf("merged env missing %q (non-credential parent var should pass through): %v", want, merged)
		}
	}
	// ExtraEnv must pass through.
	if !containsKey(merged, "TRIAGE_FACTORY_RUN_ID=run-123") {
		t.Errorf("ExtraEnv var missing from merged env: %v", merged)
	}
}

// TestMergeEnv_PreservesParentEnvWhenNoResolvedCreds covers the
// local-mode subscription path: when the resolver returns an empty
// map (no per-org credentials configured), the parent's env passes
// through unchanged so existing single-user installs that rely on
// process-environment ANTHROPIC_API_KEY or the Claude Code
// subscription's own auth path continue to work.
func TestMergeEnv_PreservesParentEnvWhenNoResolvedCreds(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=user-shell-key",
	}
	merged := mergeEnv(parent, nil, map[string]string{})

	if !containsKey(merged, "ANTHROPIC_API_KEY=user-shell-key") {
		t.Errorf("parent ANTHROPIC_API_KEY stripped despite no resolver creds: %v", merged)
	}
}

func TestFilterEnv_HandlesValuesContainingEquals(t *testing.T) {
	// env values often contain '=' (PATH, structured tokens). Filter
	// must split on the *first* '=' only, never strip a var whose
	// value happens to start with a forbidden key.
	env := []string{
		"PATH=/bin:/usr/bin",
		"ANTHROPIC_API_KEY=secret",
		"DEBUG_HINT=value with=equals inside",
		"SOMETHING_LIKE_ANTHROPIC_API_KEY=not-actually-a-key",
	}
	out := filterEnv(env, []string{"ANTHROPIC_API_KEY"})

	if containsKey(out, "ANTHROPIC_API_KEY=secret") {
		t.Errorf("target key not removed")
	}
	for _, want := range []string{
		"PATH=/bin:/usr/bin",
		"DEBUG_HINT=value with=equals inside",
		"SOMETHING_LIKE_ANTHROPIC_API_KEY=not-actually-a-key",
	} {
		if !containsKey(out, want) {
			t.Errorf("non-matching entry stripped: missing %q in %v", want, out)
		}
	}
}

// containsKey reports whether env contains the exact line want
// (KEY=VALUE form). Used by the merge tests instead of substring
// search because env entries are exact-match strings.
func containsKey(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestCredentialEnvKeysSorted catches accidental duplication or
// drift in the leak-guard list — drift here weakens the multi-mode
// safety net silently.
func TestCredentialEnvKeysSorted(t *testing.T) {
	seen := make(map[string]struct{}, len(credentialEnvKeys))
	for _, k := range credentialEnvKeys {
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate entry in credentialEnvKeys: %q", k)
		}
		seen[k] = struct{}{}
	}
	// Don't enforce alphabetical, but flag obvious typos by
	// requiring upper-case (env vars are conventionally upper).
	for _, k := range credentialEnvKeys {
		if strings.ToUpper(k) != k {
			t.Errorf("credential env key %q is not upper-case", k)
		}
	}
}
