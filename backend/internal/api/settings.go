// Package api exposes Dubarr's HTTP surface for application settings and the
// wizard's server-side directory browser.
//
// Handlers are registered with RegisterRoutes(mux, deps) per AGENTS.md; the
// package never touches cmd/dubarr/main.go. Admin authorization is injected as a
// middleware so this package has no import dependency on internal/auth.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

// Dependencies are the collaborators the settings API needs.
type Dependencies struct {
	// DB is required when Settings is nil; it backs the default store.
	DB *sql.DB
	// Settings is the settings store, normally shared with the rest of the app.
	Settings *settings.Store
	// Secrets is the encrypted secret store used for arr API keys. When nil the
	// arr connection routes answer 503 rather than storing keys unencrypted.
	Secrets *secrets.Service
	// RequireAdmin wraps admin-only handlers. When nil every route answers 503
	// rather than silently running unauthenticated.
	RequireAdmin func(http.Handler) http.Handler
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	store        *settings.Store
	secrets      *secrets.Service
	requireAdmin func(http.Handler) http.Handler
}

// RegisterRoutes mounts:
//
//	GET  /api/settings         read all non-secret sections
//	PUT  /api/settings         validate + persist a partial update
//	GET  /api/settings/export  sanitized configuration export
//	GET  /api/system/browse    canonicalized directory listing
//	GET  /api/settings/arr                     list arr connections (masked keys)
//	PUT  /api/settings/arr/{kind}              upsert one connection
//	DELETE /api/settings/arr/{kind}            remove one connection
//	POST /api/settings/arr/{kind}/test         ping the saved connection
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	store := deps.Settings
	if store == nil && deps.DB != nil {
		store = settings.New(deps.DB)
	}
	svc := &Service{store: store, secrets: deps.Secrets, requireAdmin: deps.RequireAdmin}

	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}

	mux.Handle("GET /api/settings", admin(svc.handleGetSettings))
	mux.Handle("PUT /api/settings", admin(svc.handlePutSettings))
	mux.Handle("GET /api/settings/export", admin(svc.handleExportSettings))
	mux.Handle("GET /api/system/browse", admin(svc.handleBrowse))
	mux.Handle("GET /api/settings/arr", admin(svc.handleListArr))
	mux.Handle("PUT /api/settings/arr/{kind}", admin(svc.handleUpsertArr))
	mux.Handle("DELETE /api/settings/arr/{kind}", admin(svc.handleDeleteArr))
	mux.Handle("POST /api/settings/arr/{kind}/test", admin(svc.handleTestArr))
	return svc
}

type settingsEnvelope struct {
	Settings map[string]json.RawMessage `json:"settings"`
}

func (s *Service) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store is not configured")
		return
	}
	all, err := s.store.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read settings")
		return
	}
	writeJSON(w, http.StatusOK, settingsEnvelope{Settings: all})
}

// handlePutSettings applies a partial update: only the sections present in the
// body are written, and each provided section replaces that section wholesale
// (GET /api/settings returns complete sections, so a read-modify-write round
// trip is lossless). Unknown keys, protected keys and type errors are 400 and
// nothing is written.
func (s *Service) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store is not configured")
		return
	}
	var body settingsEnvelope
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.Settings) == 0 {
		writeError(w, http.StatusBadRequest, "no settings provided")
		return
	}
	if err := s.store.Apply(r.Context(), body.Settings); err != nil {
		switch {
		case errors.Is(err, settings.ErrProtectedKey):
			writeError(w, http.StatusBadRequest, "setting is protected and cannot be changed through the API")
		case errors.Is(err, settings.ErrInvalidKey):
			writeError(w, http.StatusBadRequest, "setting key is invalid")
		case errors.Is(err, settings.ErrUnknownKey):
			writeError(w, http.StatusBadRequest, "unknown setting key")
		case errors.Is(err, settings.ErrInvalidValue):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "could not save settings")
		}
		return
	}
	all, err := s.store.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings saved but could not be re-read")
		return
	}
	writeJSON(w, http.StatusOK, settingsEnvelope{Settings: all})
}

func (s *Service) handleExportSettings(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store is not configured")
		return
	}
	exp, err := s.store.Export(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not export settings")
		return
	}
	writeJSON(w, http.StatusOK, exp)
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
