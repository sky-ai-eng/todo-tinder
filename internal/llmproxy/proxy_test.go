package llmproxy_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// upstreamRecord captures what the fake upstream observed so tests
// can assert the proxy rewrote things correctly.
type upstreamRecord struct {
	method     string
	path       string
	xAPIKey    string
	authHeader string
	host       string
	body       string
	hits       atomic.Int64
}

// fakeUpstream stands in for api.anthropic.com or
// bedrock-runtime.<region>.amazonaws.com. Records what arrived,
// returns a canned response (optionally streaming for the SSE test).
func fakeUpstream(rec *upstreamRecord, stream bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.xAPIKey = r.Header.Get("x-api-key")
		rec.authHeader = r.Header.Get("Authorization")
		rec.host = r.Host
		body, _ := io.ReadAll(r.Body)
		rec.body = string(body)

		if stream {
			// Mimic SSE — ReverseProxy needs to handle chunked writes
			// without buffering or the live SDK case will deadlock on
			// long-running tool-use streams.
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "event: chunk\ndata: {\"i\":%d}\n\n", i)
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(10 * time.Millisecond)
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	}))
}

// startProxyWithAddr boots a llmproxy.Server pointed at upstream and
// returns both the Server and its "http://127.0.0.1:PORT" URL ready
// to drop into a request. Caller is responsible for Shutdown (via
// t.Cleanup registered here).
func startProxyWithAddr(t *testing.T, provider llmproxy.Provider, apiKey, upstream string) (*llmproxy.Server, string) {
	t.Helper()
	srv, err := llmproxy.New(llmproxy.Config{
		Provider: provider,
		APIKey:   apiKey,
		Upstream: upstream,
	})
	if err != nil {
		t.Fatalf("llmproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	return srv, "http://" + addr
}

// TestProxyInjectsAnthropicAPIKey is the load-bearing assertion of
// Phase 1's Anthropic-direct path: a request arriving at the proxy
// with NO x-api-key header should leave the proxy with x-api-key set
// to the real key from the config. This is the credential-injection
// step that makes the agent-side env clean.
func TestProxyInjectsAnthropicAPIKey(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "sk-ant-REAL-KEY", upstream.URL)

	body := strings.NewReader(`{"model":"claude-haiku","messages":[]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", body)
	// Caller-supplied x-api-key is deliberately wrong — proxy must
	// overwrite, not duplicate or append.
	req.Header.Set("x-api-key", "caller-supplied-garbage")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()

	if rec.hits.Load() != 1 {
		t.Fatalf("upstream hit count = %d, want 1", rec.hits.Load())
	}
	if rec.xAPIKey != "sk-ant-REAL-KEY" {
		t.Errorf("upstream x-api-key = %q, want sk-ant-REAL-KEY (proxy must overwrite the caller's value)", rec.xAPIKey)
	}
	if rec.path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages (proxy must preserve the request path)", rec.path)
	}
	if rec.body != `{"model":"claude-haiku","messages":[]}` {
		t.Errorf("upstream body = %q; want passthrough unchanged", rec.body)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestProxyInjectsBedrockBearer is the Bedrock-bearer-path mirror of
// the Anthropic test. The header name is different (Authorization)
// and the value shape is "Bearer <key>" but the rewrite logic is
// identical.
func TestProxyInjectsBedrockBearer(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderBedrockBearer, "bdrk-REAL-BEARER", upstream.URL)

	req, _ := http.NewRequest("POST", proxyURL+"/model/anthropic.claude-haiku-4-5/invoke", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer caller-supplied-garbage")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()

	if rec.authHeader != "Bearer bdrk-REAL-BEARER" {
		t.Errorf("upstream Authorization = %q, want \"Bearer bdrk-REAL-BEARER\"", rec.authHeader)
	}
	if !strings.HasPrefix(rec.path, "/model/") {
		t.Errorf("upstream path = %q, want Bedrock invoke path preserved", rec.path)
	}
}

// TestProxyStreamingResponsePassesThrough pins SSE / chunked-response
// behavior. The agent SDK uses streaming for every model call; if
// the proxy buffers responses, the agent's stream-json parser would
// see the whole batch at once and behavior would diverge from a
// direct connection.
func TestProxyStreamingResponsePassesThrough(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, true)
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "sk-ant-stream", upstream.URL)

	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streamed body: %v", err)
	}
	got := string(body)
	for i := 0; i < 3; i++ {
		want := fmt.Sprintf(`"i":%d`, i)
		if !strings.Contains(got, want) {
			t.Errorf("streamed body missing chunk %d (looking for %q) in %q", i, want, got)
		}
	}
}

// TestProxyRequestCount tracks ReverseProxy.ModifyResponse firing
// per upstream response. Useful as the test-time signal that an
// agent actually went through the proxy (vs. bypassing it via some
// fallback path).
func TestProxyRequestCount(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	srv, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "sk", upstream.URL)

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if got := srv.RequestCount(); got != 5 {
		t.Errorf("RequestCount = %d, want 5", got)
	}
}

// TestProxyRejectsInvalidConfig pins the construction-time validation
// so callers fail loudly at boot rather than producing a Server that
// silently doesn't work.
func TestProxyRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  llmproxy.Config
	}{
		{"unknown_provider", llmproxy.Config{Provider: "vertex", APIKey: "k", Upstream: "https://x"}},
		{"empty_apikey", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "", Upstream: "https://x"}},
		{"empty_upstream", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: ""}},
		{"upstream_no_scheme", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: "api.anthropic.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := llmproxy.New(c.cfg); err == nil {
				t.Errorf("New(%+v) = nil err; want validation error", c.cfg)
			}
		})
	}
}

// TestProxyForwardsHostHeader confirms the Host header sent to the
// upstream matches the upstream's hostname, not the proxy's. Some
// HTTPS terminators / WAFs reject requests where Host doesn't match
// the SNI / certificate, so getting this wrong would cause real
// Anthropic to 4xx the request.
func TestProxyForwardsHostHeader(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "k", upstream.URL)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	// fakeUpstream is an httptest.Server, so its host is something
	// like 127.0.0.1:NNNN. The proxy must rewrite req.Host to match
	// that, not pass through the proxy's own host.
	wantHost := strings.TrimPrefix(upstream.URL, "http://")
	if rec.host != wantHost {
		t.Errorf("upstream Host = %q, want %q (proxy must rewrite Host header to upstream's value)", rec.host, wantHost)
	}
}
