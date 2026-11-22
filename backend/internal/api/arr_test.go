package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
	"github.com/Subject412/dubarr/backend/internal/auth"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/settings"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// newArrHarness wires the settings API with an encrypted secret service backed
// by a fixed test key, so no OS keyring is involved.
func newArrHarness(t *testing.T) (*harness, *secrets.Service) {
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
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sec, err := secrets.NewWithKey(st.DB, key, nil, secrets.SourceKeyfile)
	if err != nil {
		t.Fatalf("secrets.NewWithKey: %v", err)
	}

	mux := http.NewServeMux()
	authSvc := auth.RegisterRoutes(mux, auth.Dependencies{DB: st.DB})
	RegisterRoutes(mux, Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		Secrets:      sec,
		RequireAdmin: authSvc.RequireAdmin,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, db: st.DB, dataDir: dir, mux: mux, server: srv,
		client: &http.Client{Jar: jar}}, sec
}

type arrListResponse struct {
	Instances []struct {
		Kind    string `json:"kind"`
		BaseURL string `json:"base_url"`
		Enabled bool   `json:"enabled"`
		APIKey  string `json:"api_key"`
	} `json:"instances"`
}

func (h *harness) listArr() arrListResponse {
	h.t.Helper()
	code, body := h.do(http.MethodGet, "/api/settings/arr", nil)
	if code != http.StatusOK {
		h.t.Fatalf("GET /api/settings/arr = %d: %s", code, body)
	}
	var out arrListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		h.t.Fatalf("decode arr list: %v (%s)", err, body)
	}
	return out
}

func TestArrListEmpty(t *testing.T) {
	h, _ := newArrHarness(t)
	h.setupAdmin()
	code, body := h.do(http.MethodGet, "/api/settings/arr", nil)
	if code != http.StatusOK {
		t.Fatalf("GET = %d: %s", code, body)
	}
	if !bytes.Contains(body, []byte(`"instances":[]`)) {
		t.Fatalf("empty list must be [], got %s", body)
	}
}

func TestArrUpsertStoresKeyEncryptedAndListsMasked(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	const apiKey = "0123456789abcdef-RADARR-KEY-abcd"

	code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": "http://radarr.example:7878",
		"api_key":  apiKey,
		"enabled":  true,
	})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	if bytes.Contains(body, []byte(apiKey)) {
		t.Fatalf("upsert response leaked the API key: %s", body)
	}
	var view struct {
		Kind    string `json:"kind"`
		BaseURL string `json:"base_url"`
		Enabled bool   `json:"enabled"`
		APIKey  string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode upsert: %v (%s)", err, body)
	}
	if view.Kind != "radarr" || view.BaseURL != "http://radarr.example:7878" || !view.Enabled {
		t.Fatalf("upsert response = %+v", view)
	}
	if want := secrets.Mask(apiKey); view.APIKey != want {
		t.Fatalf("masked key = %q, want %q", view.APIKey, want)
	}

	list := h.listArr()
	if len(list.Instances) != 1 {
		t.Fatalf("instances = %+v", list.Instances)
	}
	if got := list.Instances[0].APIKey; got != secrets.Mask(apiKey) {
		t.Fatalf("list key = %q, want masked suffix", got)
	}

	// The key must live only in the encrypted secrets table.
	stored, err := sec.Get("arr.radarr.api_key")
	if err != nil || stored != apiKey {
		t.Fatalf("secret round-trip = %q, %v", stored, err)
	}
	var valueEnc []byte
	if err := h.db.QueryRow(`SELECT value_enc FROM secrets WHERE key = 'arr.radarr.api_key'`).Scan(&valueEnc); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(valueEnc, []byte(apiKey)) {
		t.Fatal("secrets row contains the plaintext API key")
	}
	var section string
	if err := h.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settings.KeyArrConnections).Scan(&section); err != nil {
		t.Fatalf("read section: %v", err)
	}
	if strings.Contains(section, apiKey) || strings.Contains(section, "abcd") {
		t.Fatalf("settings section carries key material: %s", section)
	}

	// The generic settings read must not expose the key either.
	_, settingsBody := h.do(http.MethodGet, "/api/settings", nil)
	if bytes.Contains(settingsBody, []byte(apiKey)) {
		t.Fatalf("GET /api/settings leaked the API key: %s", settingsBody)
	}
}

func TestArrUpsertRequiresKeyOnCreateAndBaseURL(t *testing.T) {
	h, _ := newArrHarness(t)
	h.setupAdmin()
	for _, tc := range []struct {
		kind string
		body map[string]any
	}{
		{"sonarr", map[string]any{"base_url": "http://sonarr:8989"}},
		{"sonarr", map[string]any{"base_url": "not-a-url", "api_key": "abcdef-1234"}},
		{"kodi", map[string]any{"base_url": "http://kodi:8080", "api_key": "abcdef-1234"}},
	} {
		code, body := h.do(http.MethodPut, "/api/settings/arr/"+tc.kind, tc.body)
		if code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Errorf("PUT %s %v = %d, want 400/404: %s", tc.kind, tc.body, code, body)
		}
	}
	if res := h.listArr(); len(res.Instances) != 0 {
		t.Fatalf("rejected writes persisted: %+v", res.Instances)
	}
}

func TestArrUpdateWithoutKeyKeepsExistingKey(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	if code, body := h.do(http.MethodPut, "/api/settings/arr/sonarr", map[string]any{
		"base_url": "http://sonarr:8989", "api_key": "abcdef-1234", "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	code, body := h.do(http.MethodPut, "/api/settings/arr/sonarr", map[string]any{
		"base_url": "http://sonarr-new:8989", "enabled": false,
	})
	if code != http.StatusOK {
		t.Fatalf("update = %d: %s", code, body)
	}
	list := h.listArr()
	if len(list.Instances) != 1 || list.Instances[0].BaseURL != "http://sonarr-new:8989" || list.Instances[0].Enabled {
		t.Fatalf("updated instance = %+v", list.Instances)
	}
	if key, err := sec.Get("arr.sonarr.api_key"); err != nil || key != "abcdef-1234" {
		t.Fatalf("key after update = %q, %v", key, err)
	}
}

func TestArrDeleteRemovesConnectionAndSecret(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	if code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": "http://radarr:7878", "api_key": "abcdef-1234", "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	if code, body := h.do(http.MethodDelete, "/api/settings/arr/radarr", nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", code, body)
	}
	if res := h.listArr(); len(res.Instances) != 0 {
		t.Fatalf("instances after delete = %+v", res.Instances)
	}
	if _, err := sec.Get("arr.radarr.api_key"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("secret survived delete: %v", err)
	}
}

func TestArrConnectionTestPingsInstance(t *testing.T) {
	h, _ := newArrHarness(t)
	h.setupAdmin()
	const mockKey = "mock-radarr-api-key"
	fake := arrmock.New(arrmock.Radarr, mockKey, arrmock.Fixtures{})
	t.Cleanup(fake.Close)

	if code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": fake.URL, "api_key": mockKey, "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	code, body := h.do(http.MethodPost, "/api/settings/arr/radarr/test", nil)
	if code != http.StatusOK {
		t.Fatalf("test = %d: %s", code, body)
	}
	if !bytes.Contains(body, []byte(`"ok":true`)) {
		t.Fatalf("test body = %s", body)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/api/v3/system/status" || reqs[0].APIKey != mockKey {
		t.Fatalf("ping requests = %+v", reqs)
	}
}

func TestArrConnectionTestRejectsBadKeyWithoutLeaking(t *testing.T) {
	h, _ := newArrHarness(t)
	h.setupAdmin()
	fake := arrmock.New(arrmock.Radarr, "right-key", arrmock.Fixtures{})
	t.Cleanup(fake.Close)
	const wrongKey = "wrong-key-do-not-leak"
	if code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": fake.URL, "api_key": wrongKey,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	code, body := h.do(http.MethodPost, "/api/settings/arr/radarr/test", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("test with bad key = %d, want 502: %s", code, body)
	}
	if bytes.Contains(body, []byte(wrongKey)) {
		t.Fatalf("test error leaked the API key: %s", body)
	}

	// Testing an unconfigured kind is a client error, not a crash.
	if code, body := h.do(http.MethodPost, "/api/settings/arr/sonarr/test", nil); code != http.StatusNotFound && code != http.StatusBadRequest {
		t.Fatalf("test unconfigured = %d: %s", code, body)
	}
}

func TestArrConnectionsSectionRejectsKeyField(t *testing.T) {
	h, _ := newArrHarness(t)
	h.setupAdmin()
	code, body := h.do(http.MethodPut, "/api/settings", map[string]any{
		"settings": map[string]any{settings.KeyArrConnections: map[string]any{
			"instances": []map[string]any{{
				"kind": "radarr", "base_url": "http://radarr:7878", "enabled": true,
				"api_key": "must-not-be-stored-here",
			}},
		}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("generic settings PUT accepted an api_key field = %d: %s", code, body)
	}
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE value LIKE '%must-not-be-stored-here%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("api_key reached the settings table (%d rows)", n)
	}
}

func TestArrRoutesRequireAdmin(t *testing.T) {
	h, _ := newArrHarness(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/settings/arr"},
		{http.MethodPut, "/api/settings/arr/radarr"},
		{http.MethodDelete, "/api/settings/arr/radarr"},
		{http.MethodPost, "/api/settings/arr/radarr/test"},
	} {
		code, body := h.do(tc.method, tc.path, nil)
		if code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("%s %s without auth = %d: %s", tc.method, tc.path, code, body)
		}
	}
}

func TestArrRoutesUnavailableWithoutSecretService(t *testing.T) {
	h := newHarness(t) // no Secrets dependency wired
	h.setupAdmin()
	code, body := h.do(http.MethodGet, "/api/settings/arr", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET without secrets = %d, want 503: %s", code, body)
	}
}
