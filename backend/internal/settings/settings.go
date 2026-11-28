// Package settings persists Dubarr's application configuration in the
// `settings` key/value table and validates every write.
//
// Layout: one row per settings section, the row value being
// the JSON object for that section. Section keys:
//
//	storage.paths      watch_dir, vault_dir, work_dir, output_dir, media_roots
//	languages.policy   target/reference languages, commentary exclusion, export mode
//	target.tag_filter  Processing Gate: filename tags + optional arr tags/roots (ADR-0005, ADR-0009)
//	sync.defaults      max offset, checkpoint/preview sampling positions, parallel checkpoints
//	security.options   tag override
//	mux.options        track title written into Matroska
//	audio.options      opt-in Transcode Rules: codec/channels/bitrate/sample rate (ADR-0010)
//	watch.options      watch-folder scan cadence and stability window
//	arr.options        Radarr/Sonarr polling cadence
//	arr.connections    Radarr/Sonarr instances (API keys live in internal/secrets)
//	vault.naming       arr-style vault file name patterns and the season folder
//
// mux/watch/arr sections are an extension of the original key list: the spec
// only names the first five, but the settled defaults for the track title,
// the 10 s watch scan and the 300 s *arr poll need a home. They live as
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
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/langtoken"
	"github.com/Subject412/dubarr/backend/internal/naming"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/transcode"
)

// Section keys.
const (
	KeyStoragePaths    = "storage.paths"
	KeyLanguagesPolicy = "languages.policy"
	KeyTargetTagFilter = "target.tag_filter"
	KeySyncDefaults    = "sync.defaults"
	KeySyncStrategy    = "sync.strategy"
	KeySecurityOptions = "security.options"
	KeyMuxOptions      = "mux.options"
	KeyWatchOptions    = "watch.options"
	KeyArrOptions      = "arr.options"
	KeyArrConnections  = "arr.connections"
	KeyImportsOptions  = "imports.options"
	KeyAudioOptions    = "audio.options"
	KeyVaultNaming     = "vault.naming"
)

var knownKeys = []string{
	KeyStoragePaths, KeyLanguagesPolicy, KeyTargetTagFilter, KeySyncDefaults,
	KeySyncStrategy, KeySecurityOptions, KeyMuxOptions, KeyWatchOptions,
	KeyArrOptions, KeyArrConnections, KeyImportsOptions, KeyAudioOptions,
	KeyVaultNaming,
}

// KnownKeys returns the writable section keys in stable order.
func KnownKeys() []string { return append([]string(nil), knownKeys...) }

// StoragePaths are the five logical storage areas. Empty values
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

// TargetTagFilter is the Processing Gate (GLOSSARY.md): the filename criterion
// from ADR-0005 plus the optional arr criterion from ADR-0009.
//
// SourceMode selects which sources are consulted ("filename", "arr" or
// "both"); the active sources are AND-ed, so adding the arr source can only
// narrow eligibility. RequiredArrTags/ExcludedArrTags are Radarr/Sonarr tag
// labels (matched case-insensitively by default) and RootFolderPaths are
// canonical arr root-folder paths. Legacy stored rows predate these fields and
// decode as source_mode=filename with empty arr criteria (see UnmarshalJSON).
type TargetTagFilter struct {
	RequiredFilenameTags []string `json:"required_filename_tags"`
	TagMatchMode         string   `json:"tag_match_mode"`
	TagCaseSensitive     bool     `json:"tag_case_sensitive"`
	ExcludedFilenameTags []string `json:"excluded_filename_tags"`
	EnforceTagFilter     bool     `json:"enforce_tag_filter"`
	// SourceMode is filename (default) | arr | both.
	SourceMode string `json:"source_mode"`
	// RequiredArrTags/ExcludedArrTags are arr tag labels; the same ANY/ALL/
	// REGEX match mode and case sensitivity apply. Excluded labels always win.
	RequiredArrTags []string `json:"required_arr_tags"`
	ExcludedArrTags []string `json:"excluded_arr_tags"`
	// RootFolderPaths is an allowlist of canonical arr root folders.
	RootFolderPaths []string `json:"root_folder_paths"`
}

// Processing Gate source modes (ADR-0009).
const (
	SourceModeFilename = "filename"
	SourceModeArr      = "arr"
	SourceModeBoth     = "both"
)

// UnmarshalJSON materialises the ADR-0009 defaults, so a section written before
// source_mode existed keeps its exact previous behaviour and Get/All/Export all
// serve the complete shape. Unknown fields still fail: the settings store
// rejects typos rather than silently dropping them.
func (t *TargetTagFilter) UnmarshalJSON(data []byte) error {
	type plain TargetTagFilter
	aux := struct {
		*plain
		SourceMode *string `json:"source_mode"`
	}{plain: (*plain)(t)}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&aux); err != nil {
		return err
	}
	if aux.SourceMode == nil {
		t.SourceMode = SourceModeFilename
	} else {
		t.SourceMode = strings.ToLower(strings.TrimSpace(*aux.SourceMode))
	}
	if t.RequiredArrTags == nil {
		t.RequiredArrTags = []string{}
	}
	if t.ExcludedArrTags == nil {
		t.ExcludedArrTags = []string{}
	}
	if t.RootFolderPaths == nil {
		t.RootFolderPaths = []string{}
	}
	return nil
}

// SyncDefaults bounds the automatic offset search and sampling windows.
type SyncDefaults struct {
	MaxOffsetSec           int   `json:"max_offset_sec"`
	CheckpointPositionsPct []int `json:"checkpoint_positions_pct"`
	CheckpointWindowSec    int   `json:"checkpoint_window_sec"`
	PreviewPositionsPct    []int `json:"preview_positions_pct"`
	PreviewWindowSec       int   `json:"preview_window_sec"`
	// ParallelCheckpoints is how many checkpoints one sync job analyses at
	// the same time (1..8; see analyze.MaxParallelCheckpoints for the engine's
	// hard bound). Absent in a row written before the key existed, which
	// [SyncDefaults.UnmarshalJSON] resolves to [DefaultParallelCheckpoints].
	ParallelCheckpoints int `json:"parallel_checkpoints"`
	// AnalysisSource is where a checkpoint's engine job gets its audio from:
	// [analyze.AnalysisSourceWindows] or [analyze.AnalysisSourceFull]. Absent
	// in a row written before the key existed, which
	// [SyncDefaults.UnmarshalJSON] resolves to [DefaultAnalysisSource].
	AnalysisSource string `json:"analysis_source"`
}

// DefaultAnalysisSource is the shipped analysis mode: each sync job analyses a
// track in 190 s windows decoded with a 2.0 s pre-roll, instead of decoding both
// sources whole. It falls back to a whole-track decode whenever a window plan
// cannot be built (unknown duration/origin, a setting that says full) or a
// window fails twice, so the shipped mode never fails where the whole-track
// path would have succeeded. [analyze.AnalysisSourceFull] stays selectable for
// operators who would rather never fall back. This is the only place in the
// backend the default is written down, so flipping it is one constant; the SPA
// mirrors it in DEFAULTS['sync.defaults'] and its select fallback.
const DefaultAnalysisSource = analyze.AnalysisSourceWindows

// DefaultParallelCheckpoints is the shipped worker count: one checkpoint at a
// time, i.e. exactly the behaviour every install had before the setting
// existed.
const DefaultParallelCheckpoints = 1

// UnmarshalJSON makes an absent parallel_checkpoints mean
// [DefaultParallelCheckpoints] and an absent analysis_source mean
// [DefaultAnalysisSource]. Legacy rows written before the keys existed must
// keep analysing one checkpoint at a time; their absent analysis_source now
// resolves to the shipped windowed mode, which falls back to a whole-track
// decode when a window plan cannot be built. A plain decode would read the
// two as 0 and "", and fail validation. A *present* 0 is kept as 0
// so validateSync rejects it: treating it as "absent" would make an operator's
// explicit 0 silently mean 1. A present-but-empty analysis_source is kept empty
// for the same reason, so "" and absent stay distinguishable at the boundary
// (validateSync accepts "" as the compiled default and normalize materialises
// it before storing).
func (s *SyncDefaults) UnmarshalJSON(data []byte) error {
	type plain SyncDefaults
	aux := struct {
		*plain
		ParallelCheckpoints *int    `json:"parallel_checkpoints"`
		AnalysisSource      *string `json:"analysis_source"`
	}{plain: (*plain)(s)}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&aux); err != nil {
		return err
	}
	if aux.ParallelCheckpoints == nil {
		s.ParallelCheckpoints = DefaultParallelCheckpoints
	} else {
		s.ParallelCheckpoints = *aux.ParallelCheckpoints
	}
	if aux.AnalysisSource == nil {
		s.AnalysisSource = DefaultAnalysisSource
	} else {
		s.AnalysisSource = *aux.AnalysisSource
	}
	return nil
}

// StrategyOptions is the sync.strategy section: which sync techniques run, in
// what order, and where each one's answer has to clear to be used (#82).
//
// It is the analyzer's [analyze.Strategy] with a settings-side decoder,
// so the JSON the UI sends, the JSON that is stored, and the value the pipeline
// runs are one schema — no translation layer to drift.
type StrategyOptions analyze.Strategy

// UnmarshalJSON materialises the shipped strategy when the section does not
// carry a list.
//
// Three cases need it. A row written before 0.5.0 has no `techniques` key at
// all; the SPA's generic section editor posts back exactly what it was served;
// and an operator who deletes the list from the advanced editor must not end up
// with a pipeline that has nothing to run. An explicit empty list is a
// *present* key and stays empty, so [validateStrategy] can reject it with the
// message the operator needs rather than silently replacing their edit.
//
// The `piecewise` block (0.5.0, #84) is decoded the same absent-tolerant way: a
// row written before the block existed — and a client that has never heard of it
// — keeps the documented default, which is "off" (D5). That is why no migration
// is needed and why there is no "exists but does nothing" flag (D1).
func (o *StrategyOptions) UnmarshalJSON(data []byte) error {
	var aux struct {
		Preset     *string                  `json:"preset"`
		Techniques *[]analyze.Technique     `json:"techniques"`
		Piecewise  *analyze.PiecewisePolicy `json:"piecewise"`
	}
	// Unknown fields are rejected here, not only by the caller's decoder: the
	// section is served back to the UI as it was stored, so a stray key
	// (`checkpoints`, a field of a different section) must fail the save.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&aux); err != nil {
		return err
	}
	strategy := analyze.DefaultStrategy()
	if aux.Preset != nil {
		strategy.Preset = *aux.Preset
	}
	if aux.Techniques != nil {
		strategy.Techniques = *aux.Techniques
	}
	if aux.Piecewise != nil {
		strategy.Piecewise = *aux.Piecewise
	}
	*o = StrategyOptions(strategy)
	return nil
}

// MarshalJSON always writes the piecewise block, at the documented numbers when
// the section does not carry them.
//
// The analyzer's own [analyze.Strategy] omits the block while it is "off at the
// shipped numbers", because every stored report embeds it and a report must not
// grow a key for a feature that never ran. The *settings section* is the
// opposite contract: it is the document the operator edits, so the UI has to be
// served the numbers it would be editing even on a fresh install.
func (o StrategyOptions) MarshalJSON() ([]byte, error) {
	type section struct {
		Preset     string                  `json:"preset"`
		Techniques []analyze.Technique     `json:"techniques"`
		Piecewise  analyze.PiecewisePolicy `json:"piecewise"`
	}
	return json.Marshal(section{
		Preset:     o.Preset,
		Techniques: o.Techniques,
		Piecewise:  o.Piecewise.Normalised(),
	})
}

// SecurityOptions are appliance-wide safety switches.
type SecurityOptions struct {
	AllowForceTagOverride bool `json:"allow_force_tag_override"`
}

// Mux output destination modes (ADR-0011).
const (
	MuxOutputReplace  = "replace"
	MuxOutputSeparate = "separate"
)

// MuxOptions control atomic muxing side effects.
type MuxOptions struct {
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

// AudioOptions is the audio.options section: an ordered, first-match-wins list
// of Transcode Rules (ADR-0010). The type lives in internal/transcode so the
// rule engine and the settings validator share one schema; an empty or absent
// list keeps the lossless-copy behaviour.
type AudioOptions = transcode.Options

// WatchOptions configure donor-folder scanning. StabilityCheckIntervalSec is
// how far apart the two size checks must be (>= 4 s per the watch contract).
type WatchOptions struct {
	ScanIntervalSec           int `json:"scan_interval_sec"`
	StabilityCheckIntervalSec int `json:"stability_check_interval_sec"`
}

// ArrOptions configure Radarr/Sonarr polling.
type ArrOptions struct {
	PollIntervalSec int `json:"poll_interval_sec"`
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

// The shipped vault naming patterns (#47). They are the arr-style names the
// settings UI's "Arr style (recommended)" preset fills in, and the values the
// plan documents byte for byte.
const (
	DefaultMovieTemplate   = "{Title} ({Year}) [{Variant} {Codec} {Channels}]"
	DefaultEpisodeTemplate = "{Title} - S{Season:00}E{Episode:00} - {Episode Title} [{Variant} {Codec} {Channels}]"
	// DefaultSeasonFolder is the season subfolder inside a show's folder. A
	// blank value means "keep episodes beside the show folder".
	DefaultSeasonFolder = "Season {Season:00}"
)

// VaultNaming is the vault.naming section: the patterns that decide the *stem*
// of an archived track inside the folder Dubarr already owns.
//
// The templates never carry the extension — every vault file ends in
// ".<iso2>.mka" (internal/naming.VaultPath appends it) — which is why
// {Language} is refused here with the documented message rather than stored and
// later contradicted by the suffix.
//
// Nothing in this section renames anything: an apply is always an explicit
// operator action against a fresh preview.
type VaultNaming struct {
	MovieTemplate   string `json:"movie_template"`
	EpisodeTemplate string `json:"episode_template"`
	// SeasonFolder is rendered per episode; blank disables the subfolder.
	SeasonFolder string `json:"season_folder"`
}

// Store reads and writes settings sections on the shared database handle.
type Store struct {
	db *sql.DB

	// hookMu guards onWrite: writes may run concurrently with the composition
	// root registering the hook, and the hook itself must run without this
	// lock held (it reads settings back).
	hookMu  sync.RWMutex
	onWrite func(keys []string)
}

// New wraps an open database handle. It does not seed; call Seed once at
// startup.
func New(db *sql.DB) *Store { return &Store{db: db} }

// OnWrite registers fn to run after every successful write (Set or Apply) with
// the section keys written. cmd/dubarr uses it to start subsystems whose
// settings became complete after boot (#53), so a wizard save takes effect
// without a restart. Passing nil removes the hook.
//
// fn runs after the write commits and without any store lock held, so it may
// read settings; it must not write them, which would recurse. It runs on the
// writing goroutine, so it must stay cheap: register it before the API starts
// serving.
func (s *Store) OnWrite(fn func(keys []string)) {
	s.hookMu.Lock()
	s.onWrite = fn
	s.hookMu.Unlock()
}

// notifyWrite runs the registered write hook, if any.
func (s *Store) notifyWrite(keys ...string) {
	s.hookMu.RLock()
	fn := s.onWrite
	s.hookMu.RUnlock()
	if fn != nil {
		fn(keys)
	}
}

// DB exposes the underlying handle (used by the API layer for browse roots).
func (s *Store) DB() *sql.DB { return s.db }

// Defaults returns the settled first-run configuration. dataDir supplies the
// default temp folder (<DATA_DIR>/temp), so the staging root resolves without
// an operator setting and never shares the data volume's top level with the
// database and logs; the other paths are deliberately empty so the wizard
// forces an explicit choice.
func Defaults(dataDir string) map[string]any {
	workDir := ""
	if dataDir != "" {
		workDir = filepath.Join(dataDir, "temp")
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
			// ADR-0012: a fresh install ships no required tag, so the gate
			// blocks nothing until the operator configures a criterion.
			RequiredFilenameTags: []string{},
			TagMatchMode:         "ANY",
			TagCaseSensitive:     false,
			ExcludedFilenameTags: []string{},
			EnforceTagFilter:     true,
			SourceMode:           SourceModeFilename,
			RequiredArrTags:      []string{},
			ExcludedArrTags:      []string{},
			RootFolderPaths:      []string{},
		},
		KeySyncDefaults: SyncDefaults{
			MaxOffsetSec:           180,
			CheckpointPositionsPct: []int{10, 25, 40, 55, 70, 85},
			CheckpointWindowSec:    150,
			PreviewPositionsPct:    []int{10, 25, 50, 75, 90},
			PreviewWindowSec:       60,
			ParallelCheckpoints:    DefaultParallelCheckpoints,
			AnalysisSource:         DefaultAnalysisSource,
		},
		KeySecurityOptions: SecurityOptions{
			AllowForceTagOverride: false,
		},
		KeyMuxOptions: MuxOptions{
			TrackTitleInMKV: "Polski Dubbing",
			OutputMode:      MuxOutputReplace,
		},
		KeyWatchOptions: WatchOptions{
			ScanIntervalSec:           10,
			StabilityCheckIntervalSec: 4,
		},
		KeyArrOptions: ArrOptions{
			PollIntervalSec: 300,
		},
		KeyArrConnections: ArrConnections{
			Instances: []ArrConnection{},
		},
		KeyImportsOptions: ImportsOptions{
			DonorPatterns: []string{"PL.DUB"},
		},
		KeyAudioOptions: AudioOptions{
			// Shipped default: no rules, no transcoding (ADR-0010).
			Rules: []transcode.Rule{},
		},
		// Shipped default: the D4 list — one waveform entry on every
		// checkpoint — so a fresh install's offsets are the ones 0.4.0
		// produced until the operator adds a technique.
		KeySyncStrategy: StrategyOptions(analyze.DefaultStrategy()),
		KeyVaultNaming: VaultNaming{
			MovieTemplate:   DefaultMovieTemplate,
			EpisodeTemplate: DefaultEpisodeTemplate,
			SeasonFolder:    DefaultSeasonFolder,
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
//
// A row that is not valid JSON is returned as stored rather than reported as
// absent: absence means "never configured", and a corrupt row must not be
// mistaken for that. Callers that decode get a real error, and the settings
// read route names the section instead of serving bytes no encoder can take.
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
// caller. It exists for languages.policy, mux.options, sync.defaults and
// target.tag_filter: legacy rows predate prefer_original_language /
// output_mode / parallel_checkpoints / analysis_source / source_mode, and the
// UI/API/export all decode the served JSON directly, so absence has to be
// materialized as the agreed default here too.
//
// It also canonicalises sections that lost a field: security.options and
// mux.options must not keep serving a retired key from a row stored before it
// was deleted.
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
		// A row written before keep_bak was retired still carries it, and
		// MuxOptions.UnmarshalJSON is strict about unknown fields, so the key
		// is dropped before decoding. Re-marshalling the current schema then
		// guarantees the API never serves a setting that no longer exists.
		// The stored row is left untouched.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return raw // a malformed legacy row keeps its raw bytes
		}
		// encoding/json matches object keys to struct fields case-insensitively,
		// so Keep_Bak/KEEP_BAK are just as "known" to the decoder as keep_bak is
		// unknown to it. Every spelling has to go, otherwise the strict decode
		// below rejects the row and it is served raw with the retired key in it.
		for name := range fields {
			if strings.EqualFold(name, "keep_bak") {
				delete(fields, name)
			}
		}
		cleaned, err := json.Marshal(fields)
		if err != nil {
			return raw
		}
		var p MuxOptions
		if err := json.Unmarshal(cleaned, &p); err != nil {
			return raw
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	case KeyTargetTagFilter:
		var p TargetTagFilter
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	case KeySyncDefaults:
		// A row written before analysis_source (and parallel_checkpoints)
		// existed carries neither key. Decoding into the current schema
		// resolves them to the compiled defaults and re-marshalling serves
		// the complete section, so the API never hands the UI a section whose
		// missing key it would then persist back as "". The stored row is
		// left untouched.
		var p SyncDefaults
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw // a malformed legacy row keeps its raw bytes
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	case KeyArrOptions:
		// A row written before the manual-refresh flag was retired still
		// carries it. Decoding into the current schema and re-marshalling
		// drops it, so the API never serves a setting that no longer exists.
		var p ArrOptions
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	case KeySecurityOptions:
		// A row written before vault_read_only was retired still carries it.
		// Decoding into the current schema and re-marshalling drops it, so the
		// API never serves a setting that no longer exists. The stored row is
		// left untouched.
		var p SecurityOptions
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	case KeyAudioOptions:
		// A legacy row without `rules` must serve an explicit empty list, so
		// Get/All/Export and the job path all agree that no transcode applies.
		var p AudioOptions
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw
		}
		blob, err := json.Marshal(p)
		if err != nil {
			return raw
		}
		return blob
	case KeySyncStrategy:
		// A row written before 0.5.0 carries neither key, and a hand-edited
		// one may carry only some of them. Decoding into the current schema
		// resolves an absent list to the shipped strategy and re-marshalling
		// serves a section that always has something to run. The stored row is
		// left untouched.
		var p StrategyOptions
		if err := json.Unmarshal(raw, &p); err != nil {
			return raw // a malformed legacy row keeps its raw bytes
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
		served := serveable(key, value)
		if !json.Valid(served) {
			// Serve the bytes as stored: absence means "never configured", and a
			// corrupt row must not be mistaken for that. The settings read route
			// names the section instead of handing an encoder these bytes.
			slog.Warn("settings: stored section is not valid JSON", "key", key)
		}
		out[key] = served
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
	s.notifyWrite(key)
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
	s.notifyWrite(keys...)
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
	// ErrWorkDirInVault reports a temp folder that is the vault root or a path
	// below it. A staged copy there would be reorganised by the next vault
	// pass, so the setting is refused instead of silently accepted.
	ErrWorkDirInVault = errors.New("settings: work_dir must not be the vault folder or inside it")
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
		// "" is accepted by validateSync as "the compiled default", but it is
		// materialised here so the stored row never carries an empty mode: a
		// client that omits the field (or sends "") must not leave the DB with
		// a row every reader then has to interpret.
		if err == nil && p.AnalysisSource == "" {
			p.AnalysisSource = DefaultAnalysisSource
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
	case KeyAudioOptions:
		var p AudioOptions
		err = strict(raw, &p)
		if err == nil {
			err = transcode.ValidateOptions(&p)
		}
		v = p
	case KeySyncStrategy:
		var p StrategyOptions
		err = strict(raw, &p)
		if err == nil {
			err = validateStrategy(&p)
		}
		v = p
	case KeyVaultNaming:
		var p VaultNaming
		err = strict(raw, &p)
		if err == nil {
			err = validateVaultNaming(&p)
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
	// The temp folder is written constantly while a job runs. Inside the vault
	// those staged copies would be reorganised by the next vault pass and the
	// job would read a path that no longer exists, so it is refused here too.
	if p.WorkDir != "" && p.VaultDir != "" {
		if err := CheckWorkDirOutsideVault(p.WorkDir, p.VaultDir); err != nil {
			return err
		}
	}
	return nil
}

// CheckWorkDirOutsideVault reports ErrWorkDirInVault when workDir is the vault
// root or a path below it. The comparison is lexical on cleaned paths, so a
// temp folder that does not exist yet is still checked; boot calls it before
// creating anything so a bad mount cannot stage files into the vault.
func CheckWorkDirOutsideVault(workDir, vaultDir string) error {
	workDir, vaultDir = strings.TrimSpace(workDir), strings.TrimSpace(vaultDir)
	if workDir == "" || vaultDir == "" {
		return nil
	}
	if pathWithin(filepath.Clean(vaultDir), filepath.Clean(workDir)) {
		return fmt.Errorf("%w: %s", ErrWorkDirInVault, workDir)
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
	// An absent or empty mode keeps the historical in-place behaviour.
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
	// SourceMode is caught here as well as in UnmarshalJSON: Set/Apply accept a
	// decoded struct from any caller, so the check cannot live only in JSON.
	if p.SourceMode == "" {
		p.SourceMode = SourceModeFilename
	}
	p.SourceMode = strings.ToLower(strings.TrimSpace(p.SourceMode))
	switch p.SourceMode {
	case SourceModeFilename, SourceModeArr, SourceModeBoth:
	default:
		return fmt.Errorf(`source_mode must be %q, %q or %q`, SourceModeFilename, SourceModeArr, SourceModeBoth)
	}
	switch strings.ToUpper(p.TagMatchMode) {
	case "ALL", "ANY", "REGEX":
		p.TagMatchMode = strings.ToUpper(p.TagMatchMode)
	default:
		return errors.New(`tag_match_mode must be ALL, ANY or REGEX`)
	}
	if p.RequiredArrTags == nil {
		p.RequiredArrTags = []string{}
	}
	if p.ExcludedArrTags == nil {
		p.ExcludedArrTags = []string{}
	}
	for _, list := range []struct {
		name  string
		items []string
	}{
		{"required_arr_tags", p.RequiredArrTags},
		{"excluded_arr_tags", p.ExcludedArrTags},
	} {
		for i := range list.items {
			label := strings.TrimSpace(list.items[i])
			if label == "" {
				return fmt.Errorf("%s entries must not be empty", list.name)
			}
			// Reference-language tokens (org/all) belong to languages.policy;
			// they are not arr tag labels and would silently never match.
			if langtoken.IsReserved(label) {
				return fmt.Errorf("%s entry %q is a reference-language token, not an arr tag label", list.name, label)
			}
			list.items[i] = label
		}
	}
	if p.RootFolderPaths == nil {
		p.RootFolderPaths = []string{}
	}
	roots := make([]string, 0, len(p.RootFolderPaths))
	seenRoots := make(map[string]bool, len(p.RootFolderPaths))
	for _, raw := range p.RootFolderPaths {
		root, err := canonicalRootFolderPath(raw)
		if err != nil {
			return err
		}
		if seenRoots[root] {
			continue
		}
		seenRoots[root] = true
		roots = append(roots, root)
	}
	p.RootFolderPaths = roots
	// An enforced gate may legitimately have no criterion at all: ADR-0012
	// ships exactly that, and the gate treats an empty criterion as no-op.
	return nil
}

// canonicalRootFolderPath confines a configured root folder to a canonical
// absolute path. Radarr/Sonarr report absolute paths, so a relative or
// traversal-shaped entry can never match a cached root and is rejected instead
// of silently disabling the root criterion.
func canonicalRootFolderPath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", errors.New("root_folder_paths entries must not be empty")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("root folder path %q contains a NUL byte", raw)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("root folder path %q must be absolute", raw)
	}
	return filepath.Clean(path), nil
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

// validateVaultNaming is the one gate a naming pattern must pass, and it is the
// same gate every other invalid setting passes: a pattern that fails
// naming.Validate is refused here, with the section and field named, and the
// stored row is left untouched.
//
// Trimming is the only rewriting performed: a blank season folder is a
// meaningful value ("no subfolder"), so it is preserved as blank rather than
// defaulted, while the two file patterns must be present and render something.
func validateVaultNaming(p *VaultNaming) error {
	p.MovieTemplate = strings.TrimSpace(p.MovieTemplate)
	p.EpisodeTemplate = strings.TrimSpace(p.EpisodeTemplate)
	p.SeasonFolder = strings.TrimSpace(p.SeasonFolder)

	if err := naming.Validate(p.MovieTemplate); err != nil {
		return fmt.Errorf("movie_template: %w", err)
	}
	if err := naming.Validate(p.EpisodeTemplate); err != nil {
		return fmt.Errorf("episode_template: %w", err)
	}
	if p.SeasonFolder == "" {
		// Blank = "keep episodes beside the show folder".
		return nil
	}
	// A season folder is one directory name, never a path: a separator here
	// would either escape the media folder or be silently flattened by the
	// cleaner, and both are surprises an operator should not get on save.
	if strings.ContainsAny(p.SeasonFolder, `/\`) {
		return errors.New("season_folder must be a single folder name, not a path")
	}
	if err := naming.Validate(p.SeasonFolder); err != nil {
		return fmt.Errorf("season_folder: %w", err)
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
	// A present 0 is rejected rather than read as "absent": UnmarshalJSON only
	// defaults a value the row does not carry, and an operator who typed 0 must
	// be told it is out of range instead of silently getting 1.
	if p.ParallelCheckpoints < 1 || p.ParallelCheckpoints > 8 {
		return errors.New("parallel_checkpoints must be 1..8")
	}
	// "" is the compiled default, not an error: a legacy row that reaches here
	// without the key (absent resolves in UnmarshalJSON) or a client that sends
	// an empty mode keeps working. Everything else must be one of the two
	// values, and case is NOT normalised: "Windows" is a typo the operator has
	// to fix, because silently lowercasing it would hide a broken client.
	switch p.AnalysisSource {
	case "", analyze.AnalysisSourceWindows, analyze.AnalysisSourceFull:
		return nil
	default:
		return errors.New("analysis_source must be windows or full")
	}
}

// Preset is the one place a preset's contents exist. The server never expands a
// preset into a section at run time and the UI must not either: both read it
// from here, and a settings test asserts the SPA's
// DEFAULT_SETTINGS['sync.strategy'] is this value, so the two cannot drift.
//
// It returns the *whole* section — techniques and the piecewise block — because
// a preset is one statement about the job. Keeping the two halves in separate
// functions is what would let a preset mean two different things in two places,
// which is why this replaced PresetTechniques rather than sitting beside it.
//
// No preset switches segmentation on (#84). Piecewise is the one setting that
// changes what a job *does* to the operator's dub copy — a re-cut file encoded
// once, instead of a mux with an offset — so it is only ever on because someone
// said so, never because a stored preset was redefined under them. A section
// that enables it is therefore `custom`, which is the one label that cannot be
// mistaken for "nothing changed" (D5).
//
// `custom` has no canonical section — it is what every other section is called —
// and returns the zero Strategy.
//
// D4a: `fast` keeps the waveform matcher and spends fewer checkpoints instead
// (3 rather than 6, a sync.defaults decision), so its list is `balanced`'s.
// MFCC is never a preset: its hop error is only acceptable once an operator
// opts in.
//
// The σ gate is per technique. The waveform entry is gated by it — 5σ is the
// shipped "a human should look" line on the sample correlator's curve. The
// `maximum_accuracy` mfcc entry is not: its z lives on a different curve (a few
// thousand overlapping hop lags against ~8·10⁴ near-independent samples), so a
// correct answer scores 5.3 where the waveform scores 21-28 on the same pair and
// the 8σ band refused the answers MFCC exists for. That entry carries
// `min_match_z: 0` and is judged on quality alone; the z is still recorded in
// the attempt, as a diagnostic.
func Preset(name string) analyze.Strategy {
	switch name {
	case analyze.PresetFast:
		return Preset(analyze.PresetBalanced)
	case analyze.PresetBalanced:
		// A copy: the caller may edit the section it gets back (the editor
		// does), and the shipped strategy must not change with it.
		shipped := analyze.DefaultStrategy()
		shipped.Techniques = append([]analyze.Technique(nil), shipped.Techniques...)
		return shipped
	case analyze.PresetMaximumAccuracy:
		return analyze.Strategy{
			Preset: analyze.PresetMaximumAccuracy,
			Techniques: []analyze.Technique{
				{
					ID:            analyze.TechniqueWaveform,
					Enabled:       true,
					When:          analyze.WhenAlways,
					MinConfidence: analyze.LowQualityThreshold,
					MinMatchZ:     analyze.DefaultThresholds.MatchZMaybe,
					ZGate:         true,
				},
				{
					// A second opinion only where the waveform answer was weak:
					// every checkpoint would pay a decode for no gain.
					ID:            analyze.TechniqueMFCC,
					Enabled:       true,
					When:          analyze.WhenBelowConfidence,
					MinConfidence: 0.15,
					MinMatchZ:     0,
					ZGate:         false,
				},
			},
			// Off, like every preset: see above.
			Piecewise: analyze.DefaultPiecewisePolicy(),
		}
	default:
		return analyze.Strategy{}
	}
}

// validateStrategy enforces the #82 table and the #84 piecewise ranges on the
// way in, and stores the preset name the section actually is.
//
// The label is only believed where it matches its own canonical section: a
// client that posts preset "maximum accuracy" with the balanced list — or with
// segmentation switched off — gets "custom" stored, never "balanced", and a
// hand-edited section is "custom" too. Only an unknown label and an unsound
// section are errors.
//
// The piecewise numbers are materialised before validation, so an absent block
// is stored as the documented defaults (off, 45/60/250/120/12) rather than as
// zeros the UI would have to guess at.
func validateStrategy(p *StrategyOptions) error {
	s := analyze.Strategy(*p)
	s.Piecewise = s.Piecewise.Normalised()
	s.Preset = resolvePreset(s)
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%s: %w", KeySyncStrategy, err)
	}
	*p = StrategyOptions(s)
	return nil
}

// resolvePreset answers "which name is this section?".
//
// A known label whose section is that label's own canonical section is kept —
// `fast` and `balanced` share a section, so the label is the only thing that
// tells them apart, and it is what the operator picked. A known label whose
// section differs is `custom`: an edited preset is never silently promoted to a
// different preset, even when the edit happens to reproduce that preset's
// section. An empty label is derived from the section (balanced, then
// maximum_accuracy), and an unknown label is passed through untouched so
// [analyze.Strategy.Validate] rejects it instead of quietly renaming the
// operator's choice.
func resolvePreset(s analyze.Strategy) string {
	switch s.Preset {
	case analyze.PresetFast, analyze.PresetBalanced, analyze.PresetMaximumAccuracy:
		if sameStrategy(s, Preset(s.Preset)) {
			return s.Preset
		}
		return analyze.PresetCustom
	case analyze.PresetCustom:
		return analyze.PresetCustom
	case "":
	default:
		return s.Preset
	}
	for _, name := range []string{analyze.PresetBalanced, analyze.PresetMaximumAccuracy} {
		if sameStrategy(s, Preset(name)) {
			return name
		}
	}
	return analyze.PresetCustom
}

// sameStrategy compares two sections field by field, ignoring the label itself.
// Both sides come from the same JSON schema, so exact equality is the right
// test: a threshold the operator changed by one thousandth is a different
// strategy, and a segmentation knob that differs is a different preset.
func sameStrategy(a, b analyze.Strategy) bool {
	return sameTechniques(a.Techniques, b.Techniques) && a.Piecewise == b.Piecewise
}

// sameTechniques compares two lists element for element. Both sides come from
// the same JSON schema, so exact equality is the right test: a threshold the
// operator changed by one thousandth is a different strategy.
func sameTechniques(a, b []analyze.Technique) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
