// Package settings persists Dubarr's application configuration in the
// `settings` key/value table and validates every write.
//
// Layout follows IDEA.md §4: one row per settings section, the row value being
// the JSON object for that section. Section keys:
//
//	storage.paths      watch_dir, vault_dir, work_dir, output_dir, media_roots
//	languages.policy   target/reference languages, commentary exclusion, export mode
//	target.tag_filter  Tdarr destination filename gate (ADR-0005)
//	sync.defaults      max offset, checkpoint/preview sampling positions
//	security.options   vault read-only, tag override, AV1 warnings
//	mux.options        keep .bak, track title written into Matroska
//	watch.options      watch-folder scan cadence and stability window
//	arr.options        Radarr/Sonarr polling cadence
//	arr.connections    Radarr/Sonarr instances (API keys live in internal/secrets)
//
// mux/watch/arr sections are an extension of the IDEA.md key list: the spec
// only names the first five, but the settled defaults for keep_bak, the track
// title, the 10 s watch scan and the 300 s *arr poll need a home. They live as
// separate sections rather than being smuggled into sync.defaults.
//
// Secret material NEVER belongs in a served section: auth.session_key and any
// key that looks like a credential are rejected on write and stripped from
// reads/exports.
package settings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/langtoken"
	"github.com/Subject412/dubarr/backend/internal/secrets"
)

// Section keys.
const (
	KeyStoragePaths    = "storage.paths"
	KeyLanguagesPolicy = "languages.policy"
	KeyTargetTagFilter = "target.tag_filter"
	KeySyncDefaults    = "sync.defaults"
	KeySecurityOptions = "security.options"
	KeyMuxOptions      = "mux.options"
	KeyWatchOptions    = "watch.options"
	KeyArrOptions      = "arr.options"
	KeyArrConnections  = "arr.connections"
	KeyImportsOptions  = "imports.options"
)

var knownKeys = []string{
	KeyStoragePaths, KeyLanguagesPolicy, KeyTargetTagFilter, KeySyncDefaults,
	KeySecurityOptions, KeyMuxOptions, KeyWatchOptions, KeyArrOptions,
	KeyArrConnections, KeyImportsOptions,
}

// KnownKeys returns the writable section keys in stable order.
func KnownKeys() []string { return append([]string(nil), knownKeys...) }

// StoragePaths are the five logical storage areas (IDEA.md §3.3). Empty values
// mean "not configured yet" and are filled by the setup wizard.
type StoragePaths struct {
	WatchDir string `json:"watch_dir"`
	VaultDir string `json:"vault_dir"`
	WorkDir  string `json:"work_dir"`
	// OutputDir is the target-independent destination root for
	// mux.options.output_mode=separate. It is a root in its own right (the
	// muxer canonicalizes it again with EvalSymlinks) and must never overlap
	// the Audio Vault (ADR-0002).
	OutputDir  string   `json:"output_dir"`
	MediaRoots []string `json:"media_roots"`
}

// LanguagesPolicy drives automatic track selection and export.
type LanguagesPolicy struct {
	TargetImportLanguages   []string `json:"target_import_languages"`
	ExtraReferenceLanguages []string `json:"extra_reference_languages"`
	OriginalLanguageFromArr bool     `json:"original_language_from_arr"`
	// PreferOriginalLanguage makes the arr original the first-choice reference.
	// Off keeps it available as an ordinary reference candidate.
	PreferOriginalLanguage bool `json:"prefer_original_language"`
	ExcludeCommentary      bool `json:"exclude_commentary"`
	// ExportMode is "rule_based" (only policy-selected tracks) or "all".
	ExportMode string `json:"export_mode"`
}

// UnmarshalJSON makes an absent prefer_original_language mean true. Legacy rows
// written before the key existed predate the settled upgrade default; plain
// decoding would read them as false and silently flip the operator's policy.
// An explicit false is preserved. Unknown fields stay rejected like every other
// section decoded by normalize.
func (p *LanguagesPolicy) UnmarshalJSON(data []byte) error {
	type plain LanguagesPolicy
	aux := struct {
		*plain
		PreferOriginalLanguage *bool `json:"prefer_original_language"`
	}{plain: (*plain)(p)}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&aux); err != nil {
		return err
	}
	if aux.PreferOriginalLanguage == nil {
		p.PreferOriginalLanguage = true
	} else {
		p.PreferOriginalLanguage = *aux.PreferOriginalLanguage
	}
	return nil
}

// TargetTagFilter is the Tdarr destination filename gate (ADR-0005).
type TargetTagFilter struct {
	RequiredFilenameTags []string `json:"required_filename_tags"`
	TagMatchMode         string   `json:"tag_match_mode"`
	TagCaseSensitive     bool     `json:"tag_case_sensitive"`
	ExcludedFilenameTags []string `json:"excluded_filename_tags"`
	EnforceTagFilter     bool     `json:"enforce_tag_filter"`
}

// SyncDefaults bounds the automatic offset search and sampling windows.
type SyncDefaults struct {
	MaxOffsetSec           int   `json:"max_offset_sec"`
	CheckpointPositionsPct []int `json:"checkpoint_positions_pct"`
	CheckpointWindowSec    int   `json:"checkpoint_window_sec"`
	PreviewPositionsPct    []int `json:"preview_positions_pct"`
	PreviewWindowSec       int   `json:"preview_window_sec"`
}

// SecurityOptions are appliance-wide safety switches.
type SecurityOptions struct {
	VaultReadOnly         bool `json:"vault_read_only"`
	AllowForceTagOverride bool `json:"allow_force_tag_override"`
	RequireAV1Warning     bool `json:"require_av1_warning"`
	RequireAV1Strict      bool `json:"require_av1_strict"`
}

// Mux output destination modes (ADR-0011).
const (
	MuxOutputReplace  = "replace"
	MuxOutputSeparate = "separate"
)

// MuxOptions control atomic muxing side effects.
type MuxOptions struct {
	KeepBak         bool   `json:"keep_bak"`
	TrackTitleInMKV string `json:"track_title_in_mkv"`
	// OutputMode is "replace" (atomically replace the Target, current
	// behaviour) or "separate" (write <basename>.dubarr.mkv into
	// storage.paths.output_dir and leave the Target untouched, ADR-0011).
	OutputMode string `json:"output_mode"`
}

// UnmarshalJSON makes an absent output_mode mean "replace". Legacy rows written
// before the key existed must keep the current in-place behaviour; plain
// decoding would read them as an empty mode and fail validation.
func (m *MuxOptions) UnmarshalJSON(data []byte) error {
	type plain MuxOptions
	aux := struct {
		*plain
		OutputMode *string `json:"output_mode"`
	}{plain: (*plain)(m)}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&aux); err != nil {
		return err
	}
	if aux.OutputMode == nil {
		m.OutputMode = MuxOutputReplace
	} else {
		m.OutputMode = *aux.OutputMode
	}
	return nil
}

// WatchOptions configure donor-folder scanning. StabilityCheckIntervalSec is
// how far apart the two size checks must be (>= 4 s per IDEA.md §3.5).
type WatchOptions struct {
	ScanIntervalSec           int `json:"scan_interval_sec"`
	StabilityCheckIntervalSec int `json:"stability_check_interval_sec"`
}

// ArrOptions configure Radarr/Sonarr polling.
type ArrOptions struct {
	PollIntervalSec int  `json:"poll_interval_sec"`
	ManualSync      bool `json:"manual_sync"`
}

// ImportsOptions configure approval-queue donor pairing. DonorPatterns are
// matched case-insensitively against sibling filenames; a pattern without '*'
// is a substring match, with '*' it is a glob (e.g. "PL.DUB", "*LEKTOR.PL*").
type ImportsOptions struct {
	DonorPatterns []string `json:"donor_patterns"`
}

// ArrConnection is one Radarr or Sonarr instance. The API key is deliberately
// absent: it lives in the encrypted secrets store and only a masked suffix is
// derived when the API lists connections.
type ArrConnection struct {
	// Kind is "radarr" or "sonarr"; it is also the identity of the instance.
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
}

// ArrConnections is the arr.connections section: at most one instance per kind.
type ArrConnections struct {
	Instances []ArrConnection `json:"instances"`
}

// Store reads and writes settings sections on the shared database handle.
type Store struct {
	db *sql.DB
}

// New wraps an open database handle. It does not seed; call Seed once at
// startup.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the underlying handle (used by the API layer for browse roots).
func (s *Store) DB() *sql.DB { return s.db }

// Defaults returns the settled first-run configuration. dataDir supplies the
// default WORK_DIR (<DATA_DIR>/work); the other paths are deliberately empty so
// the wizard forces an explicit choice.
func Defaults(dataDir string) map[string]any {
	workDir := ""
	if dataDir != "" {
		workDir = filepath.Join(dataDir, "work")
	}
	return map[string]any{
		KeyStoragePaths: StoragePaths{
			WorkDir:    workDir,
			MediaRoots: []string{},
		},
		KeyLanguagesPolicy: LanguagesPolicy{
			TargetImportLanguages:   []string{"pl"},
			ExtraReferenceLanguages: []string{"en"},
			OriginalLanguageFromArr: true,
			PreferOriginalLanguage:  true,
			ExcludeCommentary:       true,
			ExportMode:              "rule_based",
		},
		KeyTargetTagFilter: TargetTagFilter{
			RequiredFilenameTags: []string{"[ArcA310]"},
			TagMatchMode:         "ANY",
			TagCaseSensitive:     false,
			ExcludedFilenameTags: []string{},
			EnforceTagFilter:     true,
		},
		KeySyncDefaults: SyncDefaults{
			MaxOffsetSec:           180,
			CheckpointPositionsPct: []int{10, 25, 40, 55, 70, 85},
			CheckpointWindowSec:    150,
			PreviewPositionsPct:    []int{10, 25, 50, 75, 90},
			PreviewWindowSec:       60,
		},
		KeySecurityOptions: SecurityOptions{
			VaultReadOnly:         true,
			AllowForceTagOverride: false,
			RequireAV1Warning:     true,
			RequireAV1Strict:      false,
		},
		KeyMuxOptions: MuxOptions{
			KeepBak:         true,
			TrackTitleInMKV: "Polski Dubbing",
			OutputMode:      MuxOutputReplace,
		},
		KeyWatchOptions: WatchOptions{
			ScanIntervalSec:           10,
			StabilityCheckIntervalSec: 4,
		},
		KeyArrOptions: ArrOptions{
			PollIntervalSec: 300,
			ManualSync:      true,
		},
		KeyArrConnections: ArrConnections{
			Instances: []ArrConnection{},
		},
		KeyImportsOptions: ImportsOptions{
			DonorPatterns: []string{"PL.DUB"},
		},
	}
}

// Seed inserts defaults with INSERT OR IGNORE, so it is safe on every startup
// and never overwrites a value the user has edited.
func (s *Store) Seed(ctx context.Context, dataDir string) error {
	defaults := Defaults(dataDir)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: begin seed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, key := range knownKeys {
		raw, err := json.Marshal(defaults[key])
		if err != nil {
			return fmt.Errorf("settings: marshal default %s: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			key, string(raw), time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
			return fmt.Errorf("settings: seed %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: commit seed: %w", err)
	}
	return nil
}

// Get returns the stored JSON for a writable section key.
func (s *Store) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if !isKnownKey(key) {
		return nil, false, fmt.Errorf("settings: %q is not a settings section: %w", key, ErrUnknownKey)
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("settings: read %s: %w", key, err)
	}
	return serveable(key, value), true, nil
}

// serveable applies load-time defaults to a stored section before it reaches a
// caller. It exists for languages.policy and mux.options: legacy rows predate
// prefer_original_language / output_mode, and the UI/API/export all decode the
// served JSON directly, so absence has to be materialized as the agreed default
// here too.
func serveable(key, value string) json.RawMessage {
	raw := json.RawMessage(value)
	switch key {
	case KeyLanguagesPolicy:
		var p LanguagesPolicy
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw // a malformed legacy row keeps its raw bytes
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	case KeyMuxOptions:
		var p MuxOptions
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	default:
		return raw
	}
}

// All returns every readable section, already stripped of non-section rows.
func (s *Store) All(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: list: %w", err)
	}
	defer rows.Close()

	out := make(map[string]json.RawMessage, len(knownKeys))
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("settings: scan: %w", err)
		}
		if !isKnownKey(key) || IsSensitiveKey(key) {
			continue
		}
		out[key] = serveable(key, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings: iterate: %w", err)
	}
	return out, nil
}

// Set validates raw against the section's schema and persists it.
func (s *Store) Set(ctx context.Context, key string, raw json.RawMessage) error {
	normalized, err := normalize(key, raw)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, normalized, time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
		return fmt.Errorf("settings: write %s: %w", key, err)
	}
	return nil
}

// Apply validates every section in the payload before writing any of them, so a
// bad section cannot leave a half-applied update behind.
func (s *Store) Apply(ctx context.Context, updates map[string]json.RawMessage) error {
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	normalized := make(map[string]string, len(keys))
	for _, key := range keys {
		v, err := normalize(key, updates[key])
		if err != nil {
			return err
		}
		normalized[key] = v
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: begin apply: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, normalized[key], now); err != nil {
			return fmt.Errorf("settings: write %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: commit apply: %w", err)
	}
	return nil
}

// Export is the sanitized configuration snapshot returned by
// GET /api/settings/export.
type Export struct {
	ExportedAt string                     `json:"exported_at"`
	Settings   map[string]json.RawMessage `json:"settings"`
}

// Export returns every non-sensitive row, with credential-looking fields masked.
// Rows whose key looks like a secret (auth.session_key, arr.api_key, .masterkey,
// password hashes, ...) are omitted entirely.
func (s *Store) Export(ctx context.Context) (Export, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return Export{}, fmt.Errorf("settings: export: %w", err)
	}
	defer rows.Close()

	out := Export{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Settings:   map[string]json.RawMessage{},
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Export{}, fmt.Errorf("settings: export scan: %w", err)
		}
		// Whitelist sections: an unknown (or future) row is not part of the
		// export contract, so it can never leak even under a benign-looking name.
		if !isKnownKey(key) || IsSensitiveKey(key) {
			continue
		}
		masked, err := maskFields(serveable(key, value))
		if err != nil {
			// A non-JSON legacy row must never leak verbatim; drop it.
			continue
		}
		out.Settings[key] = masked
	}
	if err := rows.Err(); err != nil {
		return Export{}, fmt.Errorf("settings: export iterate: %w", err)
	}
	return out, nil
}

// Errors returned by validation; API handlers map them to 400.
var (
	ErrUnknownKey   = errors.New("settings: unknown key")
	ErrProtectedKey = errors.New("settings: protected key")
	ErrInvalidKey   = errors.New("settings: invalid key")
	ErrInvalidValue = errors.New("settings: invalid value")
)

var protectedKeys = map[string]struct{}{
	"auth.session_key":   {},
	"secrets.master_key": {},
}

var secretKeyPattern = regexp.MustCompile(`(?i)(secret|passw|api[-_.]?key|token|hash|master[-_.]?key|credential|private[-_.]?key|session[-_.]?key)`)

// IsSensitiveKey reports whether a settings row must never be served.
func IsSensitiveKey(key string) bool {
	if _, ok := protectedKeys[strings.ToLower(strings.TrimSpace(key))]; ok {
		return true
	}
	return secretKeyPattern.MatchString(key)
}

// IsProtectedKey reports whether writers may ever touch the key.
func IsProtectedKey(key string) bool { return IsSensitiveKey(key) }

func isKnownKey(key string) bool {
	for _, k := range knownKeys {
		if k == key {
			return true
		}
	}
	return false
}

var iso1or3 = regexp.MustCompile(`^[a-z]{2,3}$`)

// normalize validates a section payload and returns its canonical JSON.
func normalize(key string, raw json.RawMessage) (string, error) {
	// Reject padded keys instead of trimming: Set/Apply persist the key as given,
	// so " sync.defaults " would land as an orphan row All() never returns.
	if key == "" || key != strings.TrimSpace(key) {
		return "", fmt.Errorf("%w: %q must not be empty or padded with whitespace", ErrInvalidKey, key)
	}
	if IsProtectedKey(key) {
		return "", fmt.Errorf("%w: %s", ErrProtectedKey, key)
	}
	if !isKnownKey(key) {
		return "", fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("%w: %s: empty body", ErrInvalidValue, key)
	}
	// A JSON null token (or an array/scalar) decodes into a struct as a no-op,
	// which would persist an all-zero section: nulls must never reach a decoder.
	body := strings.TrimSpace(string(raw))
	if len(body) == 0 || body[0] != '{' {
		return "", fmt.Errorf("%w: %s: section payload must be a JSON object", ErrInvalidValue, key)
	}

	var (
		v   any
		err error
	)
	switch key {
	case KeyStoragePaths:
		var p StoragePaths
		err = strict(raw, &p)
		if err == nil {
			err = validateStorage(&p)
		}
		v = p
	case KeyLanguagesPolicy:
		var p LanguagesPolicy
		err = strict(raw, &p)
		if err == nil {
			err = validateLanguages(&p)
		}
		v = p
	case KeyTargetTagFilter:
		var p TargetTagFilter
		err = strict(raw, &p)
		if err == nil {
			err = validateTagFilter(&p)
		}
		v = p
	case KeySyncDefaults:
		var p SyncDefaults
		err = strict(raw, &p)
		if err == nil {
			err = validateSync(&p)
		}
		v = p
	case KeySecurityOptions:
		var p SecurityOptions
		err = strict(raw, &p)
		v = p
	case KeyMuxOptions:
		var p MuxOptions
		err = strict(raw, &p)
		if err == nil && strings.TrimSpace(p.TrackTitleInMKV) == "" {
			err = errors.New("track_title_in_mkv is required")
		}
		if err == nil {
			err = validateMuxOptions(&p)
		}
		v = p
	case KeyWatchOptions:
		var p WatchOptions
		err = strict(raw, &p)
		if err == nil && (p.ScanIntervalSec < 1 || p.ScanIntervalSec > 3600) {
			err = errors.New("scan_interval_sec must be 1..3600")
		}
		if err == nil && p.StabilityCheckIntervalSec < 4 {
			err = errors.New("stability_check_interval_sec must be >= 4")
		}
		v = p
	case KeyArrOptions:
		var p ArrOptions
		err = strict(raw, &p)
		if err == nil && (p.PollIntervalSec < 30 || p.PollIntervalSec > 86400) {
			err = errors.New("poll_interval_sec must be 30..86400")
		}
		v = p
	case KeyArrConnections:
		var p ArrConnections
		err = strict(raw, &p)
		if err == nil {
			err = validateArrConnections(&p)
		}
		v = p
	case KeyImportsOptions:
		var p ImportsOptions
		err = strict(raw, &p)
		if err == nil {
			err = validateImports(&p)
		}
		v = p
	}
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInvalidValue, key, err)
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInvalidValue, key, err)
	}
	return string(blob), nil
}

func strict(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

func validateStorage(p *StoragePaths) error {
	if p.MediaRoots == nil {
		p.MediaRoots = []string{}
	}
	for _, path := range append([]string{p.WatchDir, p.VaultDir, p.WorkDir, p.OutputDir}, p.MediaRoots...) {
		if path != "" && !filepath.IsAbs(path) {
			return fmt.Errorf("path %q must be absolute", path)
		}
	}
	// ADR-0002: nothing is ever written into the Audio Vault. The mux worker
	// repeats this check on the EvalSymlinks-resolved paths; here a lexical
	// comparison catches the obvious misconfiguration at save time.
	if p.OutputDir != "" && p.VaultDir != "" {
		if pathWithin(filepath.Clean(p.VaultDir), filepath.Clean(p.OutputDir)) {
			return errors.New("output_dir must not be the vault folder or inside it")
		}
	}
	return nil
}

// pathWithin reports whether target is root or below it. Both paths must
// already be cleaned (and, where they exist, EvalSymlinks-resolved).
func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func validateMuxOptions(p *MuxOptions) error {
	// An absent or empty mode keeps the historical in-place behaviour; a
	// caller that sends only keep_bak must not be rejected.
	if p.OutputMode == "" {
		p.OutputMode = MuxOutputReplace
	}
	switch p.OutputMode {
	case MuxOutputReplace, MuxOutputSeparate:
		return nil
	default:
		return fmt.Errorf(`output_mode must be %q or %q`, MuxOutputReplace, MuxOutputSeparate)
	}
}

func validateLanguages(p *LanguagesPolicy) error {
	if p.TargetImportLanguages == nil {
		p.TargetImportLanguages = []string{}
	}
	if p.ExtraReferenceLanguages == nil {
		p.ExtraReferenceLanguages = []string{}
	}
	for _, code := range p.TargetImportLanguages {
		if langtoken.IsReserved(code) {
			return fmt.Errorf("target import language %q is a reserved reference token, not a language code", code)
		}
		if !iso1or3.MatchString(code) {
			return fmt.Errorf("language code %q must be a lowercase ISO 639-1/2 code", code)
		}
	}
	for i, raw := range p.ExtraReferenceLanguages {
		// Reserved tokens are canonicalized to lowercase and never validated as
		// ISO codes; anything else must be a lowercase ISO 639-1/2 code.
		if tok, ok := langtoken.Token(raw); ok {
			p.ExtraReferenceLanguages[i] = tok
			continue
		}
		if !iso1or3.MatchString(raw) {
			return fmt.Errorf("language code %q must be a lowercase ISO 639-1/2 code or a reserved token (%s, %s)",
				raw, langtoken.Org, langtoken.All)
		}
	}
	switch p.ExportMode {
	case "rule_based", "all":
	default:
		return errors.New(`export_mode must be "rule_based" or "all"`)
	}
	return nil
}

func validateTagFilter(p *TargetTagFilter) error {
	if p.RequiredFilenameTags == nil {
		p.RequiredFilenameTags = []string{}
	}
	if p.ExcludedFilenameTags == nil {
		p.ExcludedFilenameTags = []string{}
	}
	switch strings.ToUpper(p.TagMatchMode) {
	case "ALL", "ANY", "REGEX":
		p.TagMatchMode = strings.ToUpper(p.TagMatchMode)
	default:
		return errors.New(`tag_match_mode must be ALL, ANY or REGEX`)
	}
	if p.EnforceTagFilter && len(p.RequiredFilenameTags) == 0 {
		return errors.New("enforce_tag_filter requires at least one required_filename_tag")
	}
	return nil
}

func validateArrConnections(p *ArrConnections) error {
	if p.Instances == nil {
		p.Instances = []ArrConnection{}
	}
	seen := make(map[string]bool, len(p.Instances))
	for i := range p.Instances {
		inst := &p.Instances[i]
		inst.Kind = strings.ToLower(strings.TrimSpace(inst.Kind))
		switch inst.Kind {
		case "radarr", "sonarr":
		default:
			return fmt.Errorf("instance %d: kind must be radarr or sonarr", i)
		}
		if seen[inst.Kind] {
			return fmt.Errorf("instance %d: duplicate kind %q", i, inst.Kind)
		}
		seen[inst.Kind] = true
		inst.BaseURL = strings.TrimSpace(inst.BaseURL)
		u, err := url.Parse(inst.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("instance %d: base_url must be an absolute http(s) URL", i)
		}
		inst.BaseURL = strings.TrimRight(inst.BaseURL, "/")
	}
	return nil
}

func validateImports(p *ImportsOptions) error {
	if p.DonorPatterns == nil {
		p.DonorPatterns = []string{}
	}
	for _, pattern := range p.DonorPatterns {
		if strings.TrimSpace(pattern) == "" {
			return errors.New("donor_patterns entries must not be empty")
		}
	}
	return nil
}

func validateSync(p *SyncDefaults) error {
	if p.MaxOffsetSec < 0 || p.MaxOffsetSec > 3600 {
		return errors.New("max_offset_sec must be 0..3600")
	}
	if len(p.CheckpointPositionsPct) == 0 || len(p.PreviewPositionsPct) == 0 {
		return errors.New("checkpoint and preview positions must not be empty")
	}
	for _, pos := range append(append([]int{}, p.CheckpointPositionsPct...), p.PreviewPositionsPct...) {
		if pos < 0 || pos > 100 {
			return fmt.Errorf("position %d must be 0..100", pos)
		}
	}
	if p.CheckpointWindowSec < 1 || p.CheckpointWindowSec > 600 {
		return errors.New("checkpoint_window_sec must be 1..600")
	}
	if p.PreviewWindowSec < 1 || p.PreviewWindowSec > 600 {
		return errors.New("preview_window_sec must be 1..600")
	}
	return nil
}

// maskFields walks a JSON value and replaces the value of any
// credential-looking field with a masked placeholder, so an operator-supplied
// export cannot carry an embedded secret even inside a known section.
func maskFields(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	masked := maskValue("", v)
	blob, err := json.Marshal(masked)
	if err != nil {
		return nil, err
	}
	return blob, nil
}

func maskValue(key string, v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			out[k] = maskValue(k, child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = maskValue(key, child)
		}
		return out
	case string:
		if key != "" && secretKeyPattern.MatchString(key) && val != "" {
			return Mask(val)
		}
		return val
	default:
		return v
	}
}

// Mask renders a credential as a display-only placeholder, delegating to
// internal/secrets so exported values match the rest of the appliance.
func Mask(secret string) string { return secrets.Mask(secret) }
