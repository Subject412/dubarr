package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/store"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st.DB)
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return v
}

func TestSeedDefaults(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	dir := t.TempDir()
	if err := s.Seed(ctx, dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, k := range KnownKeys() {
		if _, ok := all[k]; !ok {
			t.Fatalf("missing seeded key %q", k)
		}
	}

	tf := decode[TargetTagFilter](t, all[KeyTargetTagFilter])
	if len(tf.RequiredFilenameTags) != 0 {
		t.Errorf("required tags = %v, want empty (ADR 0012)", tf.RequiredFilenameTags)
	}
	if tf.SourceMode != "filename" || tf.TagMatchMode != "ANY" || tf.TagCaseSensitive || !tf.EnforceTagFilter {
		t.Errorf("tag filter = %+v", tf)
	}
	if tf.ExcludedFilenameTags == nil {
		t.Errorf("excluded tags should be an empty slice, not null")
	}

	lp := decode[LanguagesPolicy](t, all[KeyLanguagesPolicy])
	if len(lp.TargetImportLanguages) != 1 || lp.TargetImportLanguages[0] != "pl" {
		t.Errorf("target languages = %v", lp.TargetImportLanguages)
	}
	if len(lp.ExtraReferenceLanguages) != 1 || lp.ExtraReferenceLanguages[0] != "en" {
		t.Errorf("reference languages = %v", lp.ExtraReferenceLanguages)
	}
	if !lp.ExcludeCommentary || !lp.OriginalLanguageFromArr || !lp.PreferOriginalLanguage || lp.ExportMode != "rule_based" {
		t.Errorf("languages policy = %+v", lp)
	}

	sec := decode[SecurityOptions](t, all[KeySecurityOptions])
	if !sec.VaultReadOnly || sec.AllowForceTagOverride || !sec.RequireAV1Warning || sec.RequireAV1Strict {
		t.Errorf("security options = %+v", sec)
	}

	mux := decode[MuxOptions](t, all[KeyMuxOptions])
	if !mux.KeepBak || mux.TrackTitleInMKV != "Polski Dubbing" {
		t.Errorf("mux options = %+v", mux)
	}

	sd := decode[SyncDefaults](t, all[KeySyncDefaults])
	if sd.MaxOffsetSec != 180 || sd.CheckpointWindowSec != 150 || sd.PreviewWindowSec != 60 {
		t.Errorf("sync defaults = %+v", sd)
	}
	if len(sd.CheckpointPositionsPct) != 6 || sd.CheckpointPositionsPct[0] != 10 || sd.CheckpointPositionsPct[5] != 85 {
		t.Errorf("checkpoint positions = %v", sd.CheckpointPositionsPct)
	}
	if len(sd.PreviewPositionsPct) != 5 || sd.PreviewPositionsPct[4] != 90 {
		t.Errorf("preview positions = %v", sd.PreviewPositionsPct)
	}

	sp := decode[StoragePaths](t, all[KeyStoragePaths])
	if sp.WorkDir != filepath.Join(dir, "work") {
		t.Errorf("work dir = %q, want %q", sp.WorkDir, filepath.Join(dir, "work"))
	}
	if sp.MediaRoots == nil {
		t.Errorf("media_roots should be an empty slice, not null")
	}

	w := decode[WatchOptions](t, all[KeyWatchOptions])
	if w.ScanIntervalSec != 10 || w.StabilityCheckIntervalSec < 4 {
		t.Errorf("watch options = %+v", w)
	}
	a := decode[ArrOptions](t, all[KeyArrOptions])
	if a.PollIntervalSec != 300 || !a.ManualSync {
		t.Errorf("arr options = %+v", a)
	}
}

func TestSeedIsIdempotentAndKeepsUserEdits(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	dir := t.TempDir()
	if err := s.Seed(ctx, dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	edited := `{"required_filename_tags":["[CUSTOM]"],"tag_match_mode":"ALL","tag_case_sensitive":true,"excluded_filename_tags":["[X]"],"enforce_tag_filter":false}`
	if err := s.Set(ctx, KeyTargetTagFilter, json.RawMessage(edited)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Seed(ctx, dir); err != nil {
		t.Fatalf("re-Seed: %v", err)
	}
	raw, ok, err := s.Get(ctx, KeyTargetTagFilter)
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%v", err, ok)
	}
	if got := decode[TargetTagFilter](t, raw); got.TagMatchMode != "ALL" || !got.TagCaseSensitive {
		t.Fatalf("seed overwrote user edit: %+v", got)
	}
}

func TestRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	in := `{"target_import_languages":["pl","cs"],"extra_reference_languages":["en","de"],"original_language_from_arr":false,"exclude_commentary":false,"export_mode":"all"}`
	if err := s.Set(ctx, KeyLanguagesPolicy, json.RawMessage(in)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out, ok, err := s.Get(ctx, KeyLanguagesPolicy)
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%v", err, ok)
	}
	got := decode[LanguagesPolicy](t, out)
	if len(got.TargetImportLanguages) != 2 || got.TargetImportLanguages[1] != "cs" || got.ExportMode != "all" || got.ExcludeCommentary {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestLanguagesPolicyNormalizesReferenceTokens(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	in := `{"target_import_languages":["pl"],"extra_reference_languages":["ORG","all","en"],"original_language_from_arr":true,"prefer_original_language":true,"exclude_commentary":true,"export_mode":"rule_based"}`
	if err := s.Set(ctx, KeyLanguagesPolicy, json.RawMessage(in)); err != nil {
		t.Fatalf("Set with reference tokens: %v", err)
	}
	got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy))
	want := []string{"org", "all", "en"}
	if len(got.ExtraReferenceLanguages) != len(want) {
		t.Fatalf("reference languages = %v, want %v", got.ExtraReferenceLanguages, want)
	}
	for i := range want {
		if got.ExtraReferenceLanguages[i] != want[i] {
			t.Fatalf("reference languages = %v, want %v (tokens canonical lowercase)", got.ExtraReferenceLanguages, want)
		}
	}
	if !got.PreferOriginalLanguage {
		t.Error("PreferOriginalLanguage = false, want true")
	}
}

func TestLanguagesPolicyRejectsReferenceTokensAsTargets(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	for _, token := range []string{"org", "all"} {
		in := fmt.Sprintf(`{"target_import_languages":[%q],"extra_reference_languages":[],"original_language_from_arr":true,"exclude_commentary":true,"export_mode":"rule_based"}`, token)
		if err := s.Set(ctx, KeyLanguagesPolicy, json.RawMessage(in)); err == nil {
			t.Errorf("Set accepted %q as a target import language", token)
		}
	}
}

func TestSetRejectsUnknownAndProtectedKeys(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	for _, key := range []string{
		"nope.unknown",
		"auth.session_key",
		"secrets.master_key",
		"arr.api_key",
		"user.password_hash",
	} {
		if err := s.Set(ctx, key, json.RawMessage(`{}`)); err == nil {
			t.Errorf("Set(%q) accepted a disallowed key", key)
		}
	}
}

func TestSetRejectsInvalidValues(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cases := map[string]string{
		KeyTargetTagFilter: `{"tag_match_mode":"SOMETIMES"}`,
		KeyLanguagesPolicy: `{"export_mode":"whatever"}`,
		KeySyncDefaults:    `{"max_offset_sec":-5}`,
		KeyWatchOptions:    `{"scan_interval_sec":0}`,
		KeySecurityOptions: `{"vault_read_only":"yes"}`,
		KeyStoragePaths:    `{"media_roots":"not-a-list"}`,
		KeyMuxOptions:      `{"keep_bak":true,"track_title_in_mkv":""}`,
	}
	for key, body := range cases {
		if err := s.Set(ctx, key, json.RawMessage(body)); err == nil {
			t.Errorf("Set(%q, %s) accepted invalid value", key, body)
		}
	}
	if err := s.Set(ctx, KeyTargetTagFilter,
		json.RawMessage(`{"required_filename_tags":["x"],"bogus":1}`)); err == nil {
		t.Error("Set accepted an unknown field inside target.tag_filter")
	}
}

func TestExportExcludesSecrets(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for key, val := range map[string]string{
		"auth.session_key":  `"c2VjcmV0LXNlc3Npb24ta2V5"`,
		"secrets.masterkey": `"bWFzdGVyLWtleQ=="`,
		"arr.api_key":       `"abcdef1234567890"`,
		"user.password":     `"hunter2hunter2"`,
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, val); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES ('plugin.config', '{"name":"x","password":"hunter2"}')`); err != nil {
		t.Fatalf("insert nested secret: %v", err)
	}

	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	blob, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	text := string(blob)
	for _, banned := range []string{"auth.session_key", "secrets.masterkey", "arr.api_key", "user.password",
		"c2VjcmV0LXNlc3Npb24ta2V5", "bWFzdGVyLWtleQ==", "hunter2"} {
		if strings.Contains(text, banned) {
			t.Errorf("export leaked %q: %s", banned, text)
		}
	}
	if _, ok := exp.Settings[KeyTargetTagFilter]; !ok {
		t.Errorf("export dropped a normal setting")
	}
}

func mustGet(t *testing.T, s *Store, ctx context.Context, key string) json.RawMessage {
	t.Helper()
	raw, ok, err := s.Get(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Get(%s): %v ok=%v", key, err, ok)
	}
	return raw
}

// A null JSON token decodes into a struct as a no-op, so without an explicit
// object check it would persist an all-zero section: security.options would lose
// vault_read_only/require_av1_warning and storage.paths would lose its roots.
func TestSetRejectsNullAndNonObjectSections(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	dir := t.TempDir()
	if err := s.Seed(ctx, dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, key := range KnownKeys() {
		for _, body := range []string{"null", " []", `"text"`, "true", "42", "null "} {
			if err := s.Set(ctx, key, json.RawMessage(body)); err == nil {
				t.Errorf("Set(%q, %s) accepted a non-object section payload", key, body)
			}
		}
	}

	sec := decode[SecurityOptions](t, mustGet(t, s, ctx, KeySecurityOptions))
	if !sec.VaultReadOnly || !sec.RequireAV1Warning || sec.AllowForceTagOverride || sec.RequireAV1Strict {
		t.Errorf("security.options changed by a rejected write: %+v", sec)
	}
	sp := decode[StoragePaths](t, mustGet(t, s, ctx, KeyStoragePaths))
	if sp.WorkDir != filepath.Join(dir, "work") || len(sp.MediaRoots) != 0 {
		t.Errorf("storage.paths changed by a rejected write: %+v", sp)
	}
	if sp.MediaRoots == nil {
		t.Error("media_roots became null")
	}
}

// Keys are validated after trimming, but must be persisted exactly as the
// caller supplied them; a padded key would create an orphan row All() never
// returns.
func TestSetRejectsWhitespaceKeys(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	payload := json.RawMessage(`{"max_offset_sec":240,"checkpoint_positions_pct":[10],"checkpoint_window_sec":150,"preview_positions_pct":[10],"preview_window_sec":60}`)
	for _, key := range []string{" " + KeySyncDefaults, KeySyncDefaults + " ", "	" + KeySyncDefaults + "\n", ""} {
		if err := s.Set(ctx, key, payload); err == nil {
			t.Errorf("Set(%q) accepted a key with surrounding whitespace", key)
		}
	}
	if err := s.Apply(ctx, map[string]json.RawMessage{" " + KeySyncDefaults + " ": payload}); err == nil {
		t.Error("Apply accepted a key with surrounding whitespace")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key LIKE '%sync.defaults%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows matching sync.defaults = %d, want only the seeded row", n)
	}
}

// Export is the last line of defence: only whitelisted section keys may leave
// the appliance, so an unknown future row cannot leak even if its name does not
// look like a credential.
func TestExportWhitelistsKnownKeys(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for key, val := range map[string]string{
		"plugin.config":  `{"name":"x","endpoint":"http://example.invalid"}`,
		"future.setting": `{"label":"plain"}`,
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, val); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, key := range []string{"plugin.config", "future.setting"} {
		if _, ok := exp.Settings[key]; ok {
			t.Errorf("export included non-section key %q", key)
		}
	}
	if _, ok := exp.Settings[KeyMuxOptions]; !ok {
		t.Error("export dropped a whitelisted section")
	}
}

// legacyLanguagesPolicyJSON is a row written before prefer_original_language
// existed. Absence must mean the agreed upgrade default (true), not Go's zero
// value: Seed uses INSERT OR IGNORE and never backfills the row.
const legacyLanguagesPolicyJSON = `{"target_import_languages":["pl"],"extra_reference_languages":["en","all"],"original_language_from_arr":true,"exclude_commentary":true,"export_mode":"rule_based"}`

func insertLegacyLanguagesPolicy(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		KeyLanguagesPolicy, legacyLanguagesPolicyJSON); err != nil {
		t.Fatalf("insert legacy languages.policy: %v", err)
	}
}

func TestLegacyLanguagesPolicyDefaultsPreferOriginalLanguage(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	insertLegacyLanguagesPolicy(t, s)
	// Seed is a no-op for an existing row; it must not be the thing that fixes
	// the value.
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	if got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy)); !got.PreferOriginalLanguage {
		t.Error("Get: PreferOriginalLanguage = false for a legacy row, want true")
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := decode[LanguagesPolicy](t, all[KeyLanguagesPolicy]); !got.PreferOriginalLanguage {
		t.Error("All (API payload): PreferOriginalLanguage = false for a legacy row, want true")
	}
	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := decode[LanguagesPolicy](t, exp.Settings[KeyLanguagesPolicy]); !got.PreferOriginalLanguage {
		t.Error("Export: PreferOriginalLanguage = false for a legacy row, want true")
	}

	// Set with the same legacy-shaped payload: an omitted key still means true.
	if err := s.Set(ctx, KeyLanguagesPolicy, json.RawMessage(legacyLanguagesPolicyJSON)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy)); !got.PreferOriginalLanguage {
		t.Error("Set/Get: PreferOriginalLanguage = false after a keyless write, want true")
	}
}

func TestLanguagesPolicyExplicitFalseSurvivesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	in := `{"target_import_languages":["pl"],"extra_reference_languages":["en"],"original_language_from_arr":true,"prefer_original_language":false,"exclude_commentary":true,"export_mode":"rule_based"}`
	if err := s.Set(ctx, KeyLanguagesPolicy, json.RawMessage(in)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy)); got.PreferOriginalLanguage {
		t.Error("explicit prefer_original_language=false read back as true")
	}
}

// TestStorageOutputDirValidation pins the settings-API side of the
// storage.paths.output_dir guard (ADR-0011): absolute only, and never the vault
// or a directory inside it (ADR-0002). The mux worker repeats the check on the
// EvalSymlinks-resolved paths.
func TestStorageOutputDirValidation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	cases := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{name: "sibling root", dir: "/outputs", wantErr: false},
		{name: "empty", dir: "", wantErr: false},
		{name: "relative", dir: "outputs", wantErr: true},
		{name: "vault itself", dir: "/vault", wantErr: true},
		{name: "inside vault", dir: "/vault/mux", wantErr: true},
		{name: "vault with trailing slash", dir: "/vault/", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(StoragePaths{
				WatchDir: "/watch", VaultDir: "/vault", WorkDir: "/work",
				OutputDir: tc.dir, MediaRoots: []string{},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = s.Set(ctx, KeyStoragePaths, raw)
			if tc.wantErr && err == nil {
				t.Fatalf("Set accepted output_dir %q", tc.dir)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Set(%q): %v", tc.dir, err)
			}
		})
	}
}

// TestMuxOutputMode covers the mode default and validation: legacy rows are
// served as "replace", both real modes round-trip and anything else is a 400.
func TestMuxOutputMode(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if got := decode[MuxOptions](t, mustGet(t, s, ctx, KeyMuxOptions)); got.OutputMode != MuxOutputReplace {
		t.Fatalf("seeded output_mode = %q, want %q", got.OutputMode, MuxOutputReplace)
	}

	for _, mode := range []string{MuxOutputReplace, MuxOutputSeparate} {
		raw := json.RawMessage(`{"keep_bak":true,"track_title_in_mkv":"Polski Dubbing","output_mode":"` + mode + `"}`)
		if err := s.Set(ctx, KeyMuxOptions, raw); err != nil {
			t.Fatalf("Set(%s): %v", mode, err)
		}
		if got := decode[MuxOptions](t, mustGet(t, s, ctx, KeyMuxOptions)); got.OutputMode != mode {
			t.Errorf("stored output_mode = %q, want %q", got.OutputMode, mode)
		}
	}

	bad := json.RawMessage(`{"keep_bak":true,"track_title_in_mkv":"Polski Dubbing","output_mode":"elsewhere"}`)
	if err := s.Set(ctx, KeyMuxOptions, bad); err == nil {
		t.Fatal("Set accepted an unknown output_mode")
	}

	// A row written before the field existed must read back as replace, both
	// from All (serveable) and from a direct decode.
	legacy := `{"keep_bak":true,"track_title_in_mkv":"Polski Dubbing"}`
	if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, legacy, KeyMuxOptions); err != nil {
		t.Fatalf("write legacy row: %v", err)
	}
	if got := decode[MuxOptions](t, mustGet(t, s, ctx, KeyMuxOptions)); got.OutputMode != MuxOutputReplace {
		t.Errorf("legacy output_mode = %q, want %q", got.OutputMode, MuxOutputReplace)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := decode[MuxOptions](t, all[KeyMuxOptions]); got.OutputMode != MuxOutputReplace {
		t.Errorf("served legacy output_mode = %q, want %q", got.OutputMode, MuxOutputReplace)
	}
}

// TestSeedTargetTagFilterNeutralDefault pins ADR 0012: a fresh install ships
// an empty required-filename-tag list, so the gate blocks nothing until the
// operator configures a criterion.
func TestSeedTargetTagFilterNeutralDefault(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	tf := decode[TargetTagFilter](t, mustGet(t, s, ctx, KeyTargetTagFilter))
	if len(tf.RequiredFilenameTags) != 0 {
		t.Errorf("required tags = %v, want empty", tf.RequiredFilenameTags)
	}
	if tf.SourceMode != "filename" {
		t.Errorf("source_mode = %q, want filename", tf.SourceMode)
	}
	if len(tf.RequiredArrTags) != 0 || len(tf.ExcludedArrTags) != 0 || len(tf.RootFolderPaths) != 0 {
		t.Errorf("arr criteria = %+v, want empty", tf)
	}
	// Enforce stays on; it only has an effect once a criterion exists.
	if !tf.EnforceTagFilter || tf.TagMatchMode != "ANY" || tf.ExcludedFilenameTags == nil {
		t.Errorf("tag filter = %+v", tf)
	}
}

// legacyTagFilter is a target.tag_filter row as written before ADR-0009 added
// source_mode and the arr criteria.
const legacyTagFilter = `{"required_filename_tags":["[CUSTOM]"],"tag_match_mode":"ALL","tag_case_sensitive":true,"excluded_filename_tags":["[X]"],"enforce_tag_filter":true}`

func insertLegacyTagFilter(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
		KeyTargetTagFilter, legacyTagFilter, "2026-01-01 00:00:00"); err != nil {
		t.Fatalf("insert legacy tag filter: %v", err)
	}
}

// TestLegacyTagFilterMaterializesDefaults is the regression this slice exists
// for: an existing stored section has no source_mode/arr fields, and Get, All
// and Export must all serve it as filename-only with empty arr criteria.
func TestLegacyTagFilterMaterializesDefaults(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	insertLegacyTagFilter(t, s)

	check := func(where string, raw json.RawMessage) {
		t.Helper()
		tf := decode[TargetTagFilter](t, raw)
		if tf.SourceMode != "filename" {
			t.Errorf("%s source_mode = %q, want filename", where, tf.SourceMode)
		}
		if tf.RequiredArrTags == nil || len(tf.RequiredArrTags) != 0 ||
			tf.ExcludedArrTags == nil || len(tf.ExcludedArrTags) != 0 ||
			tf.RootFolderPaths == nil || len(tf.RootFolderPaths) != 0 {
			t.Errorf("%s arr criteria = %+v, want empty slices", where, tf)
		}
		if len(tf.RequiredFilenameTags) != 1 || tf.RequiredFilenameTags[0] != "[CUSTOM]" {
			t.Errorf("%s required tags = %v, want the stored [CUSTOM]", where, tf.RequiredFilenameTags)
		}
	}

	check("Get", mustGet(t, s, ctx, KeyTargetTagFilter))
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	check("All", all[KeyTargetTagFilter])
	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	check("Export", exp.Settings[KeyTargetTagFilter])

	// Seeding an existing install must not rewrite the stored row.
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, KeyTargetTagFilter).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != legacyTagFilter {
		t.Errorf("stored row = %s, want untouched legacy value", stored)
	}
}

// TestTargetTagFilterValidation covers the new write-time checks: a known
// source mode, non-empty arr tag labels, canonical absolute root paths, and an
// enforced gate that may legitimately have no filename tags when an arr
// criterion exists.
func TestTargetTagFilterValidation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	valid := `{"source_mode":"both","required_filename_tags":[],"tag_match_mode":"ANY","tag_case_sensitive":false,"excluded_filename_tags":[],"enforce_tag_filter":true,"required_arr_tags":["dub"],"excluded_arr_tags":[],"root_folder_paths":["/media/movies/","/media/tv"]}`
	if err := s.Set(ctx, KeyTargetTagFilter, json.RawMessage(valid)); err != nil {
		t.Fatalf("Set(valid arr criteria): %v", err)
	}
	tf := decode[TargetTagFilter](t, mustGet(t, s, ctx, KeyTargetTagFilter))
	if tf.SourceMode != "both" {
		t.Errorf("source_mode = %q, want both", tf.SourceMode)
	}
	if len(tf.RootFolderPaths) != 2 || tf.RootFolderPaths[0] != "/media/movies" {
		t.Errorf("root paths = %v, want canonicalized /media/movies", tf.RootFolderPaths)
	}

	// A language-looking label is a tag label, not a language token; the gate
	// section must not apply target-language rules here.
	if err := s.Set(ctx, KeyTargetTagFilter,
		json.RawMessage(`{"source_mode":"arr","tag_match_mode":"ANY","enforce_tag_filter":true,"required_arr_tags":["pl"]}`)); err != nil {
		t.Errorf("Set(arr tag label 'pl'): %v", err)
	}

	bad := map[string]string{
		"unknown source mode":                `{"source_mode":"filename_only","tag_match_mode":"ANY","enforce_tag_filter":true,"required_filename_tags":["x"]}`,
		"blank arr tag":                      `{"source_mode":"arr","tag_match_mode":"ANY","required_arr_tags":["  "]}`,
		"blank excluded arr tag":             `{"source_mode":"arr","tag_match_mode":"ANY","excluded_arr_tags":[""]}`,
		"relative root path":                 `{"source_mode":"arr","tag_match_mode":"ANY","root_folder_paths":["media/movies"]}`,
		"traversal root path":                `{"source_mode":"arr","tag_match_mode":"ANY","root_folder_paths":["../media/movies"]}`,
		"reserved token as required arr tag": `{"source_mode":"arr","tag_match_mode":"ANY","required_arr_tags":["org"]}`,
	}
	for name, body := range bad {
		if err := s.Set(ctx, KeyTargetTagFilter, json.RawMessage(body)); err == nil {
			t.Errorf("%s: Set accepted %s", name, body)
		}
	}
}
