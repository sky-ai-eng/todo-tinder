package skills

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	// SQLite :memory: is per connection; pin to one connection so schema/data
	// are visible across calls.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })

	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir to %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prev)
	})
}

func writeSkillFile(t *testing.T, root, skillName, content string) string {
	t.Helper()
	path := filepath.Join(root, ".claude", "skills", skillName, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
	return path
}

func countVisibleImportedPrompts(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`
		SELECT COUNT(*)
		FROM prompts
		WHERE source = 'imported' AND hidden = 0
	`).Scan(&count); err != nil {
		t.Fatalf("count visible imported prompts: %v", err)
	}
	return count
}

func countHiddenImportedPrompts(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`
		SELECT COUNT(*)
		FROM prompts
		WHERE source = 'imported' AND hidden = 1
	`).Scan(&count); err != nil {
		t.Fatalf("count hidden imported prompts: %v", err)
	}
	return count
}

func TestImportAll_DedupesResolvedSearchDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, home)

	writeSkillFile(t, home, "review-pr", "Review pull requests carefully.")

	database := newTestDB(t)
	result := ImportAll(database)
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if result.Scanned != 1 {
		t.Fatalf("expected 1 scanned skill, got %d", result.Scanned)
	}
	if result.Imported != 1 {
		t.Fatalf("expected 1 imported skill, got %d", result.Imported)
	}
	if result.Skipped != 0 {
		t.Fatalf("expected 0 skipped skills, got %d", result.Skipped)
	}
	if got := countVisibleImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 visible imported prompt, got %d", got)
	}
}

func TestImportAll_DedupesByNameAndBodyAcrossLocations(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, project)

	content := "Help resolve merge conflicts and keep diffs small."
	writeSkillFile(t, home, "merge-helper", content)
	writeSkillFile(t, project, "merge-helper", content)

	database := newTestDB(t)
	result := ImportAll(database)
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if result.Scanned != 2 {
		t.Fatalf("expected 2 scanned skills, got %d", result.Scanned)
	}
	if result.Imported != 1 {
		t.Fatalf("expected 1 imported skill, got %d", result.Imported)
	}
	if result.Skipped != 1 {
		t.Fatalf("expected 1 skipped duplicate skill, got %d", result.Skipped)
	}
	if got := countVisibleImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 visible imported prompt, got %d", got)
	}
}

func TestImportAll_HidesExistingDuplicateImportedPrompts(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, project)

	database := newTestDB(t)
	body := "Triage and prioritize incoming work."
	if err := db.CreatePrompt(database, domain.Prompt{
		ID:     "imported-duplicate-a",
		Name:   "triage",
		Body:   body,
		Source: "imported",
	}); err != nil {
		t.Fatalf("create first duplicate prompt: %v", err)
	}
	if err := db.CreatePrompt(database, domain.Prompt{
		ID:     "imported-duplicate-b",
		Name:   "triage",
		Body:   body,
		Source: "imported",
	}); err != nil {
		t.Fatalf("create second duplicate prompt: %v", err)
	}

	result := ImportAll(database)
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if got := countVisibleImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 visible imported prompt after dedupe, got %d", got)
	}
	if got := countHiddenImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 hidden imported prompt after dedupe, got %d", got)
	}
}
