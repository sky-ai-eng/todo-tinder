package projectbundle

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Preview returns the exact file list and aggregate size that Export would
// include for the given project. orgID scopes every store lookup so a
// multi-mode caller cannot read another tenant's project state.
func Preview(ctx context.Context, database *sql.DB, projects db.ProjectStore, curatorStore db.CuratorStore, orgID, projectID string) (*ExportPreview, error) {
	state, err := collectExportState(ctx, database, projects, curatorStore, orgID, projectID)
	if err != nil {
		return nil, err
	}
	out := &ExportPreview{
		Files: make([]ExportPreviewFile, 0, len(state.artifacts)),
	}
	for _, a := range state.artifacts {
		out.Files = append(out.Files, ExportPreviewFile{
			Path:      a.bundlePath,
			SizeBytes: a.size,
		})
		out.TotalSize += a.size
	}
	return out, nil
}
