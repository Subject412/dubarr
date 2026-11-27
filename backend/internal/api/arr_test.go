package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
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
	h, _, sec := newArrHarnessWithSeam(t)
	return h, sec
}

// newArrHarnessWithSeam also returns the api Service so a test can replace the
// secrets seam (handlers read it per request, after registration).
func newArrHarnessWithSeam(t *testing.T) (*harness, *Service, *secrets.Service) {
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
	svc := RegisterRoutes(mux, Dependencies{
		DB:           st.DB,
		Settings:     settingsStore,
		Secrets:      sec,
		RequireAdmin: authSvc.RequireAdmin,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, db: st.DB, dataDir: dir, mux: mux, server: srv,
		client: &http.Client{Jar: jar}}, svc, sec
}

// fakeArrSecrets wraps the real encrypted store so tests can fail Delete or run
// a hook once a key has been written, making the ordering guarantees observable.
type fakeArrSecrets struct {
	inner     *secrets.Service
	mu        sync.Mutex
	deletes   []string
	deleteErr error
	afterSet  func()
	inSet     func()
}

func (f *fakeArrSecrets) Set(key, plaintext string) error {
	if err := f.inner.Set(key, plaintext); err != nil {
		return err
	}
	if f.inSet != nil {
		f.inSet()
	}
	if f.afterSet != nil {
		f.afterSet()
	}
	return nil
}

func (f *fakeArrSecrets) Get(key string) (string, error) { return f.inner.Get(key) }

func (f *fakeArrSecrets) Delete(key string) error {
	f.mu.Lock()
	f.deletes = append(f.deletes, key)
	f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.inner.Delete(key)
}

func (f *fakeArrSecrets) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
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

	// Testing an unknown/unconfigured kind with no body is a 404: the handler
	// looks the kind up in the saved connections and finds nothing.
	if code, body := h.do(http.MethodPost, "/api/settings/arr/sonarr/test", nil); code != http.StatusNotFound {
		t.Fatalf("test unconfigured = %d, want 404: %s", code, body)
	}

	// A draft base URL is enough to test a kind with no saved instance: the
	// request pings the draft, so it is neither a 404 nor a 400.
	draft := arrmock.New(arrmock.Sonarr, "draft-sonarr-key", arrmock.Fixtures{})
	t.Cleanup(draft.Close)
	if code, body := h.do(http.MethodPost, "/api/settings/arr/sonarr/test", map[string]any{
		"base_url": draft.URL, "api_key": "draft-sonarr-key",
	}); code != http.StatusOK {
		t.Fatalf("test with draft url and no saved instance = %d, want 200: %s", code, body)
	}
	if reqs := draft.Requests(); len(reqs) != 1 || reqs[0].APIKey != "draft-sonarr-key" {
		t.Fatalf("draft pings = %+v", reqs)
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

// TestArrDeleteRemovesSecretBeforeMetadata pins the fix: if the secret delete
// fails, the metadata must still be there so the operator can retry (the key is
// not orphaned, it is still reachable through the connection row).
func TestArrDeleteRemovesSecretBeforeMetadata(t *testing.T) {
	h, svc, real := newArrHarnessWithSeam(t)
	h.setupAdmin()
	if code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": "http://radarr:7878", "api_key": "abcdef-1234", "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}

	fake := &fakeArrSecrets{inner: real, deleteErr: errors.New("secret store unavailable")}
	svc.secrets = fake

	code, body := h.do(http.MethodDelete, "/api/settings/arr/radarr", nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("delete with failing secret store = %d, want 500: %s", code, body)
	}
	if got := fake.deletedKeys(); len(got) != 1 || got[0] != "arr.radarr.api_key" {
		t.Fatalf("secret delete attempts = %v, want the delete to be tried first", got)
	}
	if res := h.listArr(); len(res.Instances) != 1 {
		t.Fatalf("metadata was dropped before the secret: %+v", res.Instances)
	}
	if key, err := real.Get("arr.radarr.api_key"); err != nil || key != "abcdef-1234" {
		t.Fatalf("secret after failed delete = %q, %v", key, err)
	}
}

// TestArrDeleteToleratesAlreadyMissingSecret covers the retry path: the previous
// attempt removed the key but not the metadata, so the retry must succeed.
func TestArrDeleteToleratesAlreadyMissingSecret(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	if code, body := h.do(http.MethodPut, "/api/settings/arr/sonarr", map[string]any{
		"base_url": "http://sonarr:8989", "api_key": "abcdef-1234", "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	if err := sec.Delete("arr.sonarr.api_key"); err != nil {
		t.Fatalf("pre-delete secret: %v", err)
	}
	if code, body := h.do(http.MethodDelete, "/api/settings/arr/sonarr", nil); code != http.StatusNoContent {
		t.Fatalf("retry delete = %d: %s", code, body)
	}
	if res := h.listArr(); len(res.Instances) != 0 {
		t.Fatalf("instances after retry delete = %+v", res.Instances)
	}
}

// TestArrUpsertCompensatesSecretWhenSaveFails makes the section save fail after
// the key was written (closing the DB) and asserts the handler deletes that key
// instead of leaving an orphan.
func TestArrUpsertCompensatesSecretWhenSaveFails(t *testing.T) {
	h, svc, real := newArrHarnessWithSeam(t)
	h.setupAdmin()

	fake := &fakeArrSecrets{inner: real, afterSet: func() { _ = h.db.Close() }}
	svc.secrets = fake

	body := strings.NewReader(`{"base_url":"http://radarr:7878","api_key":"abcdef-1234","enabled":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/settings/arr/radarr", body)
	req.SetPathValue("kind", "radarr")
	rec := httptest.NewRecorder()
	svc.handleUpsertArr(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("upsert with failing save = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if got := fake.deletedKeys(); len(got) != 1 || got[0] != "arr.radarr.api_key" {
		t.Fatalf("compensating deletes = %v, want the freshly written secret removed", got)
	}
}

// TestArrConcurrentUpsertsKeepBothConnections would lose one instance without
// the section mutex: both writers load the same empty slice and the last save
// wins. The inSet hook makes the overlap deterministic — the first writer to
// reach the secret write waits briefly for a second one, so without the lock
// both have already read the stale section when they save.
func TestArrConcurrentUpsertsKeepBothConnections(t *testing.T) {
	h, svc, real := newArrHarnessWithSeam(t)
	h.setupAdmin()

	var inSet int32
	fake := &fakeArrSecrets{inner: real}
	fake.inSet = func() {
		if atomic.AddInt32(&inSet, 1) == 1 {
			deadline := time.Now().Add(250 * time.Millisecond)
			for atomic.LoadInt32(&inSet) < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		}
	}
	svc.secrets = fake

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 4; i++ {
		for _, kind := range []string{"radarr", "sonarr"} {
			wg.Add(1)
			go func(kind string, i int) {
				defer wg.Done()
				payload := fmt.Sprintf(`{"base_url":"http://%s-%d:7878","api_key":"key-%s-%d"}`,
					kind, i, kind, i)
				req := httptest.NewRequest(http.MethodPut, "/api/settings/arr/"+kind, strings.NewReader(payload))
				req.SetPathValue("kind", kind)
				rec := httptest.NewRecorder()
				svc.handleUpsertArr(rec, req)
				if rec.Code != http.StatusOK {
					errs <- fmt.Sprintf("%s #%d = %d: %s", kind, i, rec.Code, rec.Body.String())
				}
			}(kind, i)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	list := h.listArr()
	if len(list.Instances) != 2 {
		t.Fatalf("instances after concurrent upserts = %+v, want both kinds", list.Instances)
	}
	seen := map[string]bool{}
	for _, inst := range list.Instances {
		if seen[inst.Kind] {
			t.Fatalf("duplicate instance for %s: %+v", inst.Kind, list.Instances)
		}
		seen[inst.Kind] = true
	}
}

// arrSectionSnapshot reads the raw arr.connections section so a test can prove
// a request left the persisted metadata byte-identical.
func arrSectionSnapshot(t *testing.T, h *harness) string {
	t.Helper()
	var section string
	if err := h.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settings.KeyArrConnections).Scan(&section); err != nil {
		t.Fatalf("read arr section: %v", err)
	}
	return section
}

// TestArrConnectionTestUsesDraftValuesWithoutPersisting pins the draft test:
// the ping must reach the base URL and key from the request body, while the
// saved connection and its stored secret stay byte-identical.
func TestArrConnectionTestUsesDraftValuesWithoutPersisting(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	const savedKey = "saved-radarr-key"
	const draftKey = "draft-radarr-key"

	saved := arrmock.New(arrmock.Radarr, savedKey, arrmock.Fixtures{})
	t.Cleanup(saved.Close)
	draft := arrmock.New(arrmock.Radarr, draftKey, arrmock.Fixtures{})
	t.Cleanup(draft.Close)

	if code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": saved.URL, "api_key": savedKey, "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	before := arrSectionSnapshot(t, h)

	code, body := h.do(http.MethodPost, "/api/settings/arr/radarr/test", map[string]any{
		"base_url": draft.URL,
		"api_key":  draftKey,
	})
	if code != http.StatusOK {
		t.Fatalf("draft test = %d: %s", code, body)
	}

	// The draft instance was pinged with the draft key; the saved one was not.
	if reqs := draft.Requests(); len(reqs) != 1 || reqs[0].Path != "/api/v3/system/status" || reqs[0].APIKey != draftKey {
		t.Fatalf("draft pings = %+v", reqs)
	}
	if reqs := saved.Requests(); len(reqs) != 0 {
		t.Fatalf("saved instance was pinged instead of the draft: %+v", reqs)
	}

	// Nothing persisted: same section bytes, same secret, same listing.
	if after := arrSectionSnapshot(t, h); after != before {
		t.Fatalf("draft test rewrote the section:\nbefore=%s\nafter=%s", before, after)
	}
	if key, err := sec.Get("arr.radarr.api_key"); err != nil || key != savedKey {
		t.Fatalf("stored key after draft test = %q, %v", key, err)
	}
	list := h.listArr()
	if len(list.Instances) != 1 || list.Instances[0].BaseURL != saved.URL ||
		list.Instances[0].APIKey != secrets.Mask(savedKey) {
		t.Fatalf("listing after draft test = %+v", list.Instances)
	}
}

// TestArrConnectionTestBlankDraftKeyUsesStoredKey covers "blank key field means
// keep the stored key" for an empty and a whitespace-only draft: both must fall
// back to the saved secret instead of being sent as an empty X-Api-Key.
func TestArrConnectionTestBlankDraftKeyUsesStoredKey(t *testing.T) {
	const savedKey = "stored-sonarr-key"
	for _, draftKey := range []string{"", "   "} {
		t.Run(fmt.Sprintf("draft key %q", draftKey), func(t *testing.T) {
			h, sec := newArrHarness(t)
			h.setupAdmin()
			fake := arrmock.New(arrmock.Sonarr, savedKey, arrmock.Fixtures{})
			t.Cleanup(fake.Close)

			if code, body := h.do(http.MethodPut, "/api/settings/arr/sonarr", map[string]any{
				"base_url": "http://sonarr-old:8989", "api_key": savedKey, "enabled": true,
			}); code != http.StatusOK {
				t.Fatalf("create = %d: %s", code, body)
			}
			before := arrSectionSnapshot(t, h)

			code, body := h.do(http.MethodPost, "/api/settings/arr/sonarr/test", map[string]any{
				"base_url": fake.URL,
				"api_key":  draftKey,
			})
			if code != http.StatusOK {
				t.Fatalf("draft test with blank key = %d: %s", code, body)
			}
			if reqs := fake.Requests(); len(reqs) != 1 || reqs[0].APIKey != savedKey {
				t.Fatalf("pings = %+v, want the stored key", reqs)
			}
			if after := arrSectionSnapshot(t, h); after != before {
				t.Fatalf("blank-key test rewrote the section: %s", after)
			}
			if key, err := sec.Get("arr.sonarr.api_key"); err != nil || key != savedKey {
				t.Fatalf("stored key after blank-key test = %q, %v", key, err)
			}

			// No stored key and no usable draft key is still the 400 the saved
			// path returns: a whitespace-only key counts as absent.
			h2, _ := newArrHarness(t)
			h2.setupAdmin()
			code, body = h2.do(http.MethodPost, "/api/settings/arr/radarr/test", map[string]any{
				"base_url": fake.URL,
				"api_key":  draftKey,
			})
			if code != http.StatusBadRequest {
				t.Fatalf("draft test without any key = %d, want 400: %s", code, body)
			}
			if !bytes.Contains(body, []byte("api_key is not configured")) {
				t.Fatalf("draft test without any key body = %s", body)
			}
		})
	}
}

// TestArrConnectionTestInvalidDraftBaseURLIsBadRequest mirrors the upsert: a
// malformed draft URL is a client error, never a 502 from the client ctor.
func TestArrConnectionTestInvalidDraftBaseURLIsBadRequest(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	fake := arrmock.New(arrmock.Radarr, "abcdef-1234", arrmock.Fixtures{})
	t.Cleanup(fake.Close)

	if code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": fake.URL, "api_key": "abcdef-1234", "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	before := arrSectionSnapshot(t, h)

	upsertCode, upsertBody := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": "not-a-url", "api_key": "abcdef-1234",
	})
	testCode, testBody := h.do(http.MethodPost, "/api/settings/arr/radarr/test", map[string]any{
		"base_url": "not-a-url",
	})
	if upsertCode != http.StatusBadRequest {
		t.Fatalf("upsert with bad url = %d: %s", upsertCode, upsertBody)
	}
	if testCode != http.StatusBadRequest {
		t.Fatalf("test with bad draft url = %d, want 400: %s", testCode, testBody)
	}
	if !bytes.Contains(testBody, []byte("base_url must be an absolute http(s) URL")) {
		t.Fatalf("test error = %s, want the upsert's validation message", testBody)
	}
	if reqs := fake.Requests(); len(reqs) != 0 {
		t.Fatalf("invalid draft URL was still pinged: %+v", reqs)
	}
	if after := arrSectionSnapshot(t, h); after != before {
		t.Fatalf("invalid draft test rewrote the section: %s", after)
	}
	if key, err := sec.Get("arr.radarr.api_key"); err != nil || key != "abcdef-1234" {
		t.Fatalf("stored key after invalid draft = %q, %v", key, err)
	}
}

// TestArrConnectionTestEmptyBodyTestsSavedConnection keeps the pre-draft
// contract load-bearing: no body, `{}` and blank fields all test what is saved.
func TestArrConnectionTestEmptyBodyTestsSavedConnection(t *testing.T) {
	const savedKey = "saved-sonarr-key"
	for _, tc := range []struct {
		name string
		body any
	}{
		{"nil body", nil},
		{"empty object", map[string]any{}},
		{"blank fields", map[string]any{"base_url": "", "api_key": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sec := newArrHarness(t)
			h.setupAdmin()
			fake := arrmock.New(arrmock.Sonarr, savedKey, arrmock.Fixtures{})
			t.Cleanup(fake.Close)
			if code, body := h.do(http.MethodPut, "/api/settings/arr/sonarr", map[string]any{
				"base_url": fake.URL, "api_key": savedKey, "enabled": true,
			}); code != http.StatusOK {
				t.Fatalf("create = %d: %s", code, body)
			}
			before := arrSectionSnapshot(t, h)

			code, body := h.do(http.MethodPost, "/api/settings/arr/sonarr/test", tc.body)
			if code != http.StatusOK {
				t.Fatalf("test = %d: %s", code, body)
			}
			if !bytes.Contains(body, []byte(`"ok":true`)) {
				t.Fatalf("test body = %s", body)
			}
			reqs := fake.Requests()
			if len(reqs) != 1 || reqs[0].Path != "/api/v5/system/status" || reqs[0].APIKey != savedKey {
				t.Fatalf("pings = %+v", reqs)
			}
			if after := arrSectionSnapshot(t, h); after != before {
				t.Fatalf("saved-state test rewrote the section: %s", after)
			}
			if key, err := sec.Get("arr.sonarr.api_key"); err != nil || key != savedKey {
				t.Fatalf("stored key after saved-state test = %q, %v", key, err)
			}
		})
	}
}

// TestArrConnectionTestFailedDraftMutatesNothing checks a 502 from an
// unreachable draft host still leaves the stored connection and secret alone.
func TestArrConnectionTestFailedDraftMutatesNothing(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	const savedKey = "abcdef-1234"
	fake := arrmock.New(arrmock.Radarr, savedKey, arrmock.Fixtures{})
	t.Cleanup(fake.Close)
	if code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": fake.URL, "api_key": savedKey, "enabled": true,
	}); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	before := arrSectionSnapshot(t, h)

	// A closed test server refuses connections, so the ping fails as a network
	// error without touching any live host.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	code, body := h.do(http.MethodPost, "/api/settings/arr/radarr/test", map[string]any{
		"base_url": deadURL,
		"api_key":  "draft-key",
	})
	if code != http.StatusBadGateway {
		t.Fatalf("failed draft test = %d, want 502: %s", code, body)
	}
	if bytes.Contains(body, []byte("draft-key")) || bytes.Contains(body, []byte(savedKey)) {
		t.Fatalf("failed draft test leaked a key: %s", body)
	}
	if after := arrSectionSnapshot(t, h); after != before {
		t.Fatalf("failed draft test rewrote the section: %s", after)
	}
	if key, err := sec.Get("arr.radarr.api_key"); err != nil || key != savedKey {
		t.Fatalf("stored key after failed draft = %q, %v", key, err)
	}
	list := h.listArr()
	if len(list.Instances) != 1 || list.Instances[0].BaseURL != fake.URL {
		t.Fatalf("listing after failed draft = %+v", list.Instances)
	}
}

// arrTestFailureBody is the 502 shape the diagnostics tests decode.
type arrTestFailureBody struct {
	Error      string `json:"error"`
	Class      string `json:"class"`
	StatusCode int    `json:"status_code"`
	Host       string `json:"host"`
	Port       string `json:"port"`
}

// timeoutNetError is a bare net.Error whose Timeout() reports true, modelling a
// socket-level deadline that wraps no context/os deadline sentinel.
type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

// TestArrTestDiagnosticsClassifiesFailures drives the classifier directly: it
// must name the failure kind and carry the HTTP status code structurally, and
// it must never repeat error text.
func TestArrTestDiagnosticsClassifiesFailures(t *testing.T) {
	const path = "/api/v3/system/status"
	tests := []struct {
		name       string
		err        error
		baseURL    string
		wantClass  string
		wantStatus int
		wantHost   string
		wantPort   string
	}{
		{
			name:       "401 is auth",
			err:        &arr.AuthError{Code: http.StatusUnauthorized, Method: http.MethodGet, Path: path},
			baseURL:    "http://radarr:7878",
			wantClass:  "auth",
			wantStatus: 401,
			wantHost:   "radarr",
			wantPort:   "7878",
		},
		{
			name:       "403 is auth",
			err:        &arr.AuthError{Code: http.StatusForbidden, Method: http.MethodGet, Path: path},
			baseURL:    "http://radarr:7878",
			wantClass:  "auth",
			wantStatus: 403,
			wantHost:   "radarr",
			wantPort:   "7878",
		},
		{
			name:       "404 is status",
			err:        &arr.StatusError{Code: http.StatusNotFound, Method: http.MethodGet, Path: path},
			baseURL:    "http://radarr:7878",
			wantClass:  "status",
			wantStatus: 404,
			wantHost:   "radarr",
			wantPort:   "7878",
		},
		{
			name:       "500 is status",
			err:        &arr.StatusError{Code: http.StatusInternalServerError, Method: http.MethodGet, Path: path},
			baseURL:    "http://radarr:7878",
			wantClass:  "status",
			wantStatus: 500,
			wantHost:   "radarr",
			wantPort:   "7878",
		},
		{
			name:      "deadline is timeout",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrNetwork, path, context.DeadlineExceeded),
			baseURL:   "http://radarr:7878",
			wantClass: "timeout",
			wantHost:  "radarr",
			wantPort:  "7878",
		},
		{
			name:      "dns failure",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrNetwork, path, &net.DNSError{Err: "no such host", Name: "radarr.invalid", IsNotFound: true}),
			baseURL:   "http://radarr.invalid:7878",
			wantClass: "dns",
			wantHost:  "radarr.invalid",
			wantPort:  "7878",
		},
		{
			// A resolver that gives up is a DNS problem first: the class must
			// not degrade to the generic timeout because IsTimeout is set.
			name:      "dns timeout is dns",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrNetwork, path, &net.DNSError{Err: "i/o timeout", Name: "radarr.invalid", IsTimeout: true}),
			baseURL:   "http://radarr.invalid:7878",
			wantClass: "dns",
			wantHost:  "radarr.invalid",
			wantPort:  "7878",
		},
		{
			name:      "dns not found is dns",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrNetwork, path, &net.DNSError{Err: "no such host", Name: "radarr.invalid", IsNotFound: true}),
			baseURL:   "http://radarr.invalid:7878",
			wantClass: "dns",
			wantHost:  "radarr.invalid",
			wantPort:  "7878",
		},
		{
			// Socket-level deadline: net.Error.Timeout() without a context or
			// os deadline sentinel in the chain.
			name:      "socket timeout is timeout",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrNetwork, path, timeoutNetError{}),
			baseURL:   "http://radarr:7878",
			wantClass: "timeout",
			wantHost:  "radarr",
			wantPort:  "7878",
		},
		{
			name:      "tls failure",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrNetwork, path, x509.UnknownAuthorityError{}),
			baseURL:   "https://radarr.example.com",
			wantClass: "tls",
			wantHost:  "radarr.example.com",
			wantPort:  "443",
		},
		{
			name:      "plain network failure",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrNetwork, path, errors.New("connection refused")),
			baseURL:   "http://radarr:7878",
			wantClass: "network",
			wantHost:  "radarr",
			wantPort:  "7878",
		},
		{
			name:      "kind mismatch",
			err:       &arr.KindMismatchError{Expected: arr.KindRadarr, Got: "Sonarr"},
			baseURL:   "http://sonarr:8989",
			wantClass: "kind_mismatch",
			wantHost:  "sonarr",
			wantPort:  "8989",
		},
		{
			name:      "decode failure",
			err:       fmt.Errorf("%w: GET %s: %w", arr.ErrDecode, path, io.ErrUnexpectedEOF),
			baseURL:   "http://radarr:7878",
			wantClass: "decode",
			wantHost:  "radarr",
			wantPort:  "7878",
		},
		{
			name:      "url validation failure",
			err:       &arrBaseURLError{msg: "base_url must not contain credentials"},
			baseURL:   "http://radarr:7878",
			wantClass: "invalid",
			wantHost:  "radarr",
			wantPort:  "7878",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, status, host, port := arrTestDiagnostics(tt.err, tt.baseURL)
			if class != tt.wantClass {
				t.Errorf("class = %q, want %q", class, tt.wantClass)
			}
			if status != tt.wantStatus {
				t.Errorf("status_code = %d, want %d", status, tt.wantStatus)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("target = %s:%s, want %s:%s", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestArrTargetHostPortParsesPathPrefixAndPort pins host/port extraction for a
// URL with a path prefix, a defaulted port and an unparseable value.
func TestArrTargetHostPortParsesPathPrefixAndPort(t *testing.T) {
	tests := []struct {
		baseURL  string
		wantHost string
		wantPort string
	}{
		{"https://radarr.example.com/radarr", "radarr.example.com", "443"},
		{"http://radarr:7878/radarr", "radarr", "7878"},
		{"https://proxy.example.com:9443/sonarr/", "proxy.example.com", "9443"},
		{"http://192.168.1.10", "192.168.1.10", "80"},
		{"not-a-url", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.baseURL, func(t *testing.T) {
			host, port := arrTargetHostPort(tt.baseURL)
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("target = %s:%s, want %s:%s", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestArrConnectionTestFailureCarriesDiagnostics checks the 502 body of a real
// ping failure: the legacy error text is unchanged, the additive diagnostics
// describe the failure, and no key material reaches the raw JSON.
func TestArrConnectionTestFailureCarriesDiagnostics(t *testing.T) {
	t.Run("rejected key is auth/401", func(t *testing.T) {
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
			t.Fatalf("test = %d, want 502: %s", code, body)
		}
		if bytes.Contains(body, []byte(wrongKey)) {
			t.Fatalf("502 body leaked the API key: %s", body)
		}
		var got arrTestFailureBody
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode 502 body: %v (%s)", err, body)
		}
		if got.Error != "authentication failed: the API key was rejected" {
			t.Errorf("error = %q, want the unchanged operator message", got.Error)
		}
		if got.Class != "auth" || got.StatusCode != http.StatusUnauthorized {
			t.Errorf("diagnostics = %q/%d, want auth/401", got.Class, got.StatusCode)
		}
		u, err := url.Parse(fake.URL)
		if err != nil {
			t.Fatalf("parse mock URL: %v", err)
		}
		if got.Host != u.Hostname() || got.Port != u.Port() {
			t.Errorf("target = %s:%s, want %s:%s", got.Host, got.Port, u.Hostname(), u.Port())
		}
	})

	t.Run("unreachable host is network", func(t *testing.T) {
		h, _ := newArrHarness(t)
		h.setupAdmin()
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL
		dead.Close()
		const draftKey = "draft-key-do-not-leak"

		code, body := h.do(http.MethodPost, "/api/settings/arr/radarr/test", map[string]any{
			"base_url": deadURL, "api_key": draftKey,
		})
		if code != http.StatusBadGateway {
			t.Fatalf("test = %d, want 502: %s", code, body)
		}
		if bytes.Contains(body, []byte(draftKey)) {
			t.Fatalf("502 body leaked the API key: %s", body)
		}
		var got arrTestFailureBody
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode 502 body: %v (%s)", err, body)
		}
		if got.Error != "could not reach the instance: check the base URL and network" {
			t.Errorf("error = %q, want the unchanged operator message", got.Error)
		}
		if got.Class != "network" || got.StatusCode != 0 {
			t.Errorf("diagnostics = %q/%d, want network/0", got.Class, got.StatusCode)
		}
		u, err := url.Parse(deadURL)
		if err != nil {
			t.Fatalf("parse dead URL: %v", err)
		}
		if got.Host != u.Hostname() || got.Port != u.Port() {
			t.Errorf("target = %s:%s, want %s:%s", got.Host, got.Port, u.Hostname(), u.Port())
		}
	})

	// A 404 on the v5 base is not a generic 404: the handler must say which
	// product and version it required, keep the machine-readable class the UI
	// reads (status/404, inherited from the wrapped StatusError), and still leak
	// neither the key nor the host.
	t.Run("sonarr missing v5 is an api-version failure", func(t *testing.T) {
		h, _ := newArrHarness(t)
		h.setupAdmin()
		const key = "sonarr-key-do-not-leak"
		fake := arrmock.New(arrmock.Sonarr, key, arrmock.Fixtures{})
		t.Cleanup(fake.Close)
		// Every route 404s: that is how an instance serving no v5 answers, and
		// it keeps the failure off the auth and network paths.
		fake.FailAll(http.StatusNotFound)
		if code, body := h.do(http.MethodPut, "/api/settings/arr/sonarr", map[string]any{
			"base_url": fake.URL, "api_key": key,
		}); code != http.StatusOK {
			t.Fatalf("create = %d: %s", code, body)
		}

		code, body := h.do(http.MethodPost, "/api/settings/arr/sonarr/test", nil)
		if code != http.StatusBadGateway {
			t.Fatalf("test = %d, want 502: %s", code, body)
		}
		if bytes.Contains(body, []byte(key)) {
			t.Fatalf("502 body leaked the API key: %s", body)
		}
		var got arrTestFailureBody
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode 502 body: %v (%s)", err, body)
		}
		for _, want := range []string{"sonarr", "/api/v5", "Sonarr 5.x"} {
			if !strings.Contains(got.Error, want) {
				t.Errorf("error = %q, want it to name %q", got.Error, want)
			}
		}
		// The pinned release is prose in package arr and deliberately named in
		// one place only, so assert its shape here instead of copying it.
		if !regexp.MustCompile(`dubarr \d+\.\d+\.\d+ requires Sonarr 5\.x`).MatchString(got.Error) {
			t.Errorf("error = %q, want it to name the pinned dubarr release that requires Sonarr 5.x", got.Error)
		}
		if got.Class != "status" || got.StatusCode != http.StatusNotFound {
			t.Errorf("diagnostics = %q/%d, want status/404", got.Class, got.StatusCode)
		}
		u, err := url.Parse(fake.URL)
		if err != nil {
			t.Fatalf("parse mock URL: %v", err)
		}
		if strings.Contains(got.Error, u.Hostname()) {
			t.Errorf("error %q repeats the base URL host %q", got.Error, u.Hostname())
		}
	})
}

// TestArrBaseURLRejectsEmbeddedCredentials: a URL carrying userinfo is a client
// error on both the upsert and the draft test, and nothing is persisted.
func TestArrBaseURLRejectsEmbeddedCredentials(t *testing.T) {
	h, sec := newArrHarness(t)
	h.setupAdmin()
	const withCreds = "http://user:super-secret-pass@radarr:7878"

	code, body := h.do(http.MethodPut, "/api/settings/arr/radarr", map[string]any{
		"base_url": withCreds, "api_key": "abcdef-1234",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("upsert with credentials = %d, want 400: %s", code, body)
	}
	if !bytes.Contains(body, []byte("base_url must not contain credentials")) {
		t.Errorf("upsert error = %s, want the credentials rejection", body)
	}
	if bytes.Contains(body, []byte("super-secret-pass")) {
		t.Errorf("upsert error echoed the password: %s", body)
	}
	if res := h.listArr(); len(res.Instances) != 0 {
		t.Fatalf("rejected upsert persisted: %+v", res.Instances)
	}
	if _, err := sec.Get("arr.radarr.api_key"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("rejected upsert stored a secret: %v", err)
	}

	code, body = h.do(http.MethodPost, "/api/settings/arr/radarr/test", map[string]any{
		"base_url": withCreds, "api_key": "abcdef-1234",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("draft test with credentials = %d, want 400: %s", code, body)
	}
	if !bytes.Contains(body, []byte("base_url must not contain credentials")) {
		t.Errorf("draft test error = %s, want the credentials rejection", body)
	}
}
