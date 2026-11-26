package arr

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

// ConfiguredConnections returns the arr.connections entries saved in settings,
// in section order, skipping any entry without a kind.
//
// It is the single owner of "which arr instances are configured": the library
// health endpoint and the queue diagnostics both build their configured-vs-
// synced view from this list, so the two screens cannot drift apart on what
// "configured" means. A nil store (settings not wired) or an absent section
// yields no connections and no error. A read or decode failure yields no
// connections plus an error, and the CALLER decides how to degrade: both
// current callers log and carry on, because neither endpoint may fail over
// diagnostics.
func ConfiguredConnections(ctx context.Context, store *settings.Store) ([]settings.ArrConnection, error) {
	if store == nil {
		return nil, nil
	}
	raw, ok, err := store.Get(ctx, settings.KeyArrConnections)
	if err != nil {
		return nil, fmt.Errorf("read arr connections: %w", err)
	}
	if !ok {
		return nil, nil
	}
	var conns settings.ArrConnections
	if err := json.Unmarshal(raw, &conns); err != nil {
		return nil, fmt.Errorf("decode arr connections: %w", err)
	}
	out := make([]settings.ArrConnection, 0, len(conns.Instances))
	for _, conn := range conns.Instances {
		// Validation guarantees radarr/sonarr, but a malformed legacy row must
		// not add a nameless entry to either view.
		if conn.Kind == "" {
			continue
		}
		out = append(out, conn)
	}
	return out, nil
}
