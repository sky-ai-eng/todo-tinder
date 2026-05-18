package agentproc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SecretsReader is the read-only slice of the per-org secret store
// that agentproc needs to resolve LLM credentials. Declared inside
// agentproc so the package doesn't import internal/db directly —
// callers pass their db.SecretStore (which satisfies this shape
// structurally) without creating a circular dependency.
//
// The shape matches db.SecretStore.Get exactly: a missing secret is
// returned as ("", nil), and "fetch failed" surfaces a real error
// the resolver propagates.
type SecretsReader interface {
	Get(ctx context.Context, orgID, key string) (string, error)
}

// ErrNoCredentialsConfigured surfaces when multi-mode invokes Run
// against an org with no Anthropic / Bedrock secret in vault. Typed
// so the caller (delegate spawner, scorer, etc.) can decide whether
// to bubble a 5xx to the user, log + skip, or mark a run as
// errored without retrying.
//
// Local mode never produces this — empty OrgID maps to "use the
// host's Claude Code subscription" via the inherited env, which is
// the supported zero-config setup for single-user installs.
var ErrNoCredentialsConfigured = errors.New("agentproc: no LLM credentials configured for org")

// Secret key catalog. Strings match the names handlers use when
// writing to the vault / keychain via SecretStore.Put — keep these
// in sync with the admin-UI write path (D14, separate ticket).
const (
	secretAnthropicAPIKey   = "anthropic_api_key"
	secretAWSAccessKeyID    = "aws_access_key_id"
	secretAWSSecretKey      = "aws_secret_access_key"
	secretAWSSessionToken   = "aws_session_token"
	secretAWSRegion         = "aws_region"
	secretBedrockModelID    = "bedrock_model_id"
	secretAnthropicBaseURL  = "anthropic_base_url"
	secretAnthropicAuthMode = "anthropic_auth_token" // optional: gateway / proxy bearer
)

// credentialEnvKeys lists every env var the LLM SDK in wrapper.mjs
// consumes to pick up auth material. The Run path treats these as
// the parent-env leak surface: when the resolver returns per-org
// creds, every key in this list is filtered out of os.Environ before
// the subprocess inherits anything, so a misconfigured deployment
// where the operator set ANTHROPIC_API_KEY at the systemd-unit
// level can't silently override the org-scoped key.
//
// Keep this list synchronized with the SDK's defaults. Adding a new
// env-driven auth path means adding it here AND adding the secret
// key + mapping in resolveCredentials.
var credentialEnvKeys = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_REGION",
	"AWS_DEFAULT_REGION",
	"CLAUDE_CODE_USE_BEDROCK",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_MODEL",
}

// resolveCredentials looks up the env vars to inject into the Node
// subprocess for a given orgID. Anthropic and Bedrock paths are
// mutually exclusive: Anthropic key wins when both are configured
// (defensive — the admin-UI write path is expected to enforce
// exclusivity at config time, but resolver order matters if a
// malformed install ends up with both).
//
// Local-mode behavior:
//
//   - empty OrgID OR OrgID == LocalDefaultOrg with no Anthropic /
//     Bedrock secret set → returns an empty map. The subprocess
//     inherits the host's env unchanged (preserves the existing
//     "Claude Code subscription handles auth" flow + the
//     TRIAGE_FACTORY_*-style env-overlay paths some users rely on).
//   - LocalDefaultOrg with a configured Anthropic / Bedrock key →
//     returns the matching env map; Run filters credentialEnvKeys
//     from os.Environ so a stale shell var can't override the
//     keychain value.
//
// Multi-mode behavior:
//
//   - empty OrgID → error. Hard caller bug; refuse loudly rather
//     than silently leaking the parent process's env (which in a
//     hosted container would be the operator-supplied shared key,
//     which is the exact cross-tenant bleed the ticket exists to
//     prevent).
//   - orgID set, no credentials configured →
//     ErrNoCredentialsConfigured. Caller surfaces however its UX
//     dictates; we don't fall back to the parent env.
func resolveCredentials(ctx context.Context, secrets SecretsReader, orgID string) (map[string]string, error) {
	multi := runmode.Current() == runmode.ModeMulti

	if orgID == "" {
		if multi {
			return nil, fmt.Errorf("%w: empty orgID in multi mode (caller bug — pass the request's org context)", ErrNoCredentialsConfigured)
		}
		return map[string]string{}, nil
	}

	if secrets == nil {
		if multi {
			return nil, fmt.Errorf("%w: SecretsReader is nil in multi mode", ErrNoCredentialsConfigured)
		}
		// Local with no SecretsReader plumbed (test path or pre-sweep
		// caller) — degrade to inherited-env subscription fallback.
		return map[string]string{}, nil
	}

	// Anthropic direct: API key alone is sufficient. Optional base
	// URL + auth-token-style bearer let advanced setups front the
	// SDK through a gateway, but neither is required.
	apiKey, err := secrets.Get(ctx, orgID, secretAnthropicAPIKey)
	if err != nil {
		return nil, fmt.Errorf("resolve anthropic_api_key: %w", err)
	}
	if apiKey != "" {
		env := map[string]string{"ANTHROPIC_API_KEY": apiKey}
		baseURL, _ := secrets.Get(ctx, orgID, secretAnthropicBaseURL)
		if baseURL != "" {
			env["ANTHROPIC_BASE_URL"] = baseURL
		}
		authToken, _ := secrets.Get(ctx, orgID, secretAnthropicAuthMode)
		if authToken != "" {
			env["ANTHROPIC_AUTH_TOKEN"] = authToken
		}
		return env, nil
	}

	// Bedrock: require the access-key triple. Session token + region
	// are optional (region defaults to us-east-1 in some setups; the
	// SDK has its own region resolution). Partial creds (e.g. access
	// key set, secret missing) means a malformed admin config —
	// treat as not-configured rather than half-injecting, so the
	// caller sees ErrNoCredentialsConfigured and not an AWS-SDK
	// error from inside the Node subprocess.
	accessKey, err := secrets.Get(ctx, orgID, secretAWSAccessKeyID)
	if err != nil {
		return nil, fmt.Errorf("resolve aws_access_key_id: %w", err)
	}
	secretKey, err := secrets.Get(ctx, orgID, secretAWSSecretKey)
	if err != nil {
		return nil, fmt.Errorf("resolve aws_secret_access_key: %w", err)
	}
	if accessKey != "" && secretKey != "" {
		env := map[string]string{
			"AWS_ACCESS_KEY_ID":       accessKey,
			"AWS_SECRET_ACCESS_KEY":   secretKey,
			"CLAUDE_CODE_USE_BEDROCK": "1",
		}
		sessionTok, _ := secrets.Get(ctx, orgID, secretAWSSessionToken)
		if sessionTok != "" {
			env["AWS_SESSION_TOKEN"] = sessionTok
		}
		region, _ := secrets.Get(ctx, orgID, secretAWSRegion)
		if region != "" {
			env["AWS_REGION"] = region
		}
		modelID, _ := secrets.Get(ctx, orgID, secretBedrockModelID)
		if modelID != "" {
			env["ANTHROPIC_MODEL"] = modelID
		}
		return env, nil
	}

	// No Anthropic, no Bedrock. Local mode: subscription fallback.
	// Multi mode: hard error so the caller surfaces it.
	if multi {
		return nil, fmt.Errorf("%w: org %s has no anthropic_api_key or AWS credentials in vault", ErrNoCredentialsConfigured, orgID)
	}
	return map[string]string{}, nil
}

// mergeEnv composes the subprocess env. When per-org credentials are
// resolved (creds non-empty), every key in credentialEnvKeys is
// stripped from the inherited env first — so a shell-set
// ANTHROPIC_API_KEY can't override the org-scoped key via
// last-entry-wins semantics. When the resolver returned empty (no
// per-org credentials configured), the inherited env passes through
// unchanged so existing local-mode flows that rely on
// process-environment ANTHROPIC_API_KEY continue to work.
//
// extraEnv is appended after the (optionally filtered) parent env;
// resolved creds are appended last so they take precedence on
// platforms where exec.Cmd respects last-wins.
func mergeEnv(parentEnv, extraEnv []string, creds map[string]string) []string {
	out := make([]string, 0, len(parentEnv)+len(extraEnv)+len(creds))

	if len(creds) == 0 {
		out = append(out, parentEnv...)
	} else {
		// Filter credential keys out of the parent env.
		out = filterEnv(parentEnv, credentialEnvKeys)
	}

	out = append(out, extraEnv...)

	for k, v := range creds {
		out = append(out, k+"="+v)
	}
	return out
}

// filterEnv returns a copy of env with any KEY=... entry whose KEY
// is in remove dropped. Case-sensitive on the key, matching OS env
// semantics on every platform we ship to.
func filterEnv(env, remove []string) []string {
	if len(remove) == 0 {
		return append([]string(nil), env...)
	}
	skip := make(map[string]struct{}, len(remove))
	for _, k := range remove {
		skip[k] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		// Split on the first '=' — env values may contain '='
		// (notably some PATHs and JSON-encoded vars).
		eq := strings.IndexByte(e, '=')
		if eq < 0 {
			out = append(out, e)
			continue
		}
		if _, found := skip[e[:eq]]; found {
			continue
		}
		out = append(out, e)
	}
	return out
}
