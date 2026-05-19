// Package gitproxy is a per-run HTTP intermediary that holds a GitHub
// App installation access token on the trusted side and injects it as
// Basic auth on outbound git protocol requests.
//
// # The threat it addresses
//
// The same Property B problem the LLM proxy solves (see
// internal/llmproxy) applies to git auth: anything the sandboxed agent
// can read — env vars, .git/config, the worktree filesystem — is
// exfiltratable via prompt injection. A long-lived GitHub PAT in the
// sandbox is a tenant-wide credential one tool-output leak away from
// public exposure.
//
// This package keeps the credential on the host side, hands the agent
// only an unauthenticated proxy URL, and rewrites the Authorization
// header on each outbound request before forwarding to GitHub.
//
// # Credential class
//
// Phase 1 minted PATs straight from the user. This phase moves git
// auth onto GitHub App installation access tokens, which:
//
//   - Live 1 hour, not indefinitely. A leaked token has minutes of
//     useful life.
//   - Scope to one installation (one org's repo set), not the user's
//     entire access. A compromised proxy cannot reach a different
//     tenant's data.
//   - Mint on demand from the App's private key + installation ID via
//     internal/githubapp. The private key never crosses to the agent.
//
// # Auth-header shape
//
// GitHub's git-over-HTTPS protocol accepts installation tokens via the
// Basic-auth scheme with a fixed username:
//
//	Authorization: Basic base64("x-access-token:" + <token>)
//
// This is documented at
// docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation.
// The "x-access-token" string is the literal sentinel — it's not the
// installation's display name or anything similar.
//
// # Caching
//
// The proxy caches the installation token in memory for the lifetime
// of the Server. First request mints; subsequent requests reuse until
// the token is within refreshThreshold of its expires_at, at which
// point a fresh mint replaces it. Concurrent requests during refresh
// coalesce on a single mint call via the mutex — no thundering herd.
//
// A run's proxy is single-installation, so a single cached token
// suffices. Multi-installation orgs are out of scope for v1.
//
// # Trust model on the local hop
//
// Same as the LLM proxy. Loopback-only by default; non-loopback
// (sandbox veth IP) requires AllowNonLoopback=true. The local hop is
// unauthenticated because reaching the proxy means the caller is on
// the correct side of the sandbox boundary — that boundary is
// enforced by the gVisor netns + iptables, not by proxy-level auth.
package gitproxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// refreshThreshold is how close to expiry the cached token gets before
// the next request triggers a fresh mint. Installation tokens are
// 1-hour TTL; 5 minutes' headroom is comfortably more than the
// roundtrip + mint time and gives in-flight requests time to finish
// against the old token even if the clock skews.
const refreshThreshold = 5 * time.Minute

// defaultUpstream is the github.com hostname used by git-over-HTTPS.
// Different from the REST API base (api.github.com); both are
// configurable but they're not the same thing.
const defaultUpstream = "https://github.com"

// Token is the contract between the minter and the proxy. The proxy
// doesn't care how the token was obtained, only that it has a value
// and an expiry. Compatible by-shape with githubapp.Token but typed
// separately so this package doesn't force the dependency on callers
// who want to plug in a different source (e.g. for tests).
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// TokenSource is the abstraction over "how to get a fresh installation
// token". Production callers wrap githubapp.Minter.MintInstallationToken
// closing over the installationID; tests pass a stub returning a fixed
// value.
//
// The Server calls TokenSource lazily on first request, caches the
// result, and re-invokes when the cached token is within
// refreshThreshold of expiry. Implementations should be safe for
// concurrent invocation, though the proxy serializes calls itself.
type TokenSource func(ctx context.Context) (Token, error)

// Config bundles the inputs a Server needs. Per-run construction; the
// token cache is per-Server so a new run gets a fresh credential and a
// dead one's tokens go to GC.
type Config struct {
	// TokenSource mints fresh installation tokens. Required.
	TokenSource TokenSource

	// Upstream is the absolute URL of the real git host — usually
	// "https://github.com". GitHub Enterprise Server passes its own
	// hostname (the customer's responsibility per the ticket scope).
	//
	// Only scheme + host are honored; path / query / fragment are
	// rejected at construction so a misconfigured caller fails loudly.
	// Default: defaultUpstream.
	Upstream string

	// AllowNonLoopback opts into binding Start on a non-loopback
	// address. The proxy is unauthenticated on the local hop — the
	// security boundary is "only the agent subprocess can reach it"
	// enforced by network isolation. An accidental "0.0.0.0:NNNN" bind
	// would expose a credentialed proxy to the LAN.
	//
	// The sandbox case (binding to the host-side veth IP, e.g.
	// 192.168.99.1) is the legitimate non-loopback use case and opts
	// in via this flag.
	AllowNonLoopback bool

	// RunID is the run identifier this proxy serves. Carried for
	// future per-run policy / observability; the proxy itself does not
	// branch on it today.
	RunID string
}

// Server is a single per-run proxy instance with a cached installation
// token. Not safe to share across runs — the token it holds is
// installation-scoped, and the request counter is request-scoped.
type Server struct {
	cfg         Config
	upstreamURL *url.URL
	proxy       *httputil.ReverseProxy

	requestCount atomic.Int64

	// tokenMu serializes token cache access. The cache is a single
	// (value, expiry) tuple; concurrent requests during refresh
	// coalesce on the mutex rather than thundering-herd the minter.
	tokenMu      sync.Mutex
	cachedToken  Token
	cachedNonces atomic.Int64 // total mint-source invocations; observable for tests

	// listener is owned once Start has been called. nil until then.
	listener net.Listener
	httpSrv  *http.Server
	// serveErr receives the first non-ErrServerClosed error from
	// httpSrv.Serve. Buffered(1) so the goroutine never blocks on
	// send, even if the caller never reads it.
	serveErr chan error

	// now is the testable clock. nil in production (time.Now is used).
	now func() time.Time
}

// New constructs a Server with the given config but does not start
// listening. Call Start to bind a port and begin serving.
//
// Validates config eagerly so a misconfigured caller fails at
// construction time rather than producing a Server that silently
// 5xx's every request.
func New(cfg Config) (*Server, error) {
	if cfg.TokenSource == nil {
		return nil, errors.New("gitproxy: TokenSource is required")
	}
	upstream := cfg.Upstream
	if upstream == "" {
		upstream = defaultUpstream
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("gitproxy: parse upstream URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gitproxy: upstream URL missing scheme or host: %q", upstream)
	}
	// Reject paths / queries / fragments on the upstream URL. The proxy
	// preserves the incoming request path verbatim; a caller who passed
	// "https://github.com/x" by mistake would route every git request
	// under "/x/" and 404 at the upstream.
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("gitproxy: upstream URL must not include a path (got %q); the incoming request path is forwarded as-is", upstream)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("gitproxy: upstream URL must not include query or fragment: %q", upstream)
	}
	// Require HTTPS for non-loopback upstreams — the installation
	// token travels in the rewritten Authorization header and must not
	// cross cleartext. Loopback http is permitted for httptest in unit
	// tests; real GitHub / GHE are https.
	if u.Scheme != "https" {
		host, _, _ := net.SplitHostPort(u.Host)
		if host == "" {
			host = u.Host
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("gitproxy: upstream %q must use https (loopback http is allowed for tests)", upstream)
		}
	}

	s := &Server{cfg: cfg, upstreamURL: u}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
	}
	return s, nil
}

// Handler exposes the proxy as an http.Handler. Useful for tests that
// drive the proxy via httptest.NewServer rather than the production
// Start path, and for callers that want to compose middleware (e.g.
// adding observability) outside the listener loop.
//
// The returned handler does the credential injection before delegating
// to the underlying ReverseProxy: a failure to mint a token surfaces
// as a 502 here rather than via the ReverseProxy's silent-pass-broken-
// auth path.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, err := s.installationToken(r.Context())
		if err != nil {
			// 502 Bad Gateway maps cleanly: the proxy is alive but the
			// upstream credential pipeline is broken. Avoid leaking the
			// error detail to the agent — the underlying mint error
			// may include the App ID or other identifying info.
			http.Error(w, "gitproxy: failed to obtain installation token", http.StatusBadGateway)
			return
		}
		// Stash the token on the request context so Rewrite can pick
		// it up. Passing through context (rather than mutating headers
		// here) keeps the token off the inbound request's header set,
		// which means it never appears in a log of pr.In.Header.
		r = r.WithContext(context.WithValue(r.Context(), tokenCtxKey{}, tok.Value))
		s.proxy.ServeHTTP(w, r)
	})
}

// tokenCtxKey is the request-context key used to hand the resolved
// installation token from the Handler wrapper to the Rewrite hook.
// Unexported empty struct so external code cannot collide.
type tokenCtxKey struct{}

// installationToken returns a valid cached token, minting a fresh one
// if the cache is empty or within refreshThreshold of expiry.
//
// Serialized via tokenMu — concurrent requests during a refresh
// coalesce on the mutex rather than stampeding the upstream mint
// endpoint. The mutex held during the upstream call is acceptable here
// because mint is on the run's critical path anyway and TokenSource
// implementations are expected to be fast (sub-second for the GitHub
// /app/installations/.../access_tokens endpoint).
func (s *Server) installationToken(ctx context.Context) (Token, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()

	now := s.timeNow()
	if s.cachedToken.Value != "" && now.Add(refreshThreshold).Before(s.cachedToken.ExpiresAt) {
		return s.cachedToken, nil
	}

	tok, err := s.cfg.TokenSource(ctx)
	if err != nil {
		return Token{}, fmt.Errorf("token source: %w", err)
	}
	if tok.Value == "" {
		return Token{}, errors.New("token source returned empty token")
	}
	if tok.ExpiresAt.IsZero() {
		return Token{}, errors.New("token source returned zero expires_at")
	}
	if !tok.ExpiresAt.After(now.Add(refreshThreshold)) {
		return Token{}, fmt.Errorf("token source returned expired or near-expiry token (expires_at=%s)", tok.ExpiresAt.Format(time.RFC3339))
	}
	s.cachedToken = tok
	s.cachedNonces.Add(1)
	return tok, nil
}

// rewrite is the Go 1.20+ ReverseProxy hook (replacing Director). It
// runs before the request is sent upstream and has explicit control
// over Out — unlike Director, the stdlib does NOT add X-Forwarded-*
// after Rewrite returns unless we call pr.SetXForwarded() ourselves.
// That lets us suppress those proxy-chain headers, which would just
// be noise to GitHub.
//
// The header rewrite uses Set (not Add) so any client-supplied auth
// header is overwritten, not duplicated. A duplicate Authorization
// would confuse some HTTP/2 implementations and would absolutely
// confuse the upstream's auth path.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	// SetURL rewrites Out.URL.Scheme, Out.URL.Host, joins paths, and
	// sets Out.Host. Since we validated the upstream URL has no path,
	// SetURL preserves the incoming request path verbatim.
	pr.SetURL(s.upstreamURL)

	// Defensive: drop any X-Forwarded-* headers an upstream might
	// trust. We deliberately do not call pr.SetXForwarded().
	pr.Out.Header.Del("X-Forwarded-For")
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Header.Del("X-Forwarded-Proto")
	pr.Out.Header.Del("Forwarded")

	tok, _ := pr.In.Context().Value(tokenCtxKey{}).(string)
	if tok == "" {
		// Defense in depth: if the Handler wrapper somehow skipped
		// us, fail closed by stripping any caller-supplied auth so
		// the request goes anonymous (which github will 401) rather
		// than passing through a potentially-leaked credential.
		pr.Out.Header.Del("Authorization")
		return
	}
	pr.Out.Header.Set("Authorization", basicAuthHeader(tok))
}

// basicAuthHeader returns the "Basic <b64>" string for GitHub App
// installation tokens. Exported via the tests so the encoding can be
// pinned without re-implementing it inline; kept package-private
// because nothing outside the proxy needs it.
func basicAuthHeader(token string) string {
	creds := "x-access-token:" + token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// modifyResponse is a hook for observability and per-request counter
// bumping. Returning an error here would 502 the client; we never do
// that — just observe.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	return nil
}

// Start binds a TCP port and serves until Shutdown is called. Returns
// the bound address so the caller can construct the agent-side git
// proxy URL.
//
// Defaults to 127.0.0.1:0 (random loopback port) when addr is "". A
// non-loopback bind requires Config.AllowNonLoopback=true — the proxy
// is unauthenticated on the local hop, so an accidental "0.0.0.0:NNNN"
// would expose a credentialed proxy to the LAN. The sandbox case
// (binding to the host-side veth IP, e.g. 192.168.99.1) is the
// legitimate non-loopback use and opts in via the Config flag.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("gitproxy: already started; create a new Server per run")
	}
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
		return "", fmt.Errorf("gitproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler: s.Handler(),
		// Conservative timeouts. Git operations can be slow (large
		// pack-files take minutes); ReadHeaderTimeout limits header
		// receive, not body. Total request time is unbounded (no
		// WriteTimeout) because git pack uploads can run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		err := s.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
		close(s.serveErr)
	}()
	return ln.Addr().String(), nil
}

// Err returns a channel that receives the first unexpected error from
// the background Serve goroutine (i.e. not http.ErrServerClosed). The
// channel is closed when the goroutine exits, so a range or select on
// it unblocks after Shutdown.
//
// Callers that do not need to monitor the error can ignore this channel
// safely — it is buffered and the goroutine never blocks on send.
func (s *Server) Err() <-chan error { return s.serveErr }

// Shutdown stops serving and waits for in-flight requests to drain
// (up to the context's deadline). Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// RequestCount returns the number of upstream responses the proxy has
// observed. Useful for tests asserting the agent actually went through
// the proxy.
func (s *Server) RequestCount() int64 { return s.requestCount.Load() }

// MintCount returns the number of times TokenSource has been invoked.
// Exposed so tests can verify caching behavior (first request mints;
// subsequent requests reuse).
func (s *Server) MintCount() int64 { return s.cachedNonces.Load() }

// timeNow returns the current time, honoring the testable now hook.
func (s *Server) timeNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

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
		return fmt.Errorf("gitproxy: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("gitproxy: bind address %q binds to all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("gitproxy: bind address %q is not loopback — set AllowNonLoopback=true to confirm intent", addr)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("gitproxy: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("gitproxy: bind host %q resolves to %s (not loopback) — set AllowNonLoopback=true to confirm intent", host, a)
		}
	}
	return nil
}
