package logs

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultAPILimit and maxAPILimit bound GET /api/logs.
const (
	defaultAPILimit = 200
	maxAPILimit     = 1000
)

// Dependencies are the collaborators the logs API needs.
type Dependencies struct {
	// Logs is the central logger owning the ring. Nil answers 503.
	Logs *Logger
	// RequireAdmin wraps every route. Nil answers 503 rather than running
	// unauthenticated (logs may contain sensitive paths and identifiers).
	RequireAdmin func(http.Handler) http.Handler
}

// Service holds the resolved dependencies for the registered handlers.
type Service struct {
	logs         *Logger
	requireAdmin func(http.Handler) http.Handler
}

// RegisterRoutes mounts the admin-only log endpoints:
//
//	GET /api/logs?level=&component=&limit=&since=
//	GET /api/logs/level
//	PUT /api/logs/level
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	svc := &Service{logs: deps.Logs, requireAdmin: deps.RequireAdmin}
	admin := func(h http.HandlerFunc) http.Handler {
		if svc.requireAdmin == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusServiceUnavailable, "authorization is not configured")
			})
		}
		return svc.requireAdmin(h)
	}
	mux.Handle("GET /api/logs", admin(svc.handleList))
	mux.Handle("GET /api/logs/level", admin(svc.handleGetLevel))
	mux.Handle("PUT /api/logs/level", admin(svc.handleSetLevel))
	return svc
}

type listResponse struct {
	Entries []Entry `json:"entries"`
	Level   string  `json:"level"`
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "the log sink is unavailable")
		return
	}

	q := Query{}
	if raw := strings.TrimSpace(r.URL.Query().Get("level")); raw != "" {
		if _, ok := ParseLevel(raw); !ok {
			writeError(w, http.StatusBadRequest, "level must be debug, info, warn or error")
			return
		}
		q.MinLevel = raw
	}
	q.Component = strings.TrimSpace(r.URL.Query().Get("component"))

	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxAPILimit {
			n = maxAPILimit
		}
		q.Limit = n
	} else {
		q.Limit = defaultAPILimit
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		ts, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			ts, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be an RFC 3339 timestamp")
			return
		}
		q.Since = ts
	}

	writeJSON(w, http.StatusOK, listResponse{Entries: s.logs.Entries(q), Level: s.logs.Level()})
}

func (s *Service) handleGetLevel(w http.ResponseWriter, _ *http.Request) {
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "the log sink is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"level": s.logs.Level()})
}

func (s *Service) handleSetLevel(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "the log sink is unavailable")
		return
	}
	var body struct {
		Level string `json:"level"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.logs.SetLevel(body.Level); err != nil {
		writeError(w, http.StatusBadRequest, "level must be debug, info, warn or error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"level": s.logs.Level()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
