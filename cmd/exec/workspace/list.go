package workspace

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runList prints the JSON inventory of worktrees materialized for the
// current run. Diagnostic surface — the spawner's cleanup is the source
// of truth for the actual on-disk state. Useful for the agent to confirm
// what it has materialized so far before deciding whether to add another.
func runList(database *db.DB, args []string) {
	runID := os.Getenv("TRIAGE_FACTORY_RUN_ID")
	if runID == "" {
		exitErr("workspace list: TRIAGE_FACTORY_RUN_ID not set; this command must be invoked by the delegated agent")
	}

	rows, err := db.GetRunWorktrees(database.Conn, runID)
	if err != nil {
		exitErr("workspace list: " + err.Error())
	}

	type entry struct {
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Branch string `json:"branch"`
	}
	out := make([]entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, entry{Repo: r.RepoID, Path: r.Path, Branch: r.FeatureBranch})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "workspace list: encode: "+err.Error())
		os.Exit(1)
	}
}
