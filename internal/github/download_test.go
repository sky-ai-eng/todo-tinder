package github

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// clientAgainst wires a Client at a specific test server's base URL. The
// stock NewClient hardcodes github.com → api.github.com rewriting, which
// we don't want in tests — we point directly at the httptest server.
func clientAgainst(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		pat:     "test-token",
		http:    http.DefaultClient,
	}
}

func TestDownloadArtifact_SuccessfulDownload(t *testing.T) {
	payload := []byte("hello, world — this pretends to be a zip archive")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing or wrong Authorization header: %q", got)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	n, err := c.DownloadArtifact(context.Background(), "/anywhere", &dst, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes written = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Errorf("body mismatch: got %q, want %q", dst.String(), string(payload))
	}
}

// TestDownloadArtifact_FollowsRedirect simulates GitHub's actual flow: the
// logs endpoint returns a 302 to a signed URL, and we should transparently
// follow to the second server and stream its body.
//
// Note on the cross-origin auth-strip behavior: Go's stdlib strips
// Authorization/Cookie headers on redirects to a different host (not a
// subdomain). In production that happens when api.github.com redirects to
// pipelines.actions.githubusercontent.com — different hosts, header gets
// stripped, signed URL accepts the anonymous request, everything works.
//
// We cannot reproduce that in a unit test: httptest.NewServer always binds
// to 127.0.0.1 with a fresh port, so two test servers share a hostname and
// stdlib considers them same-origin. The assertion we'd *want* to make —
// "signed URL receives no Authorization header" — would pass in prod and
// fail in test purely because of loopback semantics. Relying on the stdlib
// documentation for that guarantee; this test just verifies the
// redirect-follow path works and the final body is returned correctly.
func TestDownloadArtifact_FollowsRedirect(t *testing.T) {
	payload := []byte("signed URL body content")

	// Signed-URL server. Accepts any request (in prod, this is where the
	// stripped-auth request lands; in test, stdlib forwards the Bearer
	// token because both servers are same-host, and that's fine for the
	// assertion we can actually make).
	signedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(signedSrv.Close)

	// Primary server. Returns a 302 to the signed URL.
	primarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("primary should see Bearer token, got %q", got)
		}
		http.Redirect(w, r, signedSrv.URL+"/signed-blob", http.StatusFound)
	}))
	t.Cleanup(primarySrv.Close)

	c := clientAgainst(primarySrv.URL)
	var dst bytes.Buffer
	n, err := c.DownloadArtifact(context.Background(), "/repos/foo/bar/actions/runs/42/logs", &dst, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Errorf("body mismatch: got %q, want %q", dst.String(), string(payload))
	}
}

// TestDownloadArtifact_ContentLengthExceedsCap verifies the pre-flight cap
// check — we should refuse to read a single byte when the server advertises
// a Content-Length larger than our cap. This is the fast path; the
// runtime check below catches servers that lie or omit the header.
func TestDownloadArtifact_ContentLengthExceedsCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Claim 10 KB; we don't care what body we send because the cap
		// check should fire before we touch it.
		w.Header().Set("Content-Length", "10240")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("A"), 10240))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	_, err := c.DownloadArtifact(context.Background(), "/whatever", &dst, 1024)
	if err == nil {
		t.Fatal("expected cap-exceeded error, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should mention size, got: %v", err)
	}
}

// TestDownloadArtifact_StreamOverflowWithoutContentLength covers the
// belt-and-suspenders runtime cap: if Content-Length is missing (or wrong),
// io.LimitReader catches content that streams past the cap.
func TestDownloadArtifact_StreamOverflowWithoutContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Chunked transfer — no Content-Length advertised.
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("B"), 512))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	_, err := c.DownloadArtifact(context.Background(), "/whatever", &dst, 1024)
	if err == nil {
		t.Fatal("expected runtime cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should mention size, got: %v", err)
	}
}

func TestDownloadArtifact_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	_, err := c.DownloadArtifact(context.Background(), "/missing", &dst, 1024)
	if err == nil {
		t.Fatal("expected 404 error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should include status code, got: %v", err)
	}
}
