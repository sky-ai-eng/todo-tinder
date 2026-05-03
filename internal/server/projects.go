package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// SKY-215. Projects are the data layer underneath the Curator stack —
// the long-lived per-project context that the rest of the SKY-211
// family hangs work onto. This file is pure CRUD + on-disk knowledge
// dir cleanup; the Curator runtime, classifier, and UI all land in
// later tickets and can hit the same handlers without changes here.

// createProjectRequest is the POST body shape. Most fields are
// optional — a project starts as an empty shell named by the user
// and gets filled in over time (description, pinned repos, summary).
type createProjectRequest struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	PinnedRepos      []string `json:"pinned_repos"`
	JiraProjectKey   string   `json:"jira_project_key"`
	LinearProjectKey string   `json:"linear_project_key"`
	CuratorSessionID string   `json:"curator_session_id"` // optional; usually set by the runtime, not the user
}

// patchProjectRequest is the PATCH body shape. Pointers distinguish
// "absent → leave unchanged" from "explicit → overwrite". PinnedRepos
// uses *[]string so a client can clear it with [] without colliding
// with the absent case.
type patchProjectRequest struct {
	Name             *string   `json:"name"`
	Description      *string   `json:"description"`
	PinnedRepos      *[]string `json:"pinned_repos"`
	JiraProjectKey   *string   `json:"jira_project_key"`
	LinearProjectKey *string   `json:"linear_project_key"`
	SummaryMD        *string   `json:"summary_md"`
	SummaryStale     *bool     `json:"summary_stale"`
	CuratorSessionID *string   `json:"curator_session_id"`
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	pinned, errMsg := validatePinnedRepos(s.db, req.PinnedRepos)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}
	jiraKey, linearKey, errMsg := validateTrackerKeys(cfg, req.JiraProjectKey, req.LinearProjectKey)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}

	id, err := db.CreateProject(s.db, domain.Project{
		Name:             name,
		Description:      req.Description,
		PinnedRepos:      pinned,
		JiraProjectKey:   jiraKey,
		LinearProjectKey: linearKey,
		CuratorSessionID: req.CuratorSessionID,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	created, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "created but read-back failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleProjectList(w http.ResponseWriter, _ *http.Request) {
	projects, err := db.ListProjects(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req patchProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	existing, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	updated := *existing
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
			return
		}
		updated.Name = trimmed
	}
	if req.Description != nil {
		updated.Description = *req.Description
	}
	if req.PinnedRepos != nil {
		pinned, errMsg := validatePinnedRepos(s.db, *req.PinnedRepos)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
		updated.PinnedRepos = pinned
	}
	if req.JiraProjectKey != nil || req.LinearProjectKey != nil {
		// Validate only the fields the client sent. Re-validating the
		// untouched side against the current config would surface a
		// confusing error if the config drifted (e.g. a Jira project
		// got renamed in Settings) on an unrelated PATCH that's only
		// touching, say, the Linear key. The handler's contract is
		// "validate what the client asked to change," not "re-validate
		// the whole row on every PATCH."
		cfg, err := config.Load()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
			return
		}
		if req.JiraProjectKey != nil {
			jiraKey, _, errMsg := validateTrackerKeys(cfg, *req.JiraProjectKey, "")
			if errMsg != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
				return
			}
			updated.JiraProjectKey = jiraKey
		}
		if req.LinearProjectKey != nil {
			_, linearKey, errMsg := validateTrackerKeys(cfg, "", *req.LinearProjectKey)
			if errMsg != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
				return
			}
			updated.LinearProjectKey = linearKey
		}
	}
	if req.SummaryMD != nil {
		updated.SummaryMD = *req.SummaryMD
	}
	if req.SummaryStale != nil {
		updated.SummaryStale = *req.SummaryStale
	}
	if req.CuratorSessionID != nil {
		updated.CuratorSessionID = *req.CuratorSessionID
	}

	if err := db.UpdateProject(s.db, updated); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	fresh, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "updated but read-back failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Snapshot pinned_repos BEFORE the cascade fires so we can prune
	// each affected bare clone's worktree registration list after the
	// project's repos/ subtree gets RemoveAll'd. Without the prune,
	// stale entries accumulate in <bare>/worktrees/ — recoverable but
	// noisy, and they block re-creating the same name in a future
	// project's worktree. A read failure here is non-fatal: skip the
	// prune step, the on-disk cleanup still happens.
	var pinned []string
	if existing, err := db.GetProject(s.db, id); err == nil && existing != nil {
		pinned = existing.PinnedRepos
	}

	// Stop any in-flight Curator chat for this project BEFORE the DB
	// delete: the goroutine writes terminal cancelled status into
	// curator_requests rows, which the FK cascade is about to drop.
	// Doing it in the right order means a user who deletes a project
	// mid-chat sees a deterministic terminal state on every row
	// rather than relying on cascade behavior to handle in-flight
	// rows. No-op when the curator runtime hasn't been wired (test
	// harnesses, fresh-install before first message).
	if s.curator != nil {
		s.curator.CancelProject(id)
	}

	if err := db.DeleteProject(s.db, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Best-effort on-disk cleanup. The DB delete is the source of
	// truth and a stale on-disk dir is recoverable (next run that
	// needs that path will recreate or surface the issue), so a
	// cleanup failure surfaces as a non-fatal warning rather than
	// a 5xx.
	//
	// Two failure modes both produce the same X-Cleanup-Warning so
	// the client always learns that on-disk state may be stale:
	//   - resolving the path failed (UserHomeDir error etc.) —
	//     cleanup couldn't even be attempted
	//   - resolving worked but RemoveAll failed
	//
	// The full error (with absolute path + OS-specific detail) is
	// logged server-side; the header is a generic message so we
	// don't leak filesystem layout to the client.
	const cleanupWarning = "on-disk cleanup of project knowledge dir failed; check server logs"
	dir, dirErr := curator.KnowledgeDir(id)
	switch {
	case dirErr != nil:
		log.Printf("[projects] cannot resolve knowledge dir for project %s; on-disk cleanup skipped: %v", id, dirErr)
		w.Header().Set("X-Cleanup-Warning", cleanupWarning)
	default:
		if rmErr := os.RemoveAll(dir); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("[projects] cleanup of project %s knowledge dir %q failed: %v", id, dir, rmErr)
			w.Header().Set("X-Cleanup-Warning", cleanupWarning)
		}
	}

	// Prune the bare clone of each pinned repo — the per-project
	// worktrees we just RemoveAll'd would otherwise leave behind
	// dangling entries in <bare>/worktrees/ that block re-creating
	// the same name in a future project. Best-effort, post-RemoveAll
	// because prune is what reads the now-missing dirs.
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			continue
		}
		worktree.PruneCuratorBare(owner, repo)
	}

	w.WriteHeader(http.StatusNoContent)
}

// splitOwnerRepo splits "owner/repo" once. Mirrors the helper in
// internal/curator; duplicated here rather than imported to avoid
// pulling the curator package's surface into the projects handler.
func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	for i := 0; i < len(slug); i++ {
		if slug[i] == '/' {
			if i == 0 || i == len(slug)-1 {
				return "", "", false
			}
			return slug[:i], slug[i+1:], true
		}
	}
	return "", "", false
}

// validatePinnedRepoShape checks the "owner/repo" slug format and
// returns the normalized (trimmed) slice. Pure — does not touch the
// DB — so it stays cheap to test in isolation and stays usable in
// any future code path that just needs to canonicalize slug input.
//
// The trim-then-persist step matters: without it, " owner/repo "
// would pass validation (the validator trims internally) but get
// stored padded, breaking subsequent lookups by slug.
//
// Returns (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepoShape(repos []string) ([]string, string) {
	out := make([]string, len(repos))
	for i, r := range repos {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			return nil, "pinned_repos[" + strconv.Itoa(i) + "] is empty"
		}
		// Require exactly one '/' with non-empty owner and repo.
		// Anything else (no slash, leading/trailing slash, multiple
		// slashes) is rejected — the slug shape is "owner/repo".
		parts := strings.Split(trimmed, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, "pinned_repos[" + strconv.Itoa(i) + "] must be 'owner/repo'"
		}
		out[i] = trimmed
	}
	return out, ""
}

// validatePinnedRepos composes shape validation with the must-be-
// configured existence check: every slug must correspond to a row in
// repo_profiles. This pins the UX contract — the frontend (SKY-217)
// presents pinned_repos as a multi-select over the configured-repos
// list, so an unconfigured slug arriving here is either a stale
// client (the user removed the repo from config after pinning) or
// someone hand-crafting a curl. Rejecting it up front keeps the
// Curator from later trying to materialize a worktree for a repo
// the user can't authenticate against.
//
// Returns (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepos(database *sql.DB, repos []string) ([]string, string) {
	out, errMsg := validatePinnedRepoShape(repos)
	if errMsg != "" {
		return nil, errMsg
	}
	if len(out) == 0 {
		return out, ""
	}

	configured, err := db.GetConfiguredRepoNames(database)
	if err != nil {
		return nil, "failed to load configured repos: " + err.Error()
	}
	known := make(map[string]struct{}, len(configured))
	for _, name := range configured {
		known[name] = struct{}{}
	}
	for _, slug := range out {
		if _, ok := known[slug]; !ok {
			return nil, "pinned_repos: " + slug + " is not a configured repo (add it on the GitHub config page first)"
		}
	}
	return out, ""
}

// validateTrackerKeys validates jira_project_key and linear_project_key
// independently. Each is optional; when non-empty, jira_project_key
// must be present in cfg.Jira.Projects (the user-curated list set up
// in Settings) and linear_project_key is rejected outright until the
// Linear integration ships. Both fields are normalized via
// strings.TrimSpace before the existence check so a value padded with
// stray whitespace doesn't pass validation but get stored unmatched.
//
// Takes cfg as a parameter rather than calling config.Load() directly
// so the function is testable in isolation and so a single PATCH/POST
// only reads the config file once even when both fields need
// validating.
//
// Returns the normalized values and an empty error string on success,
// or two empty strings and an error message on failure.
func validateTrackerKeys(cfg config.Config, jiraKey, linearKey string) (string, string, string) {
	jiraNorm := strings.TrimSpace(jiraKey)
	linearNorm := strings.TrimSpace(linearKey)

	if linearNorm != "" {
		// Linear integration is future work. Surfacing this as an
		// explicit "not configured" error matches the UX contract:
		// the frontend renders a disabled Linear picker, so a
		// non-empty value arriving here is a bypass attempt or a
		// stale client.
		return "", "", "linear_project_key: Linear integration is not configured"
	}

	if jiraNorm == "" {
		return "", "", ""
	}
	for _, p := range cfg.Jira.Projects {
		if p == jiraNorm {
			return jiraNorm, "", ""
		}
	}
	return "", "", "jira_project_key: " + jiraNorm + " is not in the configured Jira projects list (add it on the Settings page first)"
}

// knowledgeFile is the per-file shape returned by the knowledge
// endpoint. content is the raw markdown; the frontend renders it.
// We surface the relative path (under <KnowledgeDir>/knowledge-base/)
// rather than the absolute path so the API doesn't leak the user's
// home directory layout.
type knowledgeFile struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
	SizeBytes int64  `json:"size_bytes"`
}

// handleProjectKnowledge serves the curated markdown files that live
// under the project's knowledge dir. SKY-217.
//
// The directory layout is `<KnowledgeDir>/knowledge-base/*.md` — the
// outer directory is also where the Curator's CC subprocess runs from
// and where pinned-repo worktrees materialize, so we deliberately
// don't read the outer dir; it's full of agent scratch state, not
// user-visible knowledge.
//
// Returns an empty list (not 404) when the project exists but the
// knowledge subdir doesn't, so the frontend can render an empty state
// instead of a noisy error. A real I/O failure (permission denied,
// etc.) is a 500.
func (s *Server) handleProjectKnowledge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	root, err := curator.KnowledgeDir(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "resolve knowledge dir: " + err.Error()})
		return
	}
	kbDir := filepath.Join(root, "knowledge-base")
	files, err := readKnowledgeFiles(kbDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// readKnowledgeFiles walks one level of the knowledge-base directory
// and returns the markdown files in stable order. Single level by
// design — the agent is supposed to keep a flat layout under
// knowledge-base/, and recursing here would surface scratch state
// from any nested dirs the agent created.
//
// "Doesn't exist" is not an error: a fresh project hasn't had any
// knowledge written yet, so an empty list is the truthful response.
func readKnowledgeFiles(dir string) ([]knowledgeFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []knowledgeFile{}, nil
		}
		return nil, err
	}
	out := make([]knowledgeFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		body, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		out = append(out, knowledgeFile{
			Path:      name,
			Content:   string(body),
			UpdatedAt: info.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
			SizeBytes: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
