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
	// The path portion is preserved from each request; only scheme +
	// host get rewritten.
	Upstream string
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

	s := &Server{cfg: cfg, upstreamURL: u}
	s.proxy = &httputil.ReverseProxy{
		Director:       s.director,
		ModifyResponse: s.modifyResponse,
		// Default ErrorHandler logs to stderr and 502s. That's fine
		// for Phase 1; upgrade observability comes later.
	}
	return s, nil
}

// director rewrites each outbound request to point at the upstream
// host and injects the right auth header. Called by ReverseProxy for
// every forwarded request.
//
// The header rewrite uses Set (not Add) so any client-supplied auth
// header is overwritten, not duplicated. That matters because the SDK
// may send an empty or placeholder x-api-key (depending on how the
// caller configured its env); a duplicate header could confuse some
// HTTP/2 implementations and would absolutely confuse the upstream's
// auth path.
func (s *Server) director(req *http.Request) {
	// Rewrite scheme + host + Host header. Path + query pass through
	// unchanged — the SDK's request URL already encodes the correct
	// API path (/v1/messages for Anthropic, /model/.../invoke for
	// Bedrock).
	req.URL.Scheme = s.upstreamURL.Scheme
	req.URL.Host = s.upstreamURL.Host
	req.Host = s.upstreamURL.Host

	// Strip headers ReverseProxy doesn't already strip and that
	// confuse upstreams. X-Forwarded-* are added by ReverseProxy's
	// default behavior; for an LLM API call those just look like
	// proxy noise. Setting them to nothing tells ReverseProxy to skip
	// the default add (per the stdlib docs).
	req.Header.Del("X-Forwarded-For")
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")

	switch s.cfg.Provider {
	case ProviderAnthropic:
		req.Header.Set("x-api-key", s.cfg.APIKey)
		// The SDK already sets anthropic-version; we don't override.
	case ProviderBedrockBearer:
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
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

// Start binds a localhost TCP port (random if addr is "" or ":0")
// and serves until Shutdown is called. Returns the bound address so
// the caller can construct the agent's BASE_URL env var.
//
// Listening on 127.0.0.1 (not 0.0.0.0) so the proxy is reachable only
// from the same host. In Phase 1 that's the TF process + the Node
// child it spawned; in Phase 2 (sandbox case) we switch to a unix
// socket bind-mounted into the sandbox, which is even tighter.
func (s *Server) Start(addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("llmproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.httpSrv = &http.Server{
		Handler: s.proxy,
		// Conservative timeouts. The SDK uses long-lived streaming
		// connections for tool-use loops; ReadTimeout is the time to
		// receive the request headers, not the body, so 30s is fine.
		// Total request time is unbounded (no WriteTimeout) because
		// streaming responses can run for minutes.
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
