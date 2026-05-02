package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestProjectCreate_Happy(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name":         "Triage Factory",
		"description":  "Local-first triage UI",
		"pinned_repos": []string{"sky-ai-eng/triage-factory"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var got domain.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" {
		t.Error("expected generated id")
	}
	if got.Name != "Triage Factory" {
		t.Errorf("name = %q", got.Name)
	}
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "sky-ai-eng/triage-factory" {
		t.Errorf("pinned_repos = %v", got.PinnedRepos)
	}
}

func TestProjectCreate_RejectsEmptyName(t *testing.T) {
	s := newTestServer(t)
	for _, name := range []string{"", "   ", "\t"} {
		rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{"name": name})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name=%q status = %d, want 400", name, rec.Code)
		}
	}
}

func TestProjectCreate_RejectsBadPinnedRepoSlugs(t *testing.T) {
	s := newTestServer(t)
	bad := [][]string{
		{""},
		{"  "},
		{"justaword"},
		{"too/many/slashes"},
		{"/missing-owner"},
		{"missing-repo/"},
	}
	for _, repos := range bad {
		rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
			"name":         "P",
			"pinned_repos": repos,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repos=%v status = %d, want 400", repos, rec.Code)
		}
	}
}

func TestProjectGet_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects/no-such-id", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestProjectList_EmptyReturnsArray(t *testing.T) {
	// The handler must return `[]`, not `null` — a frontend that
	// .map()s the response would crash on null.
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

func TestProjectPatch_PartialFieldsLeaveOthersUnchanged(t *testing.T) {
	s := newTestServer(t)
	id, err := db.CreateProject(s.db, domain.Project{
		Name:        "Original",
		Description: "Original description",
		PinnedRepos: []string{"a/b"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"description": "Updated description",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Name != "Original" {
		t.Errorf("name changed unexpectedly: %q", got.Name)
	}
	if got.Description != "Updated description" {
		t.Errorf("description = %q", got.Description)
	}
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "a/b" {
		t.Errorf("pinned_repos changed unexpectedly: %v", got.PinnedRepos)
	}
}

// TestProjectPatch_PinnedReposExplicitEmptyClears confirms a client
// can clear pinned_repos by sending []. The pointer-typed *[]string
// distinguishes "absent (leave alone)" from "explicit empty (clear)";
// without that distinction the handler couldn't tell the cases apart.
func TestProjectPatch_PinnedReposExplicitEmptyClears(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "P", PinnedRepos: []string{"a/b"}})

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got, _ := db.GetProject(s.db, id)
	if len(got.PinnedRepos) != 0 {
		t.Errorf("pinned_repos should be empty, got %v", got.PinnedRepos)
	}
}

func TestProjectPatch_RejectsEmptyName(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "P"})
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{"name": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestProjectPatch_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/no-such-id", map[string]any{"name": "X"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestProjectDelete_Happy(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "doomed"})

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got, _ := db.GetProject(s.db, id)
	if got != nil {
		t.Errorf("project still readable after delete")
	}
}

func TestProjectDelete_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodDelete, "/api/projects/no-such-id", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestProjectDelete_RemovesKnowledgeDir verifies the handler clears
// the on-disk knowledge dir. Tested via a fake HOME so we don't
// touch the real ~/.triagefactory.
func TestProjectDelete_RemovesKnowledgeDir(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "with-files"})

	dir := filepath.Join(tempHome, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("notes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	stillThere := filepath.Join(tempHome, ".triagefactory", "projects", id)
	if _, err := os.Stat(stillThere); !os.IsNotExist(err) {
		t.Errorf("knowledge dir not removed: stat err = %v", err)
	}
}

// TestProjectDelete_MissingKnowledgeDir_NoError pins the
// "delete is best-effort on disk" contract: a project with no
// on-disk artifacts must still 204, not 500.
func TestProjectDelete_MissingKnowledgeDir_NoError(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "no-files"})
	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestValidatePinnedRepos_Slugs(t *testing.T) {
	good := [][]string{
		nil,
		{},
		{"a/b"},
		{"sky-ai-eng/triage-factory", "owner/repo"},
	}
	for _, repos := range good {
		if errMsg := validatePinnedRepos(repos); errMsg != "" {
			t.Errorf("repos=%v should pass, got %q", repos, errMsg)
		}
	}
	bad := [][]string{
		{""},
		{"  "},
		{"justaword"},
		{"a/b/c"},
		{"/x"},
		{"x/"},
	}
	for _, repos := range bad {
		if errMsg := validatePinnedRepos(repos); errMsg == "" {
			t.Errorf("repos=%v should reject", repos)
		}
	}
}
