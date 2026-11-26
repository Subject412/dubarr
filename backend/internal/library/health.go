// The Library health endpoint: whether the last arr sync pass per instance
// worked (migration 0005), plus how much of the library is cached.
package library

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/settings"
)

// instanceHealth is one arr instance's sync health. LastOK is tri-state: true
// = the last attempt succeeded, false = it failed, null = never attempted
// (which is why it is a *bool). LastSyncAt is the last SUCCESS, so after a
// failing pass it still carries the last-good timestamp while LastError
// describes the failure.
//
// LastSyncAt and LastAttemptAt are pointers so "never succeeded" / "never
// attempted" marshal as null instead of an empty string that would read like a
// real (if blank) timestamp. LastError stays a string: "" legitimately means
// "no error".
type instanceHealth struct {
	Kind          string  `json:"kind"`
	Name          string  `json:"name"`
	Enabled       bool    `json:"enabled"`
	LastSyncAt    *string `json:"last_sync_at"`
	LastAttemptAt *string `json:"last_attempt_at"`
	LastOK        *bool   `json:"last_ok"`
	LastError     string  `json:"last_error"`
	// ItemsSeen is how many arr_library_cache rows this instance owns.
	ItemsSeen int64 `json:"items_seen"`
}

type healthResponse struct {
	Instances  []instanceHealth `json:"instances"`
	CacheItems int64            `json:"cache_items"`
}

// handleHealth merges two sources into one list keyed by kind:
//
//   - arr_instances rows (real sync state and per-instance cache counts);
//   - arr.connections entries that have no arr_instances row yet, reported as
//     never attempted. A freshly saved connection only gets its arr_instances
//     row on the first sync pass, so without this union a configured instance
//     would be invisible and "configured but never synced" unreachable.
//
// A kind present in both appears once: the store row is authoritative. The
// merged list keeps the store's deterministic (kind, then name) ordering.
func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "library store is not configured")
		return
	}
	rows, cacheItems, err := s.store.ArrLibraryHealth(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read arr sync health")
		return
	}
	instances := make([]instanceHealth, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		seen[row.Kind] = struct{}{}
		entry := instanceHealth{
			Kind:      row.Kind,
			Name:      row.Name,
			Enabled:   row.Enabled,
			LastError: row.LastError.String,
			ItemsSeen: row.ItemsSeen,
		}
		// NullString -> *string keeps "never happened" as JSON null; "" would
		// be indistinguishable from a real blank value.
		if row.LastSyncAt.Valid {
			ts := row.LastSyncAt.String
			entry.LastSyncAt = &ts
		}
		if row.LastAttemptAt.Valid {
			ts := row.LastAttemptAt.String
			entry.LastAttemptAt = &ts
		}
		// NullBool -> *bool keeps "never attempted" distinct from "failed".
		if row.LastOK.Valid {
			ok := row.LastOK.Bool
			entry.LastOK = &ok
		}
		instances = append(instances, entry)
	}
	// Configured-but-unsynced instances: every field describing an attempt
	// stays at its zero value, which marshals as null/"" — exactly the
	// never-attempted shape.
	for _, conn := range s.configuredConnections(r.Context()) {
		if _, ok := seen[conn.Kind]; ok {
			continue
		}
		seen[conn.Kind] = struct{}{}
		instances = append(instances, instanceHealth{
			Kind:    conn.Kind,
			Name:    connectionDisplayName(conn.Kind),
			Enabled: conn.Enabled,
		})
	}
	// The store rows already arrive ordered by (kind, name); sorting the merged
	// list the same way (case-insensitively, like COLLATE NOCASE) and stably
	// keeps their relative order.
	sort.SliceStable(instances, func(i, j int) bool {
		if instances[i].Kind != instances[j].Kind {
			return instances[i].Kind < instances[j].Kind
		}
		return strings.ToLower(instances[i].Name) < strings.ToLower(instances[j].Name)
	})
	writeJSON(w, http.StatusOK, healthResponse{
		Instances:  instances,
		CacheItems: cacheItems,
	})
}

// configuredConnections returns the arr.connections entries, or nil when no
// settings store is wired or the section is absent. The union itself is owned
// by arr.ConfiguredConnections so this view and the queue diagnostics cannot
// disagree. A read or decode failure is logged and swallowed: health must stay
// available even when settings cannot be read, so the caller falls back to the
// store-derived instances.
func (s *Service) configuredConnections(ctx context.Context) []settings.ArrConnection {
	conns, err := arr.ConfiguredConnections(ctx, s.settings)
	if err != nil {
		slog.Default().Warn("library health: read arr connections",
			"component", "library", "error", err)
	}
	return conns
}

// connectionDisplayName mirrors arrsync's displayName for a kind, so the entry
// shown before the first poll carries the same name the sync pass will write
// into arr_instances ("Radarr" / "Sonarr").
func connectionDisplayName(kind string) string {
	if kind == "" {
		return ""
	}
	return strings.ToUpper(kind[:1]) + kind[1:]
}
