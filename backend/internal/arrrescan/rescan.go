// Package arrrescan triggers a best-effort Radarr/Sonarr rescan after a
// successful mux, so the *arr instance notices the newly published audio track.
//
// A rescan is explicitly non-fatal: RescanAfterMux returns a typed error the
// mux worker logs without changing the mux result, and it skips silently
// (ErrNoInstance) when the library row's arr instance is missing or disabled.
// Credentials come from arr_instances and are decrypted with the same
// AES-256-GCM store as the rest of the appliance; they are never logged.
package arrrescan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
)

// Sentinel errors classify rescan outcomes. Callers test with errors.Is;
// ErrNoInstance is the documented "skip, do not log as failure" case.
var (
	// ErrNoInstance means the row's arr instance does not exist or is
	// disabled, so no request was made.
	ErrNoInstance = errors.New("arrrescan: no enabled arr instance")
	// ErrInvalidLibraryRow means the row lacks an id the command needs
	// (movieId, seriesId) or names an unknown media type.
	ErrInvalidLibraryRow = errors.New("arrrescan: invalid library cache row")
	// ErrInstanceKind means the instance kind does not serve the row's media
	// type (a movie row pointing at Sonarr, or vice versa).
	ErrInstanceKind = errors.New("arrrescan: arr instance kind does not match media type")
)

// Decrypter is the subset of secrets.Service used here.
type Decrypter interface {
	DecryptString(blob []byte) (string, error)
}

// LibraryCacheRow is the arr_library_cache projection a rescan needs.
type LibraryCacheRow struct {
	// ArrInstanceID is arr_library_cache.arr_instance_id.
	ArrInstanceID int64
	// MediaType is "movie" or "episode".
	MediaType string
	// ExternalItemID is the Radarr movieId or Sonarr episodeId.
	ExternalItemID int64
	// SeriesID is the Sonarr seriesId (0 for movies).
	SeriesID int64
}

// Deps configure a Rescanner.
type Deps struct {
	// DB is the shared SQLite handle; arr_instances is read directly so this
	// package does not widen the store API for one query.
	DB *sql.DB
	// Secrets decrypts arr_instances.api_key_enc.
	Secrets Decrypter
	// Timeout bounds one arr request; 0 selects arr.DefaultTimeout.
	Timeout time.Duration
}

// Rescanner resolves an arr instance and posts one rescan command.
type Rescanner struct {
	db      *sql.DB
	secrets Decrypter
	timeout time.Duration
}

// New builds a Rescanner. It fails closed when the database or secret store is
// missing so a mis-wired composition root is caught at boot.
func New(deps Deps) (*Rescanner, error) {
	if deps.DB == nil {
		return nil, errors.New("arrrescan: database handle is required")
	}
	if deps.Secrets == nil {
		return nil, errors.New("arrrescan: secret decrypter is required")
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = arr.DefaultTimeout
	}
	return &Rescanner{db: deps.DB, secrets: deps.Secrets, timeout: timeout}, nil
}

// RescanAfterMux rescans the library item named by row. It is best-effort: the
// caller logs the returned error and keeps the mux result. ErrNoInstance means
// the instance is absent or disabled and nothing was sent.
func (r *Rescanner) RescanAfterMux(ctx context.Context, row LibraryCacheRow) error {
	if r == nil || r.db == nil || r.secrets == nil {
		return errors.New("arrrescan: rescanner is not initialised")
	}
	if row.ArrInstanceID <= 0 {
		return fmt.Errorf("%w: missing arr_instance_id", ErrInvalidLibraryRow)
	}

	kind, baseURL, apiKeyEnc, enabled, err := r.instance(ctx, row.ArrInstanceID)
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("%w: instance %d is disabled", ErrNoInstance, row.ArrInstanceID)
	}

	apiKey, err := r.secrets.DecryptString(apiKeyEnc)
	if err != nil {
		return fmt.Errorf("arrrescan: decrypt instance %d API key: %w", row.ArrInstanceID, err)
	}

	switch row.MediaType {
	case "movie":
		if row.ExternalItemID <= 0 {
			return fmt.Errorf("%w: movie row has no external_item_id", ErrInvalidLibraryRow)
		}
		if kind != arr.KindRadarr {
			return fmt.Errorf("%w: instance %d is %s, not radarr", ErrInstanceKind, row.ArrInstanceID, kind)
		}
		client, err := arr.NewRadarrClient(baseURL, apiKey, r.timeout)
		if err != nil {
			return fmt.Errorf("arrrescan: %w", err)
		}
		if err := client.RescanMovie(ctx, int(row.ExternalItemID)); err != nil {
			return fmt.Errorf("arrrescan: rescan movie %d: %w", row.ExternalItemID, err)
		}
		return nil
	case "episode":
		if row.SeriesID <= 0 {
			return fmt.Errorf("%w: episode row has no series_id", ErrInvalidLibraryRow)
		}
		if kind != arr.KindSonarr {
			return fmt.Errorf("%w: instance %d is %s, not sonarr", ErrInstanceKind, row.ArrInstanceID, kind)
		}
		client, err := arr.NewSonarrClient(baseURL, apiKey, r.timeout)
		if err != nil {
			return fmt.Errorf("arrrescan: %w", err)
		}
		if err := client.RescanSeries(ctx, int(row.SeriesID)); err != nil {
			return fmt.Errorf("arrrescan: rescan series %d: %w", row.SeriesID, err)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown media type %q", ErrInvalidLibraryRow, row.MediaType)
	}
}

// instance reads the connection row. A missing row maps to ErrNoInstance so the
// caller can skip quietly, exactly like a disabled instance.
func (r *Rescanner) instance(ctx context.Context, id int64) (kind arr.Kind, baseURL string, apiKeyEnc []byte, enabled bool, err error) {
	var rawKind string
	err = r.db.QueryRowContext(ctx,
		`SELECT type, base_url, api_key_enc, enabled FROM arr_instances WHERE id = ?`, id).
		Scan(&rawKind, &baseURL, &apiKeyEnc, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil, false, fmt.Errorf("%w: instance %d not found", ErrNoInstance, id)
	}
	if err != nil {
		return "", "", nil, false, fmt.Errorf("arrrescan: read instance %d: %w", id, err)
	}
	return arr.Kind(rawKind), baseURL, apiKeyEnc, enabled, nil
}
