// Package llmproxy is a per-run HTTP intermediary that holds an LLM
// provider's API credential on the trusted side and exposes only a
// localhost (or unix-socket) base URL to the agent subprocess.
//
// # The threat it addresses
//
// The gVisor sandbox validation (docs/specs/sky-254-runsc-validation/)
// proved that whatever we inject into the OCI bundle's process.env is
// readable inside the sandbox via `env` or /proc/self/environ. That
// means injecting ANTHROPIC_API_KEY directly leaks the credential to
// the agent subprocess, which is the wrong posture for a multi-tenant
// product where the agent's prompts (and therefore behavior) are
// user-controlled.
//
// The proxy holds the real credential server-side and exposes only a
// base URL. The agent SDK natively supports this via the
// ANTHROPIC_BASE_URL env var (Anthropic direct) and
// ANTHROPIC_BEDROCK_BASE_URL (Bedrock). Per the Claude Code docs and
// the SDK source-grep of @anthropic-ai/claude-agent-sdk, requests sent
// to those URLs are forwarded transparently — the proxy doesn't need
// to know the request shape, only that it must rewrite the auth
// header before forwarding upstream.
//
// # Phase 1 scope (this file)
//
// Bearer paths only:
//
//   - Anthropic direct: rewrite "x-api-key" with the real key.
//   - Bedrock bearer (AWS_BEARER_TOKEN_BEDROCK): rewrite "Authorization"
//     with "Bearer <real-bearer>".
//
// Both are HTTP-layer header injection — no body parsing, no protocol
// translation. The proxy is built on httputil.ReverseProxy which
// handles streaming responses (SSE / chunked) natively.
//
// # Out of scope (Phase 2 follow-up)
//
// SigV4 re-signing for the Bedrock AWS-access-key-triple path. That
// case requires the proxy to re-sign request bodies with the real AWS
// credentials before forwarding, using aws-sdk-go-v2's SignHTTP. It's
// strictly more complex and gets its own ticket — Phase 1 is the
// architectural validation.
//
// # Trust model on the local hop
//
// Phase 1 listens on a localhost TCP port. The agent and the proxy
// share the same host (TF binary spawns Node as a child); the security
// boundary is "the agent's network access" (which in pre-sandbox is
// trusted-by-trust-of-user, and post-sandbox is the gVisor egress
// allowlist — the sandbox can only reach this proxy, not the wider
// internet). There is no auth on the proxy→upstream→proxy hop; the
// agent doesn't need a token to talk to the proxy because reaching the
// proxy at all means it's running under our control.
package llmproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

// Provider distinguishes the auth-header shape the proxy injects.
// More providers (Bedrock SigV4, Vertex) get added with their own
// resigning logic in later phases.
type Provider string

const (
	// ProviderAnthropic injects "x-api-key: <key>". Forwards to
	// api.anthropic.com (or whatever Upstream is configured).
	ProviderAnthropic Provider = "anthropic"

	// ProviderBedrockBearer injects "Authorization: Bearer <key>".
	// Used for AWS_BEARER_TOKEN_BEDROCK (Option E in the Claude Code
	// on Bedrock docs). Forwards to bedrock-runtime.<region>.amazonaws.com.
	ProviderBedrockBearer Provider = "bedrock_bearer"
)

// Config bundles the inputs a Server needs at construction. Each
// agent run gets its own Server with the org's resolved credentials.
type Config struct {
	// Provider selects the auth-header injection style.
	Provider Provider

	// APIKey is the real credential the proxy injects upstream.
	// Never appears in the agent subprocess's env.
	APIKey string

	// Upstream is the absolute URL of the real LLM provider — e.g.
	// "https://api.anthropic.com" or "https://bedrock-runtime.us-east-1.amazonaws.com".
	// Only scheme + host are honored; path / query / fragment must
	// be empty and are rejected at construction time so a caller who
	// passes "https://api.anthropic.com/v1" by mistake fails loudly
	// rather than silently misrouting (the incoming request path is
	// what we forward).
	Upstream string

	// AllowNonLoopback opts into binding Start on a non-loopback
	// address. The proxy is unauthenticated on the local hop — the
	// security boundary is "only the agent subprocess can reach it"
	// enforced by network isolation. An accidental "0.0.0.0:NNNN"
	// bind would expose a credentialed proxy to the LAN; loopback-
	// only by default prevents that footgun.
	//
	// Future sandbox integration (the host-side veth IP for the
	// gVisor netns) is the legitimate non-loopback use case. Set
	// this true when binding to the veth gateway IP so the caller
	// has consciously acknowledged the bind is not loopback.
	AllowNonLoopback bool
}

// Server is a single per-run proxy instance. Not safe to share
// across runs — the credential it holds is org-scoped, and the
// request counter is request-scoped.
type Server struct {
	cfg          Config
	upstreamURL  *url.URL
	proxy        *httputil.ReverseProxy
	requestCount atomic.Int64

	// listener is owned once Start has been called. nil until then.
	listener net.Listener
	httpSrv  *http.Server
}

// New constructs a Server with the given config but does not start
// listening. Call Start to bind a port and begin serving.
//
// Validates config eagerly so a misconfigured caller fails at
// construction time rather than producing a Server that silently
// can't serve requests.
func New(cfg Config) (*Server, error) {
	switch cfg.Provider {
	case ProviderAnthropic, ProviderBedrockBearer:
		// supported
	default:
		return nil, fmt.Errorf("llmproxy: unsupported provider %q", cfg.Provider)
	}
	if cfg.APIKey == "" {
		return nil, errors.New("llmproxy: APIKey is required")
	}
	if cfg.Upstream == "" {
		return nil, errors.New("llmproxy: Upstream is required")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("llmproxy: parse upstream URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("llmproxy: upstream URL missing scheme or host: %q", cfg.Upstream)
	}
	// Reject paths / queries / fragments on the upstream URL. The
	// proxy preserves the incoming request path verbatim; a caller
	// who passed "https://api.anthropic.com/v1" would silently get
	// requests routed to "/v1/v1/messages" (path-joined) and 404
	// at the upstream. Fail at construction so the misconfiguration
	// surfaces at boot, not on the first request.
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("llmproxy: upstream URL must not include a path (got %q); the incoming request path is forwarded as-is", cfg.Upstream)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("llmproxy: upstream URL must not include query or fragment: %q", cfg.Upstream)
	}

	s := &Server{cfg: cfg, upstreamURL: u}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
		// Default ErrorHandler logs to stderr and 502s. That's fine
		// for Phase 1; upgrade observability comes later.
	}
	return s, nil
}

// rewrite is the Go 1.20+ ReverseProxy hook (replacing Director). It
// runs before the request is sent upstream and has explicit control
// over Out — unlike Director, the stdlib does NOT add X-Forwarded-*
// after Rewrite returns unless we call pr.SetXForwarded() ourselves.
// That lets us suppress those proxy-chain headers, which would just
// be noise to an LLM provider.
//
// The header rewrite uses Set (not Add) so any client-supplied auth
// header is overwritten, not duplicated. That matters because the SDK
// sends an empty or placeholder x-api-key depending on env shape; a
// duplicate header could confuse some HTTP/2 implementations and
// would absolutely confuse the upstream's auth path.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	// SetURL rewrites Out.URL.Scheme, Out.URL.Host, joins paths, and
	// sets Out.Host. Since we validated the upstream URL has no path,
	// SetURL preserves the incoming request path verbatim.
	pr.SetURL(s.upstreamURL)

	// Defensive: if the incoming request happened to carry
	// X-Forwarded-* headers (some misconfigured caller), drop them.
	// We deliberately do not call pr.SetXForwarded() — the stdlib
	// only adds these when explicitly invoked under the Rewrite API.
	pr.Out.Header.Del("X-Forwarded-For")
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Header.Del("X-Forwarded-Proto")
	pr.Out.Header.Del("Forwarded")

	switch s.cfg.Provider {
	case ProviderAnthropic:
		pr.Out.Header.Set("x-api-key", s.cfg.APIKey)
		// The SDK already sets anthropic-version; we don't override.
	case ProviderBedrockBearer:
		pr.Out.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	}
}

// modifyResponse is a hook for observability and per-request counter
// bumping. Returning an error here would 502 the client; we never do
// that in Phase 1 — just observe.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	return nil
}

// Handler exposes the proxy as a standard http.Handler. Useful for
// tests that want to drive the proxy via httptest.NewServer instead
// of the production Start path.
func (s *Server) Handler() http.Handler { return s.proxy }

// Start binds a TCP port and serves until Shutdown is called. Returns
// the bound address so the caller can construct the agent's BASE_URL
// env var.
//
// Defaults to 127.0.0.1:0 (random loopback port) when addr is "". A
// non-loopback bind requires Config.AllowNonLoopback=true — the proxy
// is unauthenticated on the local hop, so an accidental "0.0.0.0:NNNN"
// would expose a credentialed proxy to the LAN. The future sandbox
// case (binding to the host-side veth IP, e.g. 192.168.99.1) is the
// legitimate non-loopback use case and opts in via the Config flag.
func (s *Server) Start(addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if !s.cfg.AllowNonLoopback {
		if err := assertLoopback(addr); err != nil {
			return "", err
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("llmproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.httpSrv = &http.Server{
		Handler: s.proxy,
		// Conservative timeouts. The SDK uses long-lived streaming
		// connections for tool-use loops; ReadHeaderTimeout limits the
		// time to receive the request headers, not the body, so 30s is
		// fine. Total request time is unbounded (no WriteTimeout)
		// because streaming responses can run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		// Errors here are either "server closed" (expected on shutdown)
		// or a fatal bind/accept failure. The accept failure case is
		// rare and we don't have a great way to surface it to the
		// run, so we log via the default http.Server error logging.
		// Future: surface via a dedicated channel.
		_ = s.httpSrv.Serve(ln)
	}()
	return ln.Addr().String(), nil
}

// Shutdown stops serving and waits for in-flight requests to drain
// (up to the context's deadline). Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// RequestCount returns the number of upstream responses the proxy
// has observed. Useful for tests asserting the agent actually went
// through the proxy.
func (s *Server) RequestCount() int64 { return s.requestCount.Load() }

// assertLoopback returns nil iff addr binds to a loopback interface.
// Hostnames resolve via the OS resolver; every resolved IP must be
// loopback for the check to pass. The empty-host case (":NNNN" form
// binds to all interfaces) is rejected explicitly.
//
// Used by Start to enforce the safety default of loopback-only when
// AllowNonLoopback is false. The veth-IP sandbox case opts out.
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("llmproxy: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("llmproxy: bind address %q binds to all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	// If host parses as an IP literal, check directly.
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("llmproxy: bind address %q is not loopback — set AllowNonLoopback=true to confirm intent", addr)
		}
		return nil
	}
	// Hostname (e.g. "localhost"); resolve and require every result
	// to be loopback. "localhost" passes; "myhost.local" pointing at
	// a routable LAN IP does not.
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("llmproxy: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("llmproxy: bind host %q resolves to %s (not loopback) — set AllowNonLoopback=true to confirm intent", host, a)
		}
	}
	return nil
}
