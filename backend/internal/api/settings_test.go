package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/auth"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

type harness struct {
	t       *testing.T
	db      *sql.DB
	dataDir string
	mux     *http.ServeMux
	server  *httptest.Server
	client  *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	settingsStore := settings.New(st.DB)
	if err := settingsStore.Seed(context.Background(), dir); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mux := http.NewServeMux()
	authSvc := auth.RegisterRoutes(mux, auth.Dependencies{DB: st.DB})
	RegisterRoutes(mux, Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		RequireAdmin: authSvc.RequireAdmin,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, db: st.DB, dataDir: dir, mux: mux, server: srv,
		client: &http.Client{Jar: jar}}
}

func (h *harness) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, h.server.URL+path, &buf)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(resp.Body); err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out.Bytes()
}

// setupAdmin creates the first-run admin account; the client keeps its cookie.
func (h *harness) setupAdmin() {
	h.t.Helper()
	code, body := h.do(http.MethodPost, "/api/auth/setup",
		map[string]string{"username": "admin", "password": "correct-horse-battery"})
	if code != http.StatusCreated {
		h.t.Fatalf("setup = %d: %s", code, body)
	}
}

type testSettingsEnvelope struct {
	Settings map[string]json.RawMessage `json:"settings"`
}

func (h *harness) getSettings() testSettingsEnvelope {
	h.t.Helper()
	code, body := h.do(http.MethodGet, "/api/settings", nil)
	if code != http.StatusOK {
		h.t.Fatalf("GET /api/settings = %d: %s", code, body)
	}
	var env testSettingsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		h.t.Fatalf("decode settings: %v (%s)", err, body)
	}
	return env
}

func TestSettingsRequiresAdmin(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/api/settings", "/api/settings/export", "/api/system/browse"} {
		code, body := h.do(http.MethodGet, path, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("GET %s without auth = %d: %s", path, code, body)
		}
	}
}

func TestNonAdminForbidden(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	hash, err := auth.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := h.db.Exec(`INSERT INTO users (username, password_hash, is_admin) VALUES ('viewer', ?, 0)`, hash); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{Jar: jar}
	code, body := h.do(http.MethodPost, "/api/auth/login",
		map[string]string{"username": "viewer", "password": "correct-horse-battery"})
	if code != http.StatusOK {
		t.Fatalf("login = %d: %s", code, body)
	}
	if code, body := h.do(http.MethodGet, "/api/settings", nil); code != http.StatusForbidden {
		t.Fatalf("non-admin GET /api/settings = %d: %s", code, body)
	}
}

func TestSettingsGetPutRoundtrip(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	env := h.getSettings()
	if len(env.Settings) == 0 {
		t.Fatal("GET /api/settings returned no sections")
	}
	if _, ok := env.Settings["auth.session_key"]; ok {
		t.Fatal("GET /api/settings leaked auth.session_key")
	}
	var tf settings.TargetTagFilter
	if err := json.Unmarshal(env.Settings[settings.KeyTargetTagFilter], &tf); err != nil {
		t.Fatalf("decode tag filter: %v", err)
	}
	if len(tf.RequiredFilenameTags) != 1 || tf.RequiredFilenameTags[0] != "[ArcA310]" {
		t.Fatalf("defaults not served: %+v", tf)
	}

	code, body := h.do(http.MethodPut, "/api/settings", map[string]any{
		"settings": map[string]any{
			settings.KeyTargetTagFilter: map[string]any{
				"required_filename_tags": []string{"[TDARR]"},
				"tag_match_mode":         "ALL",
				"tag_case_sensitive":     true,
				"excluded_filename_tags": []string{},
				"enforce_tag_filter":     false,
			},
			settings.KeySyncDefaults: map[string]any{
				"max_offset_sec":           240,
				"checkpoint_positions_pct": []int{10, 25, 40, 55, 70, 85},
				"checkpoint_window_sec":    150,
				"preview_positions_pct":    []int{10, 25, 50, 75, 90},
				"preview_window_sec":       60,
			},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("PUT /api/settings = %d: %s", code, body)
	}
	env = h.getSettings()
	if err := json.Unmarshal(env.Settings[settings.KeyTargetTagFilter], &tf); err != nil {
		t.Fatalf("decode tag filter: %v", err)
	}
	if tf.TagMatchMode != "ALL" || !tf.TagCaseSensitive || tf.EnforceTagFilter {
		t.Fatalf("PUT not persisted: %+v", tf)
	}

	// Values must survive a store re-open.
	h2 := settings.New(h.db)
	raw, ok, err := h2.Get(context.Background(), settings.KeySyncDefaults)
	if err != nil || !ok {
		t.Fatalf("Get after put: %v ok=%v", err, ok)
	}
	var sd settings.SyncDefaults
	if err := json.Unmarshal(raw, &sd); err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	if sd.MaxOffsetSec != 240 {
		t.Fatalf("max_offset_sec = %d, want 240", sd.MaxOffsetSec)
	}
}

func TestSettingsPutRejectsProtectedAndUnknown(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	for _, payload := range []any{
		map[string]any{"settings": map[string]any{"auth.session_key": "deadbeef"}},
		map[string]any{"settings": map[string]any{"secrets.master_key": "deadbeef"}},
		map[string]any{"settings": map[string]any{"totally.unknown": map[string]any{"a": 1}}},
		map[string]any{"settings": map[string]any{settings.KeyTargetTagFilter: map[string]any{"tag_match_mode": "NOPE"}}},
	} {
		code, body := h.do(http.MethodPut, "/api/settings", payload)
		if code != http.StatusBadRequest {
			t.Errorf("PUT %v = %d, want 400: %s", payload, code, body)
		}
	}
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'auth.session_key'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("auth.session_key rows = %d, want the untouched original", n)
	}
}

func TestExportSanitized(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	var sessionKey string
	if err := h.db.QueryRow(`SELECT value FROM settings WHERE key = 'auth.session_key'`).Scan(&sessionKey); err != nil {
		t.Fatalf("read session key: %v", err)
	}
	if _, err := h.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('arr.api_key', '"super-secret-api-key"')`); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	code, body := h.do(http.MethodGet, "/api/settings/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export = %d: %s", code, body)
	}
	text := string(body)
	for _, banned := range []string{"auth.session_key", "arr.api_key", sessionKey, "super-secret-api-key"} {
		if bytes.Contains([]byte(text), []byte(banned)) {
			t.Errorf("export leaked %q: %s", banned, text)
		}
	}
	var env testSettingsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode export: %v (%s)", err, body)
	}
	if _, ok := env.Settings[settings.KeyLanguagesPolicy]; !ok {
		t.Error("export dropped languages.policy")
	}
}

func TestBrowseListAndRejections(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	code, body := h.do(http.MethodPut, "/api/settings", map[string]any{
		"settings": map[string]any{settings.KeyStoragePaths: map[string]any{
			"watch_dir": root, "vault_dir": "", "work_dir": filepath.Join(h.dataDir, "work"),
			"media_roots": []string{root},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("configure storage = %d: %s", code, body)
	}

	code, body = h.do(http.MethodGet, "/api/system/browse?path="+root, nil)
	if code != http.StatusOK {
		t.Fatalf("browse valid = %d: %s", code, body)
	}
	var res struct {
		Path    string `json:"path"`
		Entries []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			IsDir bool   `json:"is_dir"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("decode browse: %v (%s)", err, body)
	}
	if res.Path != root {
		t.Errorf("path = %q, want %q", res.Path, root)
	}
	var sawDir, sawFile bool
	for _, e := range res.Entries {
		if e.Name == "movies" && e.IsDir {
			sawDir = true
		}
		if e.Name == "note.txt" && !e.IsDir {
			sawFile = true
		}
		if e.Name == "escape" {
			t.Errorf("symlink escape entry listed")
		}
	}
	if !sawDir || !sawFile {
		t.Errorf("missing entries: %+v", res.Entries)
	}

	cases := []struct {
		name string
		path string
		want int
	}{
		{"relative", "movies", http.StatusBadRequest},
		{"traversal", root + "/../" + filepath.Base(outside), http.StatusForbidden},
		{"parent-dir", filepath.Dir(root), http.StatusForbidden},
		{"symlink-escape", filepath.Join(root, "escape"), http.StatusForbidden},
		{"missing", filepath.Join(root, "nope"), http.StatusNotFound},
		{"file-not-dir", filepath.Join(root, "note.txt"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		code, body := h.do(http.MethodGet, "/api/system/browse?path="+tc.path, nil)
		if code != tc.want {
			t.Errorf("%s: browse %q = %d, want %d (%s)", tc.name, tc.path, code, tc.want, body)
		}
		if code != http.StatusOK && bytes.Contains(body, []byte(root)) {
			t.Errorf("%s: error response leaked server path: %s", tc.name, body)
		}
	}
}

func TestBrowseSetupModeAllowsAbsolutePaths(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	dir := t.TempDir()
	code, body := h.do(http.MethodGet, "/api/system/browse?path="+dir, nil)
	if code != http.StatusOK {
		t.Fatalf("setup-mode browse = %d: %s", code, body)
	}
	code, body = h.do(http.MethodGet, "/api/system/browse?path=relative/path", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("setup-mode relative browse = %d, want 400: %s", code, body)
	}
}
