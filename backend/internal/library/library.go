// Package library exposes the read-only Library screen API: the paged,
// filterable arr cache listing and the per-instance sync health panel (#29).
//
// Both routes are admin-only and read from arr_library_cache / arr_instances
// through the store's Library read surface; health additionally reads the
// arr.connections settings section to report configured-but-never-synced
// instances. Nothing here writes.
package library

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// Dependencies are the collaborators the library API needs.
type Dependencies struct {
	DB *sql.DB
	// Settings supplies the arr.connections section, so a connection saved but
	// not yet polled (no arr_instances row) still appears in health. When nil
	// the endpoint serves the store-derived instances only.
	Settings *settings.Store
	// RequireAdmin wraps every route. When nil the API answers 503 rather than
	// running unauthenticated (same contract as the queue API).
	RequireAdmin func(http.Handler) http.Handler
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	store        *store.Store
	settings     *settings.Store
	requireAdmin func(http.Handler) http.Handler
}

// RegisterRoutes mounts:
//
//	GET /api/library/items   paged, filtered arr_library_cache listing
//	GET /api/library/health  per-instance arr sync health + cache size
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	svc := &Service{
		store:        &store.Store{DB: deps.DB},
		settings:     deps.Settings,
		requireAdmin: deps.RequireAdmin,
	}
	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}
	mux.Handle("GET /api/library/items", admin(svc.handleItems))
	mux.Handle("GET /api/library/health", admin(svc.handleHealth))
	return svc
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
