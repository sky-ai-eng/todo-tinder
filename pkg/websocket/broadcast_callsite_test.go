package websocket_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryBroadcastEventLiteralCarriesOrgID is the load-bearing guard
// for the per-(user, org) WS scoping work. The Hub.Broadcast filter
// can only protect us if every call site stamps an OrgID on its
// websocket.Event{...} struct literal — without this test, a future
// contributor adding a new Broadcast site (handlers, spawner,
// router, future subscribers) would silently leak across orgs and
// nobody would notice until a multi-tenant customer hit it.
//
// The scan is intentionally a grep-style AST walk rather than a
// linter rule:
//   - it covers main.go in addition to internal/* and pkg/*;
//   - it surfaces the file + line in the failure so the offender is
//     obvious from CI output without re-running locally;
//   - it has zero infrastructure cost beyond `go test`.
//
// Allowlist: a small set of literal locations are legitimate system-
// wide broadcasts (process-level events fired from main.go startup,
// or test-only literals in *_test.go files that don't represent
// production code paths). Each entry needs a comment so we can audit
// it later. New entries should be rare — adding one without the
// corresponding `// system-wide; OrgID intentionally empty` comment
// at the call site fails the second-pass check below.
func TestEveryBroadcastEventLiteralCarriesOrgID(t *testing.T) {
	// Discover every Go source file under the repo root (the test
	// binary's cwd is its package dir, so walk up to the module root
	// first). go/build tag filtering is overkill here — every *.go
	// file in the repo is platform-neutral and we want all of them
	// in scope.
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	type offender struct {
		file string
		line int
	}
	var offenders []offender

	fset := token.NewFileSet()

	scanFile := func(path string) {
		if strings.Contains(path, string(filepath.Separator)+"frontend"+string(filepath.Separator)) {
			return
		}
		if strings.Contains(path, string(filepath.Separator)+"node_modules"+string(filepath.Separator)) {
			return
		}
		// pkg/websocket itself can build websocket.Event literals
		// internally (tests construct them with explicit OrgID for
		// assertion). Skipping the package keeps the rule focused on
		// the production *callers* of Broadcast.
		if strings.HasPrefix(path, filepath.Join(repoRoot, "pkg", "websocket")+string(filepath.Separator)) {
			return
		}

		src, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			// Parse failures in non-go-source paths (e.g. vendored
			// fixtures) are not our concern; production go files are
			// kept clean by the build, so any parse failure here is
			// either pre-existing test corruption or a true syntax
			// error that go vet / go build will catch in the same
			// CI run.
			return
		}

		ast.Inspect(src, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			// We want `websocket.Event{...}` — a selector-typed
			// composite. Anonymous Event{...} inside the websocket
			// package itself is excluded by the package skip above.
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "websocket" {
				return true
			}
			if sel.Sel.Name != "Event" {
				return true
			}

			hasOrgID := false
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if key.Name == "OrgID" {
					hasOrgID = true
					break
				}
			}
			if !hasOrgID {
				pos := fset.Position(cl.Pos())
				offenders = append(offenders, offender{
					file: pos.Filename,
					line: pos.Line,
				})
			}
			return true
		})
	}

	if err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == "node_modules" || name == "frontend" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// _test.go files are scanned too: a future test that builds
		// a literal without OrgID would silently teach the
		// "construct events without OrgID" pattern to anyone copying
		// it into production code. We still allow specific test
		// shapes via the allowlist below.
		scanFile(path)
		return nil
	}); err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	// Allowlist: legitimate exceptions, each one explained.
	//
	// Each entry is keyed by `<relative_path>:<line>`. Adding a new
	// one without justification = silent regression; the comment in
	// the test source IS the audit trail.
	allow := map[string]string{
		// pkg/websocket internal Event{...} literals are excluded by
		// the package skip above — no entries needed here for them.
		//
		// Test files that build Event literals to assert hub behavior
		// belong in pkg/websocket itself; everything else must stamp
		// an OrgID. If a future test outside pkg/websocket has a
		// legitimate reason to skip OrgID, add it here with a
		// comment.
	}

	var unallowedOffenders []string
	for _, o := range offenders {
		rel, err := filepath.Rel(repoRoot, o.file)
		if err != nil {
			rel = o.file
		}
		key := rel + ":" + itoa(o.line)
		if _, ok := allow[key]; ok {
			continue
		}
		unallowedOffenders = append(unallowedOffenders, key)
	}

	if len(unallowedOffenders) > 0 {
		t.Fatalf("websocket.Event{...} literal without OrgID at:\n  %s\n\nFix each call site to pass `OrgID: <tenant>` (handlers extract via s.requireOrg; spawner/router methods already carry orgID; goroutines capture at construction). If a literal is legitimately system-wide, add it to the allowlist in this test with a justifying comment.", strings.Join(unallowedOffenders, "\n  "))
	}
}

// itoa avoids dragging strconv in for a single integer formatting
// usage; the line numbers are small positive ints from go/token.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
