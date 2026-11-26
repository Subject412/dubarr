// The Library refresh endpoint (#29): a manual "Refresh now" that shares the
// poller's single pass slot with the scheduled loop.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/arrsync"
)

// Refresher starts one library sync pass. *arrsync.Poller satisfies it; the
// interface keeps this API package free of the poller's wiring.
type Refresher interface {
	// Trigger starts a pass for one instance kind ("" = every enabled
	// connection) and returns without waiting for it.
	Trigger(ctx context.Context, kind string) error
}

// InstanceState is an optional Refresher extension: it reports whether the
// poller has any enabled arr instance to sync. *arrsync.Poller implements it,
// so a running poller with every instance disabled can answer with a message
// naming that configuration instead of repeating the fresh-install conflict. A
// Refresher that does not implement it is treated as configured.
type InstanceState interface {
	HasEnabledInstance(ctx context.Context) bool
}

// refreshRequest is the optional POST body: an empty body (or an empty kind)
// means every configured instance.
type refreshRequest struct {
	Kind string `json:"kind"`
}

// refreshResponse acknowledges an accepted pass. Kind is the requested kind or
// "all" when the pass covers every instance.
type refreshResponse struct {
	Accepted bool   `json:"accepted"`
	Kind     string `json:"kind"`
}

// errorBody carries the machine-readable reason alongside a human message, so
// the UI can tell a busy pass from a disabled feature without matching text.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// handleRefresh starts one manual sync pass.
//
//	202 {"accepted": true, "kind": "radarr"|"sonarr"|"all"}
//	400 malformed JSON or an unknown kind
//	409 code "busy"                 a pass is already running
//	409 code "not_configured"       no poller/instance is configured
//	409 code "stopped"              the poller is not running or is shutting down
func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) {
	// The body is validated first: a malformed request is the caller's fault
	// on every install, so an instance-less install answers 400, not 409.
	kind, ok := decodeRefreshKind(w, r)
	if !ok {
		return
	}

	// A fresh install has no poller at all; that is a conflict with the
	// request, not a server fault, and it must never panic here.
	refresher := s.refresherRef()
	if refresher == nil {
		s.refreshLogf("library: refresh refused (kind=%s): no arr instance is configured yet", kindLabel(kind))
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "no arr instance is configured yet",
			Code:  "not_configured",
		})
		return
	}

	// A poller can exist while every instance is disabled (the operator turned
	// them off after boot). That is "not running", not a fresh install: name
	// the missing configuration instead of repeating the message above.
	if state, ok := refresher.(InstanceState); ok && !state.HasEnabledInstance(r.Context()) {
		s.refreshLogf("library: refresh refused (kind=%s): every arr instance is disabled", kindLabel(kind))
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "no enabled arr instance: enable Radarr or Sonarr under Settings → Connections",
			Code:  "not_configured",
		})
		return
	}

	err := refresher.Trigger(r.Context(), kind)
	switch {
	case err == nil:
		// The pass itself logs its start/finish counts under component
		// "arrsync"; this line ties the click to the request that caused it.
		s.refreshLogf("library: refresh accepted (kind=%s): the sync pass runs in the background", kindLabel(kind))
		writeJSON(w, http.StatusAccepted, refreshResponse{
			Accepted: true,
			Kind:     kindLabel(kind),
		})
	case errors.Is(err, arrsync.ErrSyncBusy):
		s.refreshLogf("library: refresh skipped (kind=%s): a sync pass is already running", kindLabel(kind))
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "a sync pass is already running",
			Code:  "busy",
		})
	case errors.Is(err, arrsync.ErrPollerStopped):
		// Shutdown, or a poller that was never started: the request cannot
		// be served but it is not a server fault.
		s.refreshLogf("library: refresh refused (kind=%s): the sync poller is stopping", kindLabel(kind))
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "the sync poller is stopping",
			Code:  "stopped",
		})
	default:
		s.refreshLogf("library: refresh failed (kind=%s): %v", kindLabel(kind), err)
		writeError(w, http.StatusInternalServerError, "could not start a sync pass")
	}
}

// refreshLogf emits one refresh-decision line; a nil seam keeps the API silent.
func (s *Service) refreshLogf(format string, args ...any) {
	if s.logf == nil {
		return
	}
	s.logf(format, args...)
}

// decodeRefreshKind reads the optional body and validates the kind. It writes
// the 400 itself and reports false so the caller stops.
func decodeRefreshKind(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body refreshRequest
	// An empty body is valid: it means "all instances".
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return "", false
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	switch kind {
	case "", "radarr", "sonarr":
		return kind, true
	default:
		writeError(w, http.StatusBadRequest, "unknown arr kind")
		return "", false
	}
}

// kindLabel is the payload's kind value: the requested kind, or "all".
func kindLabel(kind string) string {
	if kind == "" {
		return "all"
	}
	return kind
}
