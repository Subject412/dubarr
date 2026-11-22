package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

func statusBody(t *testing.T, deps Dependencies) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	RegisterRoutes(mux, deps)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/status", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}

// TestStatusShape pins the IDEA.md contract: initialized/authenticated/
// keyring_available, and nothing legacy like setup_required.
func TestStatusShape(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	code, body := statusBody(t, Dependencies{
		DB:               st.DB,
		SessionTTL:       time.Hour,
		KeyringAvailable: func() bool { return true },
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, key := range []string{"initialized", "authenticated", "keyring_available"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status is missing %q: %v", key, body)
		}
	}
	if _, ok := body["setup_required"]; ok {
		t.Errorf("status still exposes legacy setup_required: %v", body)
	}
	if body["initialized"] != false || body["authenticated"] != false {
		t.Errorf("fresh status = %v, want initialized=false authenticated=false", body)
	}
	if body["keyring_available"] != true {
		t.Errorf("keyring_available = %v, want true", body["keyring_available"])
	}
}

// TestStatusReportsKeySource keeps the pinned-source state additive: the marker
// is exposed read-only and the per-boot re-pin flag is reported when supplied.
func TestStatusReportsKeySource(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(
		`INSERT INTO settings (key, value) VALUES ('secrets.key_source', 'keyfile')`,
	); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	code, body := statusBody(t, Dependencies{
		DB:               st.DB,
		KeySourceChanged: func() bool { return true },
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := body["keySource"]; got != "keyfile" {
		t.Errorf("keySource = %v, want keyfile", got)
	}
	if got := body["keySourceChanged"]; got != true {
		t.Errorf("keySourceChanged = %v, want true", got)
	}

	// Existing contract fields survive unchanged.
	for _, key := range []string{"initialized", "authenticated", "keyring_available"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status is missing %q: %v", key, body)
		}
	}
}
func TestStatusKeyringIsNilSafe(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	code, body := statusBody(t, Dependencies{DB: st.DB})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["keyring_available"] != false {
		t.Errorf("keyring_available = %v, want false when unset", body["keyring_available"])
	}
	if body["authenticated"] != false {
		t.Errorf("authenticated = %v, want false without a session", body["authenticated"])
	}
}
