package agentproc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// proxyPlaceholderAPIKey is the dummy value the sandboxed agent sees
// in ANTHROPIC_API_KEY. The agent SDK requires *some* non-empty value
// to construct an authenticated request; the real key never enters
// the sandbox env. The proxy rewrites the x-api-key header before
// forwarding upstream, so the placeholder is discarded on the host
// side and the upstream receives the real credential. The shape
// (sk-ant-... prefix) matches Anthropic's published format so any
// in-SDK shape check passes; the body of the value is "PROXY-
// PLACEHOLDER-DO-NOT-USE" so logs / debug dumps make the substitution
// obvious to anyone grepping for a leak.
const proxyPlaceholderAPIKey = "sk-ant-PROXY-PLACEHOLDER-DO-NOT-USE"

// proxyPlaceholderBedrockBearer mirrors the Anthropic placeholder for
// the Bedrock-bearer path. AWS_BEARER_TOKEN_BEDROCK is the env var the
// SDK reads; the proxy injects the real bearer in Authorization
// before forwarding to bedrock-runtime.<region>.amazonaws.com.
const proxyPlaceholderBedrockBearer = "PROXY-PLACEHOLDER-BEDROCK-BEARER"

// runProxies bundles the per-run proxy handles for shutdown. Only
// the LLM proxy is wired in SKY-335; the git proxy slot is reserved
// for the sibling ticket and remains nil until it lands.
type runProxies struct {
	llm *llmproxy.Server
	// git *gitproxy.Server // sibling ticket (SKY-335's twin)
}

// Shutdown stops every proxy in the bundle. Returns errors.Join of
// every proxy's Shutdown error so a future bundle with multiple
// proxies (git proxy slot) surfaces all failures, not just the
// first. Today, with only the LLM proxy wired, the result is either
// nil or a single wrapped error.
func (p *runProxies) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.llm != nil {
		if err := p.llm.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("llm proxy shutdown: %w", err))
		}
	}
	return errors.Join(errs...)
}

// ErrUnsupportedSandboxCredentials is returned when the resolved
// credentials don't map to a proxy-able shape. AWS SigV4 (access-key
// triple without bearer) is the current gap: the Phase 1 llmproxy
// only handles bearer-style headers, so an org configured with the
// AWS triple can't run in multi mode until the Phase 2 SigV4 proxy
// lands. Surfaced as a typed error so the caller (delegate / scorer)
// can produce a clear admin-facing message rather than a confusing
// upstream auth error from inside the Node subprocess.
var ErrUnsupportedSandboxCredentials = errors.New("agentproc: resolved credentials shape not supported in sandbox mode")

// startProxiesForSandbox starts the per-run LLM proxy (and, when the
// sibling ticket lands, the git proxy) on hostVethIP. Returns the
// proxy bundle for shutdown plus the env entries the sandbox should
// inject so the agent reaches the proxy instead of the real upstream.
//
// hostVethIP is the host-side veth address — the 10.42.<idx>.1 IP
// that the sandbox's netns can reach via its default route. Binding
// here (not loopback) is the whole point: the sandboxed agent sees
// only the netns and can't reach 127.0.0.1, so a loopback-bound proxy
// would be invisible to it.
//
// The resolvedCreds map is what resolveCredentials produced — the
// shape determines the proxy's provider + upstream. See the switch
// below for the mapping.
//
// Caller MUST call returned.Shutdown when the run completes (normal
// or cancelled). On error, no proxies are running and the returned
// bundle is nil — defer Shutdown is safe but a no-op.
func startProxiesForSandbox(hostVethIP string, resolvedCreds map[string]string) (*runProxies, []string, error) {
	if hostVethIP == "" {
		return nil, nil, errors.New("agentproc: startProxiesForSandbox: hostVethIP is required")
	}

	cfg, err := proxyConfigFromCreds(resolvedCreds)
	if err != nil {
		return nil, nil, err
	}

	bundle := &runProxies{}

	llm, err := llmproxy.New(cfg.llm)
	if err != nil {
		return nil, nil, fmt.Errorf("agentproc: construct llm proxy: %w", err)
	}
	// Port 0 — let the kernel pick a free port. We don't need a
	// predictable port (the env var carries the actual address into
	// the sandbox), and a random port avoids collision when multiple
	// runs share a subnet pool.
	addr, err := llm.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		// llmproxy.Start failed before any listener was set; nothing
		// to clean up. Return a clean error so the caller doesn't try
		// to Shutdown a half-constructed server.
		return nil, nil, fmt.Errorf("agentproc: start llm proxy on %s: %w", hostVethIP, err)
	}
	bundle.llm = llm

	// http:// (not https://) on the local hop — the agent talks
	// cleartext to the proxy across the veth pair, the proxy talks
	// TLS to the real upstream. The agent-side hop is bounded by the
	// netns + iptables / gVisor egress isolation, so no cleartext
	// credential ever crosses an exposed network boundary (the
	// placeholder is what the agent sends, not the real key).
	llmURL := "http://" + addr

	env := buildSandboxProxyEnv(cfg, llmURL)

	// Git proxy slot: the sibling ticket (per SKY-335's body) will
	// spawn a second proxy on a different port of hostVethIP and
	// inject http.proxy git config into the worktree's .git/config.
	// Until then, multi-mode agents that try to push will fail at the
	// git-clone or git-push step because no proxy is listening — that
	// is the intended interim state (multi mode is not user-facing
	// yet; the gate is the parent SKY-242 epic). The SKY-322
	// credential resolver exposes the GitHub PAT via the org's vault;
	// the future git proxy will consume it the same way startProxies
	// consumes the Anthropic key here.

	return bundle, env, nil
}

// sandboxProxyConfig collects the parsed proxy-side configuration the
// resolver implies. Internal — not exported because the only consumer
// is startProxiesForSandbox in this file.
type sandboxProxyConfig struct {
	llm llmproxy.Config
	// One of "anthropic" or "bedrock_bearer". Drives the placeholder
	// env shape (different env vars for each provider).
	providerKind llmproxy.Provider
}

// proxyConfigFromCreds maps a resolveCredentials output to the
// llmproxy.Config + provider kind. The mapping mirrors the resolver's
// precedence order: Anthropic key wins over Bedrock; Bedrock bearer
// wins over the AWS triple.
//
// AWS triple without bearer is rejected with
// ErrUnsupportedSandboxCredentials — the Phase 1 proxy doesn't
// implement SigV4 re-signing. An org configured this way can't run
// in multi mode until the Phase 2 SigV4 proxy lands. Admin UX:
// surface this as "switch to Bedrock API key or Anthropic direct".
func proxyConfigFromCreds(creds map[string]string) (sandboxProxyConfig, error) {
	if apiKey := creds["ANTHROPIC_API_KEY"]; apiKey != "" {
		// Anthropic direct (or org-gateway) path. The org may have
		// overridden ANTHROPIC_BASE_URL — point the proxy upstream at
		// the gateway instead of api.anthropic.com so the gateway's
		// own auth / quota / logging is preserved. Default to the
		// public Anthropic endpoint when no override.
		upstream := strings.TrimRight(creds["ANTHROPIC_BASE_URL"], "/")
		if upstream == "" {
			upstream = "https://api.anthropic.com"
		}
		if err := validateProxyUpstream(upstream); err != nil {
			return sandboxProxyConfig{}, fmt.Errorf("anthropic upstream: %w", err)
		}
		return sandboxProxyConfig{
			llm: llmproxy.Config{
				Provider:         llmproxy.ProviderAnthropic,
				APIKey:           apiKey,
				Upstream:         upstream,
				AllowNonLoopback: true,
			},
			providerKind: llmproxy.ProviderAnthropic,
		}, nil
	}

	if bearer := creds["AWS_BEARER_TOKEN_BEDROCK"]; bearer != "" {
		// Bedrock bearer path. Region is required for the upstream
		// URL — the Bedrock endpoint is regional. The resolver
		// already injected AWS_REGION when configured; default to
		// us-east-1 (Bedrock's primary region for Anthropic models)
		// when missing rather than refusing — matches the SDK's own
		// region resolution fallback.
		region := creds["AWS_REGION"]
		if region == "" {
			region = "us-east-1"
		}
		upstream := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
		if err := validateProxyUpstream(upstream); err != nil {
			return sandboxProxyConfig{}, fmt.Errorf("bedrock upstream: %w", err)
		}
		return sandboxProxyConfig{
			llm: llmproxy.Config{
				Provider:         llmproxy.ProviderBedrockBearer,
				APIKey:           bearer,
				Upstream:         upstream,
				AllowNonLoopback: true,
			},
			providerKind: llmproxy.ProviderBedrockBearer,
		}, nil
	}

	if creds["AWS_ACCESS_KEY_ID"] != "" || creds["AWS_SECRET_ACCESS_KEY"] != "" {
		// The resolver only returns the AWS triple when both halves
		// are present; checking either is sufficient to detect the
		// triple-without-bearer case here.
		return sandboxProxyConfig{}, fmt.Errorf("%w: AWS access-key triple requires SigV4 proxy (Phase 2); configure aws_bearer_token_bedrock or anthropic_api_key instead", ErrUnsupportedSandboxCredentials)
	}

	return sandboxProxyConfig{}, fmt.Errorf("%w: empty credentials map", ErrUnsupportedSandboxCredentials)
}

// buildSandboxProxyEnv constructs the env entries the sandbox sees:
// proxy URLs + placeholder credentials. Provider-shaped so the agent
// SDK reads the right vars — ANTHROPIC_BASE_URL for direct,
// ANTHROPIC_BEDROCK_BASE_URL + CLAUDE_CODE_USE_BEDROCK for Bedrock.
//
// Property B invariant: every value here is a URL, a placeholder, or
// a provider-selection toggle. No real secret material crosses into
// the sandbox env.
func buildSandboxProxyEnv(cfg sandboxProxyConfig, llmURL string) []string {
	switch cfg.providerKind {
	case llmproxy.ProviderAnthropic:
		return []string{
			"ANTHROPIC_BASE_URL=" + llmURL,
			"ANTHROPIC_API_KEY=" + proxyPlaceholderAPIKey,
		}
	case llmproxy.ProviderBedrockBearer:
		return []string{
			"ANTHROPIC_BEDROCK_BASE_URL=" + llmURL,
			"AWS_BEARER_TOKEN_BEDROCK=" + proxyPlaceholderBedrockBearer,
			"CLAUDE_CODE_USE_BEDROCK=1",
		}
	}
	return nil
}

// validateProxyUpstream is a pre-flight check that mirrors the
// llmproxy.New validation. Done here so a malformed org-configured
// ANTHROPIC_BASE_URL surfaces at proxy-config time (before any
// listener opens) with a message that names "anthropic upstream"
// rather than the generic "llmproxy: parse upstream URL" error from
// inside the proxy package. Keep the rules in lockstep with
// llmproxy.New: any rule added there must be added here, or
// org-configured gateway URLs will pass this check and fail later
// with a less debuggable error.
func validateProxyUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("missing scheme or host: %q", raw)
	}
	// llmproxy.New forwards the incoming request path verbatim; an
	// upstream with a path component (other than "/") would route
	// requests to "/<path>/<request-path>" and 404 at the real
	// upstream. Reject here so the admin-facing error names which
	// proxy upstream is malformed.
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must not include a path: %q", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must not include query or fragment: %q", raw)
	}
	// Cleartext-credential guard: the real key travels on the
	// proxy→upstream hop inside the rewritten auth header. http://
	// is only safe when the upstream is loopback (httptest pattern).
	if u.Scheme != "https" {
		host, _, _ := net.SplitHostPort(u.Host)
		if host == "" {
			host = u.Host
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("must use https (loopback http allowed for tests): %q", raw)
		}
	}
	return nil
}
