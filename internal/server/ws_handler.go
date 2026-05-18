package server

import "net/http"

// handleWS wraps the hub's HandleWS so the websocket package stays
// free of any dependency on internal/server. Identity (userID, orgID)
// is pulled out of r.Context() before the upgrade and passed to the
// hub, which captures them on the per-connection struct for the
// Broadcast-side scoping filter.
//
// /api/ws is mounted via s.api(...), so withSession has already run
// by the time we get here: in local mode both values are the sentinel
// org/user, in multi mode they're the real session identity.
//
// Three identity shapes flow through here:
//
//   - Both empty (no claims, no orgID): pre-auth or test-harness path.
//     Falls through to the hub as an unscoped client that receives
//     every event — matches pre-D9b behavior.
//   - Both set: normal local-mode (sentinels) or normal multi-mode
//     (authenticated user with an active org). Scoped to that (user,
//     org) pair by the hub filter.
//   - userID set, orgID empty: authenticated multi-mode session whose
//     active_org_id is NULL (user has zero memberships, or hasn't
//     called POST /api/me/active-org yet). Rejected here with 409 +
//     no_active_org rather than registered as unscoped — the
//     unscoped carveout exists for the no-identity path, not for
//     authenticated callers with no org. Without this gate such a
//     client would receive every tenant's broadcasts.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	var userID string
	if claims := ClaimsFrom(r.Context()); claims != nil {
		userID = claims.Subject
	}
	orgID := OrgIDFrom(r.Context())
	if userID != "" && orgID == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "no_active_org",
			"message": "websocket handshake requires an active org; call POST /api/me/active-org to choose one",
		})
		return
	}
	s.ws.HandleWS(w, r, userID, orgID)
}
