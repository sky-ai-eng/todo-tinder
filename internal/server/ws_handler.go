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
// org/user, in multi mode they're the real session identity. A nil
// claims pointer (transient multi-mode boot state where authDeps
// hasn't landed yet) yields empty userID; the hub treats that as an
// unscoped client and delivers every event — matching pre-D9b
// behavior. Once SetAuthDeps lands, a re-handshake picks up real
// identity.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	var userID string
	if claims := ClaimsFrom(r.Context()); claims != nil {
		userID = claims.Subject
	}
	orgID := OrgIDFrom(r.Context())
	s.ws.HandleWS(w, r, userID, orgID)
}
