// Package vaultapi exposes the Audio Vault: the read-only listing consumed by
// the /vault page, plus the operator-driven reorganisation (#47) that renames
// archived files inside the vault after a preview. It never serves vault file
// contents and never rewrites a file's bytes — a reorganisation changes paths
// only, and only when an operator applies a previewed plan.
package vaultapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// Dependencies are the collaborators the vault API needs.
type Dependencies struct {
	DB *sql.DB
	// RequireAdmin wraps every route. When nil the API answers 503 rather than
	// running unauthenticated.
	RequireAdmin func(http.Handler) http.Handler
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	store        *store.Store
	settings     *settings.Store
	requireAdmin func(http.Handler) http.Handler

	// plans caches previewed organise plans (10 minutes, in memory only) and
	// applyMu serialises applies: two operators cannot move files at once.
	plans   *planCache
	applyMu sync.Mutex
	// renameFile is os.Rename. Tests swap it to inject a mid-apply failure;
	// production never does.
	renameFile func(oldpath, newpath string) error
}

// RegisterRoutes mounts:
//
//	GET  /api/vault                     archived audio tracks, newest first, read-only
//	POST /api/vault/organise/preview    plan a rename; writes nothing
//	POST /api/vault/organise/apply      apply a previewed plan, all-or-nothing
//	GET  /api/vault/organise/runs       audit history
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	svc := &Service{
		store:        &store.Store{DB: deps.DB},
		settings:     settings.New(deps.DB),
		requireAdmin: deps.RequireAdmin,
		plans:        newPlanCache(),
	}
	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}
	mux.Handle("GET /api/vault", admin(svc.handleList))
	mux.Handle("POST /api/vault/organise/preview", admin(svc.handleOrganisePreview))
	mux.Handle("POST /api/vault/organise/apply", admin(svc.handleOrganiseApply))
	mux.Handle("GET /api/vault/organise/runs", admin(svc.handleOrganiseRuns))
	return svc
}

// VaultItem is one archived .mka card/row.
type VaultItem struct {
	ID                int64  `json:"id"`
	Title             string `json:"title"`
	MediaType         string `json:"media_type"`
	SeasonNumber      *int   `json:"season_number"`
	EpisodeNumber     *int   `json:"episode_number"`
	Filename          string `json:"filename"`
	FileSize          int64  `json:"file_size"`
	SHA256            string `json:"sha256"`
	Language          string `json:"language"`
	LanguageISO3      string `json:"language_iso3,omitempty"`
	Variant           string `json:"variant"`
	Codec             string `json:"codec"`
	Channels          string `json:"channels"`
	ChannelCount      int    `json:"channel_count"`
	SourceReleaseName string `json:"source_release_name"`
	CreatedAt         string `json:"created_at"`
}

type listResponse struct {
	Items []VaultItem `json:"items"`
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "vault store is not configured")
		return
	}
	rows, err := s.store.ListVaultTracks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the vault")
		return
	}
	items := make([]VaultItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, VaultItem{
			ID:                row.ID,
			Title:             row.Title,
			MediaType:         row.MediaType,
			SeasonNumber:      row.SeasonNumber,
			EpisodeNumber:     row.EpisodeNumber,
			Filename:          filepath.Base(row.FilePath),
			FileSize:          fileSize(row.FilePath),
			SHA256:            row.SHA256,
			Language:          row.LangISO2,
			LanguageISO3:      row.LangISO3,
			Variant:           row.Variant,
			Codec:             row.Codec,
			Channels:          row.Channels,
			ChannelCount:      row.ChannelCount,
			SourceReleaseName: row.SourceReleaseName,
			CreatedAt:         row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, listResponse{Items: items})
}

// fileSize reports the archived file size, or 0 when it cannot be stat'ed
// (the listing must still render).
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
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
