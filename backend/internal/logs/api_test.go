package logs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type apiHarness struct {
	t     *testing.T
	mux   *http.ServeMux
	logs  *Logger
	admin bool
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	l, _ := newTestLogger(t, Options{})
	h := &apiHarness{t: t, mux: http.NewServeMux(), logs: l, admin: true}
	RegisterRoutes(h.mux, Dependencies{
		Logs: l,
		RequireAdmin: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !h.admin {
					writeError(w, http.StatusForbidden, "administrator access required")
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	return h
}

func (h *apiHarness) do(method, path, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	h.mux.ServeHTTP(rec, req)
	return rec
}

func TestAPIListFilters(t *testing.T) {
	h := newAPIHarness(t)
	if err := h.logs.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	log := h.logs.Logger()
	log.Debug("debug a", "component", "watch")
	log.Info("info a", "component", "watch")
	log.Warn("warn b", "component", "syncworker")

	rec := h.do(http.MethodGet, "/api/logs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var body struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 3 {
		t.Fatalf("unfiltered listing returned %d entries, want all 3: %+v", len(body.Entries), body.Entries)
	}

	rec = h.do(http.MethodGet, "/api/logs?level=info", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Entries) != 2 {
		t.Fatalf("level=info returned %d entries, want 2: %+v", len(body.Entries), body.Entries)
	}

	rec = h.do(http.MethodGet, "/api/logs?level=debug", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Entries) != 3 {
		t.Fatalf("level=debug returned %d entries, want 3", len(body.Entries))
	}

	rec = h.do(http.MethodGet, "/api/logs?component=syncworker", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Entries) != 1 || body.Entries[0].Message != "warn b" {
		t.Fatalf("component filter returned %+v", body.Entries)
	}

	rec = h.do(http.MethodGet, "/api/logs?limit=1", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Entries) != 1 || body.Entries[0].Message != "warn b" {
		t.Fatalf("limit filter returned %+v", body.Entries)
	}

	rec = h.do(http.MethodGet, "/api/logs?level=bogus", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid level status = %d, want 400", rec.Code)
	}
}

func TestAPISinceFilter(t *testing.T) {
	h := newAPIHarness(t)
	log := h.logs.Logger()
	log.Info("first")
	first := h.logs.Entries(Query{})[0].Time
	log.Info("second")

	since := first.Add(time.Nanosecond).UTC().Format(time.RFC3339Nano)
	rec := h.do(http.MethodGet, "/api/logs?since="+since, "")
	var body struct {
		Entries []Entry `json:"entries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Entries) != 1 || body.Entries[0].Message != "second" {
		t.Fatalf("since filter returned %+v", body.Entries)
	}

	rec = h.do(http.MethodGet, "/api/logs?since=yesterday", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid since status = %d, want 400", rec.Code)
	}
}

func TestAPIAuthorization(t *testing.T) {
	h := newAPIHarness(t)
	h.admin = false
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/logs", ""},
		{http.MethodGet, "/api/logs/level", ""},
		{http.MethodPut, "/api/logs/level", `{"level":"debug"}`},
	} {
		rec := h.do(tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, want 403 for non-admin", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAPISinkUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux, Dependencies{
		RequireAdmin: func(next http.Handler) http.Handler { return next },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the sink is missing", rec.Code)
	}
}

func TestAPILevelRoundTrip(t *testing.T) {
	h := newAPIHarness(t)

	rec := h.do(http.MethodGet, "/api/logs/level", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET level status = %d", rec.Code)
	}
	var body struct {
		Level string `json:"level"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Level != "info" {
		t.Fatalf("initial level = %q, want info", body.Level)
	}

	rec = h.do(http.MethodPut, "/api/logs/level", `{"level":"debug"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT level status = %d (%s)", rec.Code, rec.Body)
	}
	if h.logs.Level() != "debug" {
		t.Fatalf("runtime level = %q, want debug", h.logs.Level())
	}

	rec = h.do(http.MethodPut, "/api/logs/level", `{"level":"shout"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT level status = %d, want 400", rec.Code)
	}
	rec = h.do(http.MethodPut, "/api/logs/level", `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400", rec.Code)
	}
}
