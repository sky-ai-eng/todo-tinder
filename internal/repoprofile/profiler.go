package repoprofile

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/ai"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
	"github.com/sky-ai-eng/todo-tinder/internal/github"
)

const (
	profileBatchSize    = 5
	repoPolitenessDelay = 100 * time.Millisecond
	profilingModel      = "haiku"
	maxDocChars         = 3000
)

// Profiler builds and persists AI-generated profiles for GitHub repositories
// based on a user's recent activity.
type Profiler struct {
	gh       *github.Client
	database *sql.DB
}

// NewProfiler creates a Profiler with the given GitHub client and DB handle.
func NewProfiler(gh *github.Client, database *sql.DB) *Profiler {
	return &Profiler{gh: gh, database: database}
}

// repoWithDocs groups a repo profile with the documentation text to send to the LLM.
type repoWithDocs struct {
	profile domain.RepoProfile
	docs    string
}

// Run profiles all repos the user was active in during the last activeDays days.
// It pages through the GitHub Events API (capped at 300 events / 10 pages),
// fetches per-repo metadata and docs, generates AI profiles for repos with
// documentation via Haiku, and upserts everything into repo_profiles.
func (p *Profiler) Run(ctx context.Context, githubUsername string, activeDays int) error {
	since := time.Now().Add(-time.Duration(activeDays) * 24 * time.Hour)

	repoNames, err := p.gh.ListUserEvents(githubUsername, since)
	if err != nil {
		return fmt.Errorf("list user events: %w", err)
	}

	log.Printf("[repoprofile] found %d active repos for %s in the last %d days", len(repoNames), githubUsername, activeDays)

	if len(repoNames) == 0 {
		return nil
	}

	var withDocs []repoWithDocs
	var withoutDocs []domain.RepoProfile

	for i, name := range repoNames {
		if err := ctx.Err(); err != nil {
			return err
		}

		parts := strings.SplitN(name, "/", 2)
		if len(parts) != 2 {
			log.Printf("[repoprofile] skipping malformed repo name %q", name)
			continue
		}
		owner, repo := parts[0], parts[1]

		desc, err := p.gh.GetRepoDescription(owner, repo)
		if err != nil {
			log.Printf("[repoprofile] %s: get description: %v", name, err)
		}

		readme, err := p.gh.GetFileContent(owner, repo, "README.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get README.md: %v", name, err)
		}

		claudeMd, err := p.gh.GetFileContent(owner, repo, "CLAUDE.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get CLAUDE.md: %v", name, err)
		}

		agentsMd, err := p.gh.GetFileContent(owner, repo, "AGENTS.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get AGENTS.md: %v", name, err)
		}

		prof := domain.RepoProfile{
			ID:          name,
			Owner:       owner,
			Repo:        repo,
			Description: desc,
			HasReadme:   readme != "",
			HasClaudeMd: claudeMd != "",
			HasAgentsMd: agentsMd != "",
		}

		docs := buildDocText(readme, claudeMd, agentsMd)
		if docs == "" {
			withoutDocs = append(withoutDocs, prof)
		} else {
			withDocs = append(withDocs, repoWithDocs{profile: prof, docs: docs})
		}

		// Politeness delay between repo fetches — skip after the last one.
		if i < len(repoNames)-1 {
			time.Sleep(repoPolitenessDelay)
		}
	}

	log.Printf("[repoprofile] %d repos with docs, %d without", len(withDocs), len(withoutDocs))

	// Upsert repos with no docs immediately — profile_text stays NULL.
	for _, prof := range withoutDocs {
		if err := db.UpsertRepoProfile(p.database, prof); err != nil {
			log.Printf("[repoprofile] upsert %s: %v", prof.ID, err)
		}
	}

	// Batch-profile repos that have docs through Haiku.
	profiled := 0
	for i := 0; i < len(withDocs); i += profileBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		end := i + profileBatchSize
		if end > len(withDocs) {
			end = len(withDocs)
		}
		batch := withDocs[i:end]

		results, err := profileBatch(batch)
		if err != nil {
			log.Printf("[repoprofile] batch %d failed: %v", i/profileBatchSize+1, err)
			// Fallback: upsert without profile_text so the row at least exists.
			for _, d := range batch {
				if uErr := db.UpsertRepoProfile(p.database, d.profile); uErr != nil {
					log.Printf("[repoprofile] upsert %s (fallback): %v", d.profile.ID, uErr)
				}
			}
			continue
		}

		byRepo := make(map[string]string, len(results))
		for _, r := range results {
			byRepo[r.Repo] = r.Profile
		}

		now := time.Now()
		for _, d := range batch {
			prof := d.profile
			if text := byRepo[prof.ID]; text != "" {
				prof.ProfileText = text
				prof.ProfiledAt = &now
			}
			if err := db.UpsertRepoProfile(p.database, prof); err != nil {
				log.Printf("[repoprofile] upsert %s: %v", prof.ID, err)
				continue
			}
			if prof.ProfileText != "" {
				profiled++
			}
		}
	}

	log.Printf("[repoprofile] done: %d profiled with AI, %d without docs", profiled, len(withoutDocs))
	return nil
}

// repoProfileInput is the per-repo JSON sent to the LLM.
type repoProfileInput struct {
	Repo        string `json:"repo"`
	Description string `json:"description,omitempty"`
	Docs        string `json:"docs"`
}

// repoProfileResult is one entry in the LLM's JSON array response.
type repoProfileResult struct {
	Repo    string `json:"repo"`
	Profile string `json:"profile"`
}

func profileBatch(batch []repoWithDocs) ([]repoProfileResult, error) {
	inputs := make([]repoProfileInput, len(batch))
	for i, d := range batch {
		inputs[i] = repoProfileInput{
			Repo:        d.profile.ID,
			Description: d.profile.Description,
			Docs:        d.docs,
		}
	}

	inputJSON, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("marshal batch: %w", err)
	}

	prompt := fmt.Sprintf(ai.RepoProfilePrompt, string(inputJSON))

	args := []string{
		"-p", prompt,
		"--model", profilingModel,
		"--output-format", "json",
	}

	cmd := exec.Command("claude", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude command failed: %w, stderr: %s", err, stderr.String())
	}

	// claude --output-format json wraps the response in {"result": "..."}
	var envelope struct {
		Result string `json:"result"`
	}
	raw := stdout.Bytes()
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Result != "" {
		raw = []byte(envelope.Result)
	}

	raw = stripCodeFences(raw)

	var results []repoProfileResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("parse response: %w, raw: %s", err, string(raw))
	}

	return results, nil
}

// buildDocText concatenates available documentation for a repo into a single
// block to send to the LLM. Returns empty string if no docs were found.
func buildDocText(readme, claudeMd, agentsMd string) string {
	var parts []string
	if readme != "" {
		parts = append(parts, "README.md:\n"+truncateStr(readme, maxDocChars))
	}
	if claudeMd != "" {
		parts = append(parts, "CLAUDE.md:\n"+truncateStr(claudeMd, maxDocChars))
	}
	if agentsMd != "" {
		parts = append(parts, "AGENTS.md:\n"+truncateStr(agentsMd, maxDocChars))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func stripCodeFences(b []byte) []byte {
	s := bytes.TrimSpace(b)
	if bytes.HasPrefix(s, []byte("```")) {
		if idx := bytes.Index(s[3:], []byte("\n")); idx >= 0 {
			s = s[3+idx+1:]
		}
		if idx := bytes.LastIndex(s, []byte("```")); idx >= 0 {
			s = s[:idx]
		}
	}
	return bytes.TrimSpace(s)
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
