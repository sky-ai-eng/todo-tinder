package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// userEvent is the GitHub API response shape for a single user event.
type userEvent struct {
	Repo struct {
		Name string `json:"name"` // "owner/repo"
	} `json:"repo"`
	CreatedAt time.Time `json:"created_at"`
}

// ListUserEvents returns the unique "owner/repo" names where the user was
// active at or after the given cutoff time.
//
// NOTE: The GitHub Events API is capped at 300 events (10 pages of 30).
// Activity older than this window may be missed.
func (c *Client) ListUserEvents(username string, since time.Time) ([]string, error) {
	seen := make(map[string]bool)
	var repos []string

	for page := 1; page <= 10; page++ {
		path := fmt.Sprintf("/users/%s/events?per_page=30&page=%d", username, page)
		data, err := c.Get(path)
		if err != nil {
			return nil, fmt.Errorf("fetch events page %d: %w", page, err)
		}

		var events []userEvent
		if err := json.Unmarshal(data, &events); err != nil {
			return nil, fmt.Errorf("parse events page %d: %w", page, err)
		}

		if len(events) == 0 {
			break
		}

		// Events are returned newest-first. Once every event on a page
		// predates the cutoff, there's nothing newer left to page through.
		allOld := true
		for _, e := range events {
			if e.CreatedAt.Before(since) {
				continue
			}
			allOld = false
			if e.Repo.Name != "" && !seen[e.Repo.Name] {
				seen[e.Repo.Name] = true
				repos = append(repos, e.Repo.Name)
			}
		}
		if allOld {
			break
		}
	}

	return repos, nil
}

// repoMeta is the minimal GitHub repo API response we need.
type repoMeta struct {
	Description string `json:"description"`
}

// GetRepoDescription returns the description for owner/repo.
// Returns an empty string if the repo has no description set.
func (c *Client) GetRepoDescription(owner, repo string) (string, error) {
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s", owner, repo))
	if err != nil {
		return "", fmt.Errorf("get repo %s/%s: %w", owner, repo, err)
	}

	var r repoMeta
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("parse repo %s/%s: %w", owner, repo, err)
	}

	return r.Description, nil
}

// fileContent is the GitHub API response for a repository file.
type fileContent struct {
	Content  string `json:"content"`  // base64-encoded, newline-wrapped
	Encoding string `json:"encoding"` // always "base64" for text files
}

// GetFileContent fetches and decodes a file from a repo's default branch.
// Returns an empty string without error if the file does not exist (404).
func (c *Client) GetFileContent(owner, repo, path string) (string, error) {
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path))
	if err != nil {
		if strings.Contains(err.Error(), "returned 404") {
			return "", nil
		}
		return "", fmt.Errorf("get %s from %s/%s: %w", path, owner, repo, err)
	}

	var f fileContent
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("parse file content %s/%s/%s: %w", owner, repo, path, err)
	}

	if f.Encoding != "base64" {
		return "", fmt.Errorf("unexpected encoding %q for %s/%s/%s", f.Encoding, owner, repo, path)
	}

	// GitHub base64-encodes content with embedded newlines — strip them before decoding.
	clean := strings.ReplaceAll(f.Content, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", fmt.Errorf("decode %s/%s/%s: %w", owner, repo, path, err)
	}

	return string(decoded), nil
}
