package vaultapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/store"
)

type harness struct {
	t   *testing.T
	mux *http.ServeMux
	db  *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		DB: st.DB,
		RequireAdmin: func(h http.Handler) http.Handler {
			return h
		},
	})
	return &harness{t: t, mux: mux, db: st}
}

func (h *harness) request(method, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// seedTrack archives one .mka on disk plus its vault_media + audio_tracks rows.
func (h *harness) seedTrack(t *testing.T, name, lang, codec, channels string, season, episode *int) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	filePath := filepath.Join(dir, name)
	if err := os.WriteFile(filePath, []byte("vault track payload"), 0o444); err != nil {
		t.Fatalf("write vault track: %v", err)
	}
	mediaID, err := h.db.EnsureVaultMedia(ctx, store.VaultMediaRecord{
		MediaType:  "series",
		Title:      "Shrek",
		Year:       2001,
		TVDBID:     42,
		FolderPath: dir,
	})
	if err != nil {
		t.Fatalf("EnsureVaultMedia: %v", err)
	}
	if _, err := h.db.RecordVaultTrack(ctx, store.VaultTrackRecord{
		VaultMediaID:      mediaID,
		SeasonNumber:      season,
		EpisodeNumber:     episode,
		GroupUUID:         "group-1",
		LangISO2:          lang,
		Variant:           "dubbing",
		FilePath:          filePath,
		SHA256:            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Codec:             codec,
		Channels:          channels,
		ChannelCount:      2,
		SourceReleaseName: "Shrek.2001.PL.DUB.mkv",
	}); err != nil {
		t.Fatalf("RecordVaultTrack: %v", err)
	}
}

type vaultResponse struct {
	Items []struct {
		ID                int64  `json:"id"`
		Title             string `json:"title"`
		MediaType         string `json:"media_type"`
		SeasonNumber      *int   `json:"season_number"`
		EpisodeNumber     *int   `json:"episode_number"`
		Filename          string `json:"filename"`
		FileSize          int64  `json:"file_size"`
		SHA256            string `json:"sha256"`
		Language          string `json:"language"`
		Variant           string `json:"variant"`
		Codec             string `json:"codec"`
		Channels          string `json:"channels"`
		ChannelCount      int    `json:"channel_count"`
		SourceReleaseName string `json:"source_release_name"`
		CreatedAt         string `json:"created_at"`
	} `json:"items"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) vaultResponse {
	t.Helper()
	var body vaultResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestListVaultEmpty(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodGet, "/api/vault")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/vault = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	if body.Items == nil {
		t.Fatalf("items = nil, want []")
	}
	if len(body.Items) != 0 {
		t.Errorf("items = %d, want 0", len(body.Items))
	}
}

func TestListVaultReturnsArchivedTracks(t *testing.T) {
	h := newHarness(t)
	season, episode := 1, 2
	h.seedTrack(t, "Shrek.S01E02.pl.mka", "pl", "eac3", "5.1", &season, &episode)

	rec := h.request(http.MethodGet, "/api/vault")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/vault = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}
	item := body.Items[0]
	if item.Title != "Shrek" || item.MediaType != "series" {
		t.Errorf("media = %q/%q", item.Title, item.MediaType)
	}
	if item.SeasonNumber == nil || *item.SeasonNumber != 1 || item.EpisodeNumber == nil || *item.EpisodeNumber != 2 {
		t.Errorf("season/episode = %v/%v", item.SeasonNumber, item.EpisodeNumber)
	}
	if item.Filename != "Shrek.S01E02.pl.mka" {
		t.Errorf("filename = %q", item.Filename)
	}
	if item.FileSize != int64(len("vault track payload")) {
		t.Errorf("file_size = %d, want %d", item.FileSize, len("vault track payload"))
	}
	if item.SHA256 == "" || item.Language != "pl" || item.Codec != "eac3" || item.Channels != "5.1" {
		t.Errorf("track metadata = %+v", item)
	}
	if item.ChannelCount != 2 || item.Variant != "dubbing" {
		t.Errorf("channel/variant = %d/%q", item.ChannelCount, item.Variant)
	}
	if item.SourceReleaseName != "Shrek.2001.PL.DUB.mkv" {
		t.Errorf("source_release_name = %q", item.SourceReleaseName)
	}
	if item.CreatedAt == "" {
		t.Errorf("created_at is empty")
	}
}

func TestListVaultNewestFirst(t *testing.T) {
	h := newHarness(t)
	h.seedTrack(t, "a.pl.mka", "pl", "eac3", "5.1", nil, nil)
	h.seedTrack(t, "b.en.mka", "en", "aac", "2.0", nil, nil)

	body := decode(t, h.request(http.MethodGet, "/api/vault"))
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[0].Filename != "b.en.mka" {
		t.Errorf("first item = %q, want newest b.en.mka", body.Items[0].Filename)
	}
}

func TestListVaultRequiresAdmin(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{DB: st.DB})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/vault", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/vault without auth = %d, want 503", rec.Code)
	}
}
