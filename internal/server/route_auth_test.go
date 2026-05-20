package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAllProtectedRoutes_RejectUnauthenticated probes every /api/* route
// registered via s.api / s.apiMutating with three flavors of unauth and
// asserts each comes back as 401. Routes are discovered by AST-parsing
// server.go so coverage can't drift behind new mounts — if someone adds
// a route via s.api/s.apiMutating, the test picks it up on the next run.
//
// The variants:
//
//	no cookie       — most basic gate; requireAuth not wired at all
//	garbage value   — cookie present but value isn't a UUID
//	unknown sid     — well-formed UUID with no matching session row
//
// All three exit at writeUnauth in withSession before any handler or DB
// query runs, so the test is fast (~ms per route) and the variants don't
// need session-row fixtures. The path-param substitutions don't have to
// resolve to real entities because middleware fires before path-param
// parsing.
//
// What this test does NOT cover:
//
//   - Expired-but-extant sid: separate test; needs a real session row.
//     Existing TestAuthFlow_ForceExpiryReturns401 covers the mechanism.
//   - Cross-org access (sid for user A, query org B's resource): RLS
//     concern, covered by cross_org_http_test.go.
//   - CSRF rejection paths: covered by TestAuthFlow_Logout_CSRFOriginCheck.
//
// The static "is this route wrapped at all" guard lives in
// routes_coverage_test.go; this is the runtime complement that proves
// wrapping actually rejects bad input.
func TestAllProtectedRoutes_RejectUnauthenticated(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	protected := discoverProtectedRoutes(t)
	if len(protected) == 0 {
		t.Fatalf("AST discovered 0 protected routes — parser broken")
	}

	sidName := rig.srv.sidCookieName()
	publicURL := rig.srv.authCfg.publicURL

	variants := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"garbage value", &http.Cookie{Name: sidName, Value: "not-a-uuid"}},
		{"unknown sid", &http.Cookie{Name: sidName, Value: uuid.NewString()}},
	}

	for _, route := range protected {
		method, pattern := splitMethodAndPath(route.pattern)
		concrete := substitutePathParams(pattern)
		for _, v := range variants {
			t.Run(method+" "+pattern+"/"+v.name, func(t *testing.T) {
				req := httptest.NewRequest(method, concrete, nil)
				if v.cookie != nil {
					req.AddCookie(v.cookie)
				}
				// Same-origin so withCSRFOriginCheck on mutating
				// routes passes — we're testing the auth gate, not
				// CSRF, and an unset Origin would also pass anyway.
				req.Header.Set("Origin", publicURL)

				rec := httptest.NewRecorder()
				rig.srv.mux.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status=%d body=%q, want 401 (variant=%s, via=%s)",
						rec.Code, strings.TrimSpace(rec.Body.String()), v.name, route.via)
				}
			})
		}
	}
}

// protectedRoute is one route discovered by AST inspection of
// (s *Server).routes().
type protectedRoute struct {
	pattern string // e.g. "POST /api/tasks/{id}/swipe"
	via     string // "api" or "apiMutating"
}

// discoverProtectedRoutes parses server.go, walks (s *Server).routes(),
// and returns every mount registered via s.api / s.apiMutating. Raw
// s.mux.Handle* calls (pre-auth allowlist + the SPA / GoTrue proxy) are
// excluded — those are already covered by routes_coverage_test.go.
//
// Reuses the same parsing approach so the two tests can't disagree
// about what counts as "a route mount."
func discoverProtectedRoutes(t *testing.T) []protectedRoute {
	t.Helper()
	const sourceFile = "server.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, sourceFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", sourceFile, err)
	}

	var routesFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "routes" {
			continue
		}
		routesFn = fn
		break
	}
	if routesFn == nil {
		t.Fatalf("could not find (s *Server).routes() in %s", sourceFile)
	}

	var out []protectedRoute
	ast.Inspect(routesFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "s" {
			return true
		}
		switch sel.Sel.Name {
		case "api", "apiMutating":
		default:
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out = append(out, protectedRoute{pattern: pattern, via: sel.Sel.Name})
		return true
	})
	return out
}

// splitMethodAndPath splits "POST /api/foo/{id}" into ("POST",
// "/api/foo/{id}"). Patterns without a leading method (Go 1.22 mux
// permits this for any-method mounts) default to GET — none of our
// protected routes look like that today, but the fallback is safe.
func splitMethodAndPath(pattern string) (method, path string) {
	if i := strings.Index(pattern, " "); i >= 0 {
		return pattern[:i], pattern[i+1:]
	}
	return http.MethodGet, pattern
}

// substitutePathParams replaces every {name} placeholder with a
// concrete value the mux pattern will match. Middleware rejects unauth
// requests before any handler or path-param parsing runs, so the values
// don't need to resolve to real entities — they only need to satisfy
// the pattern's path-segment shape (no slashes inside a {name}).
func substitutePathParams(path string) string {
	zeroUUID := "00000000-0000-0000-0000-000000000000"
	replacements := map[string]string{
		"{id}":         zeroUUID,
		"{runID}":      zeroUUID,
		"{commentId}":  zeroUUID,
		"{number}":     "0",
		"{provider}":   "github",
		"{owner}":      "octocat",
		"{repo}":       "hello-world",
		"{path}":       "readme.md",
		"{event_type}": "github.pr.opened",
	}
	out := path
	for placeholder, value := range replacements {
		out = strings.ReplaceAll(out, placeholder, value)
	}
	// Catch-all: any remaining {name} segment gets a generic
	// alphanumeric value. Keeps the test working if a new route
	// introduces a placeholder we haven't seen before, rather than
	// silently 404ing because the unsubstituted "{newparam}" doesn't
	// match the registered pattern.
	for strings.Contains(out, "{") {
		openIdx := strings.Index(out, "{")
		closeIdx := strings.Index(out, "}")
		if closeIdx < openIdx {
			break
		}
		out = out[:openIdx] + "placeholder" + out[closeIdx+1:]
	}
	return out
}
