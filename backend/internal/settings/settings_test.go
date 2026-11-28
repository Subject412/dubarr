package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subject412/dubarr/backend/internal/analyze"
	"github.com/Subject412/dubarr/backend/internal/naming"
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
	if sec.AllowForceTagOverride {
		t.Errorf("security options = %+v", sec)
	}

	mux := decode[MuxOptions](t, all[KeyMuxOptions])
	if mux.OutputMode != MuxOutputReplace {
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
	if sp.WorkDir != filepath.Join(dir, "temp") {
		t.Errorf("work dir = %q, want %q", sp.WorkDir, filepath.Join(dir, "temp"))
	}
	if sp.MediaRoots == nil {
		t.Errorf("media_roots should be an empty slice, not null")
	}

	w := decode[WatchOptions](t, all[KeyWatchOptions])
	if w.ScanIntervalSec != 60 || w.StabilityCheckIntervalSec < 4 {
		t.Errorf("watch options = %+v", w)
	}
	a := decode[ArrOptions](t, all[KeyArrOptions])
	if a.PollIntervalSec != 300 {
		t.Errorf("arr options = %+v", a)
	}
}

func TestDefaultScanIntervalIs60(t *testing.T) {
	w, ok := Defaults("/data")[KeyWatchOptions].(WatchOptions)
	if !ok {
		t.Fatalf("Defaults()[%s] is not a WatchOptions", KeyWatchOptions)
	}
	if w.ScanIntervalSec != 60 {
		t.Errorf("default scan_interval_sec = %d, want 60", w.ScanIntervalSec)
	}
}

// The range is not part of this change; pin it so moving the default cannot
// drift the bounds.
func TestScanIntervalBoundsUnchanged(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	body := func(sec int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"scan_interval_sec":%d,"stability_check_interval_sec":4}`, sec))
	}
	for _, sec := range []int{0, 3601} {
		if err := s.Set(ctx, KeyWatchOptions, body(sec)); err == nil {
			t.Errorf("Set accepted scan_interval_sec=%d", sec)
		}
	}
	for _, sec := range []int{1, 3600} {
		if err := s.Set(ctx, KeyWatchOptions, body(sec)); err != nil {
			t.Errorf("Set rejected scan_interval_sec=%d: %v", sec, err)
		}
	}
}

// A row written by a previous release keeps its stored cadence: Seed writes
// only absent keys, so the new default never rewrites an existing install.
func TestSeedKeepsStoredScanInterval(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stored := `{"scan_interval_sec":10,"stability_check_interval_sec":4}`
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		KeyWatchOptions, stored); err != nil {
		t.Fatalf("pre-seed watch.options: %v", err)
	}
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	got := decode[WatchOptions](t, mustGet(t, s, ctx, KeyWatchOptions))
	if got.ScanIntervalSec != 10 {
		t.Errorf("stored scan_interval_sec = %d after Seed, want 10", got.ScanIntervalSec)
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

// A configured language is validated through the ISO 639 table, not by shape:
// "pol" and "Polish" both mean pl, and a stored value must come back as the
// canonical two-letter code consumers compare against.
func TestLanguageCodesAreNormalised(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"iso639-2/B", []string{"pol"}, []string{"pl"}},
		{"lowercase name", []string{"polish"}, []string{"pl"}},
		{"capitalised name", []string{"Polish"}, []string{"pl"}},
		{"bcp-47 region", []string{"pt-BR"}, []string{"pt"}},
		{"ISO 639-2/T", []string{"deu"}, []string{"de"}},
		{"ISO 639-2/B german", []string{"ger"}, []string{"de"}},
		{"duplicate after normalisation", []string{"pl", "pol"}, []string{"pl"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			body, err := json.Marshal(LanguagesPolicy{
				TargetImportLanguages:   tt.in,
				ExtraReferenceLanguages: tt.in,
				ExportMode:              "rule_based",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := s.Set(ctx, KeyLanguagesPolicy, body); err != nil {
				t.Fatalf("Set(%v): %v", tt.in, err)
			}
			got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy))
			if !reflect.DeepEqual(got.TargetImportLanguages, tt.want) {
				t.Errorf("target languages = %v, want %v", got.TargetImportLanguages, tt.want)
			}
			if !reflect.DeepEqual(got.ExtraReferenceLanguages, tt.want) {
				t.Errorf("reference languages = %v, want %v", got.ExtraReferenceLanguages, tt.want)
			}
		})
	}
}

// Existing rows already carry ISO 639-1 codes; a save must leave them byte-for-
// byte equivalent instead of rewriting or dropping them.
func TestExistingISO639_1LanguagesAreUntouched(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	in := LanguagesPolicy{
		TargetImportLanguages:   []string{"pl", "cs"},
		ExtraReferenceLanguages: []string{"en", "org", "de"},
		ExportMode:              "rule_based",
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := s.Set(ctx, KeyLanguagesPolicy, body); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy))
	if !reflect.DeepEqual(got.TargetImportLanguages, in.TargetImportLanguages) {
		t.Errorf("target languages = %v, want %v untouched", got.TargetImportLanguages, in.TargetImportLanguages)
	}
	if !reflect.DeepEqual(got.ExtraReferenceLanguages, in.ExtraReferenceLanguages) {
		t.Errorf("reference languages = %v, want %v untouched", got.ExtraReferenceLanguages, in.ExtraReferenceLanguages)
	}
}

// A value that resolves to nothing must be refused with the offending value and
// the accepted forms named, because "zzz" silently matched no track before.
func TestUnknownLanguageIsRejectedWithTheForms(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{"zzz", "engl", "12", ""} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			s := openStore(t)
			body, err := json.Marshal(LanguagesPolicy{
				TargetImportLanguages:   []string{raw},
				ExtraReferenceLanguages: []string{},
				ExportMode:              "rule_based",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = s.Set(ctx, KeyLanguagesPolicy, body)
			if err == nil {
				t.Fatalf("Set accepted %q as a target import language", raw)
			}
			msg := err.Error()
			for _, want := range []string{fmt.Sprintf("%q", raw), "not recognised", "pl, pol", "Polish"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Errorf("error %v is not ErrInvalidValue", err)
			}
		})
	}
}

// pl-PL is a BCP-47 tag whose primary subtag is a real language: the region is
// dropped rather than refused.
func TestRegionalTagSavesAsItsPrimarySubtag(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	body, err := json.Marshal(LanguagesPolicy{
		TargetImportLanguages:   []string{"pl-PL"},
		ExtraReferenceLanguages: []string{},
		ExportMode:              "rule_based",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := s.Set(ctx, KeyLanguagesPolicy, body); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy))
	if !reflect.DeepEqual(got.TargetImportLanguages, []string{"pl"}) {
		t.Errorf("target languages = %v, want [pl]", got.TargetImportLanguages)
	}
}

// Reserved tokens are not languages: they must survive normalisation untouched.
func TestReservedReferenceTokensSurvive(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	body, err := json.Marshal(LanguagesPolicy{
		TargetImportLanguages:   []string{"pl"},
		ExtraReferenceLanguages: []string{"org", "all", "pol"},
		ExportMode:              "rule_based",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := s.Set(ctx, KeyLanguagesPolicy, body); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := decode[LanguagesPolicy](t, mustGet(t, s, ctx, KeyLanguagesPolicy))
	want := []string{"org", "all", "pl"}
	if !reflect.DeepEqual(got.ExtraReferenceLanguages, want) {
		t.Errorf("reference languages = %v, want %v", got.ExtraReferenceLanguages, want)
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
		KeySecurityOptions: `{"allow_force_tag_override":"yes"}`,
		KeyStoragePaths:    `{"media_roots":"not-a-list"}`,
		KeyMuxOptions:      `{"output_mode":"elsewhere"}`,
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
// object check it would persist an all-zero section: mux.options would lose its
// output mode and storage.paths would lose its roots.
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
	if sec.AllowForceTagOverride {
		t.Errorf("security.options changed by a rejected write: %+v", sec)
	}
	sp := decode[StoragePaths](t, mustGet(t, s, ctx, KeyStoragePaths))
	if sp.WorkDir != filepath.Join(dir, "temp") || len(sp.MediaRoots) != 0 {
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

// TestDefaultWorkDirIsTemp pins the documented temp-folder default: it is
// <DATA_DIR>/temp, so a fresh install stages under a folder the operator can
// see in Settings → Storage instead of an unset field that falls back at job
// time, and it never shares the data volume's top level with the database.
func TestDefaultWorkDirIsTemp(t *testing.T) {
	cases := []struct{ dataDir, want string }{
		{dataDir: "/config", want: "/config/temp"},
		{dataDir: "/srv/dubarr", want: "/srv/dubarr/temp"},
		{dataDir: "/srv/dubarr/", want: "/srv/dubarr/temp"},
	}
	for _, tc := range cases {
		paths, ok := Defaults(tc.dataDir)[KeyStoragePaths].(StoragePaths)
		if !ok {
			t.Fatalf("Defaults(%q) has no storage.paths section", tc.dataDir)
		}
		if paths.WorkDir != tc.want {
			t.Errorf("Defaults(%q).WorkDir = %q, want %q", tc.dataDir, paths.WorkDir, tc.want)
		}
	}
	if paths := Defaults("")[KeyStoragePaths].(StoragePaths); paths.WorkDir != "" {
		t.Errorf("Defaults(\"\").WorkDir = %q, want empty: no data dir to derive from", paths.WorkDir)
	}
}

// TestStorageWorkDirVaultContainment pins that a temp folder inside the Audio
// Vault is refused with ErrWorkDirInVault instead of being stored: the next
// vault pass reorganises anything it finds there, so a staged copy would be
// moved out from under a running job.
func TestStorageWorkDirVaultContainment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	cases := []struct {
		name    string
		workDir string
		refused bool
	}{
		{name: "sibling of the vault", workDir: "/temp"},
		{name: "unset", workDir: ""},
		{name: "vault itself", workDir: "/vault", refused: true},
		{name: "inside the vault", workDir: "/vault/temp", refused: true},
		{name: "vault with trailing slash", workDir: "/vault/", refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(StoragePaths{VaultDir: "/vault", WorkDir: tc.workDir})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = s.Set(ctx, KeyStoragePaths, raw)
			if !tc.refused {
				if err != nil {
					t.Fatalf("Set(work_dir=%q): %v", tc.workDir, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Set accepted work_dir %q inside the vault", tc.workDir)
			}
			if !strings.Contains(err.Error(), "work_dir must not be") {
				t.Fatalf("Set(work_dir=%q) = %v, want the work_dir containment message", tc.workDir, err)
			}
			// The rule itself carries a sentinel, so boot can recognise it.
			if err := CheckWorkDirOutsideVault(tc.workDir, "/vault"); !errors.Is(err, ErrWorkDirInVault) {
				t.Fatalf("CheckWorkDirOutsideVault(%q) = %v, want ErrWorkDirInVault", tc.workDir, err)
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
		raw := json.RawMessage(`{"output_mode":"` + mode + `"}`)
		if err := s.Set(ctx, KeyMuxOptions, raw); err != nil {
			t.Fatalf("Set(%s): %v", mode, err)
		}
		if got := decode[MuxOptions](t, mustGet(t, s, ctx, KeyMuxOptions)); got.OutputMode != mode {
			t.Errorf("stored output_mode = %q, want %q", got.OutputMode, mode)
		}
	}

	bad := json.RawMessage(`{"output_mode":"elsewhere"}`)
	if err := s.Set(ctx, KeyMuxOptions, bad); err == nil {
		t.Fatal("Set accepted an unknown output_mode")
	}

	// A row written before the field existed must read back as replace, both
	// from Get and from All (both pass through serveable).
	legacy := `{"keep_bak":true}`
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

// TestMuxOptionsRetiredKeepBak covers the stored-row half of retiring the .bak
// option: a row written before keep_bak was deleted still loads, but the key is
// dropped before the API serves it and the stored bytes stay untouched. A PUT
// that still carries the retired key is rejected naming the field.
func TestMuxOptionsRetiredKeepBak(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	legacy := `{"keep_bak":true}`
	if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, legacy, KeyMuxOptions); err != nil {
		t.Fatalf("write legacy row: %v", err)
	}

	served, ok, err := s.Get(ctx, KeyMuxOptions)
	if err != nil || !ok {
		t.Fatalf("Get = %v ok=%v", err, ok)
	}
	if strings.Contains(string(served), "keep_bak") {
		t.Errorf("served mux.options still exposes keep_bak: %s", served)
	}
	if got := decode[MuxOptions](t, served); got.OutputMode != MuxOutputReplace {
		t.Errorf("served mux.options = %+v", got)
	}

	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if strings.Contains(string(all[KeyMuxOptions]), "keep_bak") {
		t.Errorf("All still exposes keep_bak: %s", all[KeyMuxOptions])
	}
	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if strings.Contains(string(exp.Settings[KeyMuxOptions]), "keep_bak") {
		t.Errorf("Export still exposes keep_bak: %s", exp.Settings[KeyMuxOptions])
	}

	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, KeyMuxOptions).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != legacy {
		t.Errorf("stored row = %s, want it untouched at %s", stored, legacy)
	}

	if err := s.Set(ctx, KeyMuxOptions, json.RawMessage(legacy)); err == nil {
		t.Fatal("Set accepted the retired keep_bak field")
	} else if !strings.Contains(err.Error(), "keep_bak") {
		t.Errorf("Set error %q should name keep_bak", err)
	}
}

// TestMuxOptionsRetiredKeepBakCaseInsensitive pins the encoding/json trap
// behind the strip: object keys match struct fields case-insensitively, so a
// row spelling the retired key as Keep_Bak would slip past an exact lookup,
// fail the strict decode and be served raw. No letter case may survive.
func TestMuxOptionsRetiredKeepBakCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, variant := range []string{"Keep_Bak", "KEEP_BAK", "keep_Bak"} {
		legacy := `{"` + variant + `":true}`
		if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, legacy, KeyMuxOptions); err != nil {
			t.Fatalf("write legacy row: %v", err)
		}

		served, ok, err := s.Get(ctx, KeyMuxOptions)
		if err != nil || !ok {
			t.Fatalf("Get = %v ok=%v", err, ok)
		}
		if strings.Contains(strings.ToLower(string(served)), "keep_bak") {
			t.Errorf("%s: served mux.options still exposes the retired key: %s", variant, served)
		}
		if got := decode[MuxOptions](t, served); got.OutputMode != MuxOutputReplace {
			t.Errorf("%s: served mux.options = %+v", variant, got)
		}

		all, err := s.All(ctx)
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if strings.Contains(strings.ToLower(string(all[KeyMuxOptions])), "keep_bak") {
			t.Errorf("%s: All still exposes the retired key: %s", variant, all[KeyMuxOptions])
		}

		exp, err := s.Export(ctx)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		if strings.Contains(strings.ToLower(string(exp.Settings[KeyMuxOptions])), "keep_bak") {
			t.Errorf("%s: Export still exposes the retired key: %s", variant, exp.Settings[KeyMuxOptions])
		}
	}
}

// TestStoredTrackTitleIsRetired is U10: the operator's free-text Matroska track
// title is gone, so a row stored before 0.6.0 still loads and serves, but the
// retired key is dropped before the API hands it out and the stored bytes stay
// untouched. A PUT that still carries it is rejected naming the field.
func TestStoredTrackTitleIsRetired(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	legacy := `{"track_title_in_mkv":"Polski Dubbing","output_mode":"replace"}`
	if _, err := s.db.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, legacy, KeyMuxOptions); err != nil {
		t.Fatalf("write legacy row: %v", err)
	}

	served, ok, err := s.Get(ctx, KeyMuxOptions)
	if err != nil || !ok {
		t.Fatalf("Get = %v ok=%v", err, ok)
	}
	if strings.Contains(string(served), "track_title_in_mkv") {
		t.Errorf("served mux.options still exposes the retired key: %s", served)
	}
	if got := decode[MuxOptions](t, served); got.OutputMode != MuxOutputReplace {
		t.Errorf("served mux.options = %+v", got)
	}

	// The stored bytes are left alone; only what the API serves is cleaned.
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, KeyMuxOptions).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != legacy {
		t.Errorf("stored row changed: %s", stored)
	}
	// The served section is still a valid body to save back.
	if err := s.Set(ctx, KeyMuxOptions, served); err != nil {
		t.Errorf("served mux.options no longer validates: %v", err)
	}

	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if strings.Contains(string(all[KeyMuxOptions]), "track_title_in_mkv") {
		t.Errorf("All still exposes the retired key: %s", all[KeyMuxOptions])
	}
	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if strings.Contains(string(exp.Settings[KeyMuxOptions]), "track_title_in_mkv") {
		t.Errorf("Export still exposes the retired key: %s", exp.Settings[KeyMuxOptions])
	}

	if err := s.Set(ctx, KeyMuxOptions, json.RawMessage(legacy)); err == nil {
		t.Fatal("Set accepted the retired track_title_in_mkv field")
	} else if !strings.Contains(err.Error(), "track_title_in_mkv") {
		t.Errorf("Set error %q should name track_title_in_mkv", err)
	}
}

// TestImportsOptionsIsNotAKnownKey is U11: the donor markers are a fixed part of
// how the queue pairs siblings, so `imports.options` is no longer a settings
// section at all. A row written before 0.6.0 must not break a settings read: All
// skips unknown keys, so the row becomes invisible without deleting it.
func TestImportsOptionsIsNotAKnownKey(t *testing.T) {
	ctx := context.Background()

	if isKnownKey(KeyImportsOptions) {
		t.Errorf("IsKnownKey(%q) = true, want false: the section is retired", KeyImportsOptions)
	}
	for _, key := range KnownKeys() {
		if key == KeyImportsOptions {
			t.Errorf("KnownKeys still lists %q", KeyImportsOptions)
		}
	}
	if _, ok := Defaults("")[KeyImportsOptions]; ok {
		t.Errorf("Defaults still carries %q", KeyImportsOptions)
	}

	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	legacy := `{"donor_patterns":["CUSTOM"]}`
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, '2026-01-01 00:00:00')`,
		KeyImportsOptions, legacy); err != nil {
		t.Fatalf("write legacy row: %v", err)
	}

	// A legacy row must not make the settings read fail or leak into the
	// envelope the SPA renders.
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All with a retired row: %v", err)
	}
	if _, ok := all[KeyImportsOptions]; ok {
		t.Errorf("All served the retired %q section: %s", KeyImportsOptions, all[KeyImportsOptions])
	}
	if _, ok, err := s.Get(ctx, KeyImportsOptions); !errors.Is(err, ErrUnknownKey) || ok {
		t.Errorf("Get(%q) = ok=%v err=%v, want ErrUnknownKey", KeyImportsOptions, ok, err)
	}
	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, ok := exp.Settings[KeyImportsOptions]; ok {
		t.Errorf("Export served the retired %q section", KeyImportsOptions)
	}
	if err := s.Set(ctx, KeyImportsOptions, json.RawMessage(legacy)); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Set(%q) = %v, want ErrUnknownKey", KeyImportsOptions, err)
	} else if !strings.Contains(err.Error(), KeyImportsOptions) {
		t.Errorf("Set error %q should name %s", err, KeyImportsOptions)
	}

	// The stored bytes are left alone: no migration, no rewrite.
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, KeyImportsOptions).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != legacy {
		t.Errorf("stored row changed: %s", stored)
	}
}

// TestDefaultsHaveNoTrackTitle: a fresh install ships no track title at all —
// the muxer names the track after its language.
func TestDefaultsHaveNoTrackTitle(t *testing.T) {
	mux, ok := Defaults("")[KeyMuxOptions].(MuxOptions)
	if !ok {
		t.Fatalf("Defaults()[%s] is not a MuxOptions", KeyMuxOptions)
	}
	raw, err := json.Marshal(mux)
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}
	if strings.Contains(string(raw), "track_title_in_mkv") {
		t.Errorf("mux.options defaults still carry a track title: %s", raw)
	}
	if mux.OutputMode != MuxOutputReplace {
		t.Errorf("default output_mode = %q, want %q", mux.OutputMode, MuxOutputReplace)
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

// TestSettingsRefusesAPatternThatDoesNotCompile pins #155: in REGEX mode the
// entries are patterns to the gate, so a pattern the engine refuses can never
// match anything. Accepting it would store a filter whose targets wait forever
// with no explanation, so the write is refused and the message names the
// section, the pattern and Go's own reason.
func TestSettingsRefusesAPatternThatDoesNotCompile(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	// A backreference: PCRE, not RE2 — the typo an operator actually makes.
	const pattern = `\d+\1`
	body := `{"tag_match_mode":"REGEX","tag_case_sensitive":false,"enforce_tag_filter":true,"required_filename_tags":["` + `\\d+\\1` + `"]}`
	err := s.Set(ctx, KeyTargetTagFilter, json.RawMessage(body))
	if err == nil {
		t.Fatalf("Set stored the uncompilable pattern %q", pattern)
	}
	if !errors.Is(err, ErrInvalidValue) {
		t.Errorf("error %v is not ErrInvalidValue", err)
	}
	for _, want := range []string{"target.tag_filter", "filename tag pattern", fmt.Sprintf("%q", pattern), "invalid escape sequence"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// Nothing was stored: the refusal is not a partial write.
	if _, ok, getErr := s.Get(ctx, KeyTargetTagFilter); getErr != nil || ok {
		t.Errorf("Get after a refused write = ok=%v err=%v, want no row", ok, getErr)
	}

	// The same string is a plain substring in ANY and ALL mode, where it is
	// never compiled; refusing it there would reject a legitimate filename.
	for _, mode := range []string{"ANY", "ALL"} {
		literal := `{"tag_match_mode":"` + mode + `","enforce_tag_filter":true,"required_filename_tags":["` + `\\d+\\1` + `"]}`
		if err := s.Set(ctx, KeyTargetTagFilter, json.RawMessage(literal)); err != nil {
			t.Errorf("Set(%s mode, literal %q) = %v, want accepted", mode, pattern, err)
		}
	}
}

// TestSettingsAcceptsLookaroundInLiteralMode pins the other half of #155: a
// lookahead is not a Go pattern, but in ALL mode the entry is a filename
// substring, so it must be stored exactly as typed.
func TestSettingsAcceptsLookaroundInLiteralMode(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	body := `{"tag_match_mode":"ALL","enforce_tag_filter":true,"required_filename_tags":["(?=x)"]}`
	if err := s.Set(ctx, KeyTargetTagFilter, json.RawMessage(body)); err != nil {
		t.Fatalf("Set(ALL mode, literal (?=x)) = %v, want accepted", err)
	}
	tf := decode[TargetTagFilter](t, mustGet(t, s, ctx, KeyTargetTagFilter))
	if len(tf.RequiredFilenameTags) != 1 || tf.RequiredFilenameTags[0] != "(?=x)" {
		t.Errorf("required filename tags = %v, want [(?=x)] stored verbatim", tf.RequiredFilenameTags)
	}
}

func TestSeedAudioOptionsIsEmpty(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	got := decode[AudioOptions](t, mustGet(t, s, ctx, KeyAudioOptions))
	if got.Rules == nil || len(got.Rules) != 0 {
		t.Fatalf("seeded audio.options = %#v, want an empty non-nil rule list (ADR-0010)", got.Rules)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, ok := all[KeyAudioOptions]; !ok {
		t.Fatal("All is missing the seeded audio.options section")
	}
}

// A database written before audio.options existed has no row at all: Get
// reports absence (the job path must treat that as "no rules"), while an
// existing row without `rules` decodes as an empty list everywhere.
func TestLegacyAudioOptionsDecodesAsEmpty(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	if _, ok, err := s.Get(ctx, KeyAudioOptions); err != nil || ok {
		t.Fatalf("Get(absent) = ok=%v err=%v, want not found", ok, err)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, ok := all[KeyAudioOptions]; ok {
		t.Fatal("All invented an audio.options row that does not exist")
	}
	exp, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, ok := exp.Settings[KeyAudioOptions]; ok {
		t.Fatal("Export invented an audio.options row that does not exist")
	}
	// Decoding the zero section is how the worker treats an absent row.
	zero := AudioOptions{}
	if zero.Rules != nil {
		t.Fatalf("zero AudioOptions = %#v", zero.Rules)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, '{}', '2026-01-01 00:00:00')`,
		KeyAudioOptions); err != nil {
		t.Fatalf("insert legacy audio.options: %v", err)
	}
	check := func(where string, raw json.RawMessage) {
		t.Helper()
		got := decode[AudioOptions](t, raw)
		if got.Rules == nil || len(got.Rules) != 0 {
			t.Errorf("%s rules = %#v, want empty non-nil", where, got.Rules)
		}
	}
	check("Get", mustGet(t, s, ctx, KeyAudioOptions))
	all, err = s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	check("All", all[KeyAudioOptions])
	exp, err = s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	check("Export", exp.Settings[KeyAudioOptions])
}

func TestAudioOptionsValidation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	bad := map[string]string{
		"unknown codec":       `{"rules":[{"enabled":true,"when":{},"then":{"codec":"mp3"}}]}`,
		"bad layout":          `{"rules":[{"enabled":true,"when":{"channel_layout":"surround"},"then":{"codec":"opus"}}]}`,
		"negative bitrate":    `{"rules":[{"enabled":true,"when":{"min_bitrate_bps":-1},"then":{"codec":"opus"}}]}`,
		"malformed regex":     `{"rules":[{"enabled":true,"when":{"source_codec_regex":"([a-z"},"then":{"codec":"opus"}}]}`,
		"unknown field":       `{"rules":[{"enabled":true,"when":{},"then":{"codec":"opus"},"bogus":1}]}`,
		"negative action bit": `{"rules":[{"enabled":true,"when":{},"then":{"codec":"aac","bitrate_bps":-8}}]}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			err := s.Set(ctx, KeyAudioOptions, json.RawMessage(body))
			if err == nil {
				t.Fatal("Set accepted an invalid audio.options payload")
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("error %v is not ErrInvalidValue", err)
			}
		})
	}

	// Valid rules round-trip and keep their order (first match wins).
	body := `{"rules":[{"enabled":true,"when":{"source_codecs":["pcm_s16le"],"min_bitrate_bps":1536000},"then":{"codec":"opus","bitrate_bps":160000,"channels":2}},{"enabled":false,"when":{},"then":{"codec":"keep"}}]}`
	if err := s.Set(ctx, KeyAudioOptions, json.RawMessage(body)); err != nil {
		t.Fatalf("Set(valid): %v", err)
	}
	got := decode[AudioOptions](t, mustGet(t, s, ctx, KeyAudioOptions))
	if len(got.Rules) != 2 || got.Rules[0].Then.Codec != "opus" || got.Rules[0].Then.BitrateBPS == nil || *got.Rules[0].Then.BitrateBPS != 160000 {
		t.Fatalf("roundtrip rules = %+v", got.Rules)
	}
	if got.Rules[1].Enabled || got.Rules[1].Then.Codec != "keep" {
		t.Fatalf("roundtrip second rule = %+v", got.Rules[1])
	}
}

// sync.defaults gained parallel_checkpoints in U-DSP.8. Every row written
// before that has no key, so the decode must resolve it to the shipped default;
// a row that *does* carry 0 must not be read as "absent", or an operator's
// explicit 0 would silently run one worker anyway.
func TestSyncDefaultsParallelCheckpointsDecode(t *testing.T) {
	const legacy = `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
		`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],"preview_window_sec":60}`

	var absent SyncDefaults
	if err := json.Unmarshal([]byte(legacy), &absent); err != nil {
		t.Fatalf("unmarshal legacy row: %v", err)
	}
	if absent.ParallelCheckpoints != DefaultParallelCheckpoints {
		t.Errorf("absent parallel_checkpoints = %d, want %d", absent.ParallelCheckpoints, DefaultParallelCheckpoints)
	}
	if err := validateSync(&absent); err != nil {
		t.Errorf("legacy row failed validation: %v", err)
	}

	var zero SyncDefaults
	zeroBody := `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
		`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],` +
		`"preview_window_sec":60,"parallel_checkpoints":0}`
	if err := json.Unmarshal([]byte(zeroBody), &zero); err != nil {
		t.Fatalf("unmarshal zero: %v", err)
	}
	if zero.ParallelCheckpoints != 0 {
		t.Fatalf("present 0 decoded as %d; it must stay 0 for validation to reject", zero.ParallelCheckpoints)
	}
	if err := validateSync(&zero); err == nil {
		t.Error("validateSync accepted a present 0")
	} else if err.Error() != "parallel_checkpoints must be 1..8" {
		t.Errorf("message for a present 0 = %q", err.Error())
	}

	var five SyncDefaults
	if err := json.Unmarshal([]byte(`{"parallel_checkpoints":5}`), &five); err != nil {
		t.Fatalf("unmarshal five: %v", err)
	}
	if five.ParallelCheckpoints != 5 {
		t.Fatalf("present 5 decoded as %d", five.ParallelCheckpoints)
	}

	// A stored legacy row decodes the same way through the store.
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		KeySyncDefaults, legacy); err != nil {
		t.Fatalf("insert legacy sync.defaults: %v", err)
	}
	if got := decode[SyncDefaults](t, mustGet(t, s, ctx, KeySyncDefaults)).ParallelCheckpoints; got != DefaultParallelCheckpoints {
		t.Errorf("stored legacy row served %d, want %d", got, DefaultParallelCheckpoints)
	}
}

func TestDefaultsParallelCheckpoints(t *testing.T) {
	sd, ok := Defaults("")[KeySyncDefaults].(SyncDefaults)
	if !ok {
		t.Fatalf("Defaults()[%s] is not a SyncDefaults", KeySyncDefaults)
	}
	if sd.ParallelCheckpoints != 1 {
		t.Errorf("default parallel_checkpoints = %d, want 1", sd.ParallelCheckpoints)
	}
}

func TestValidateSyncRejectsParallelCheckpointsOutOfRange(t *testing.T) {
	for _, workers := range []int{-1, 0, 9} {
		p := validSyncDefaults()
		p.ParallelCheckpoints = workers
		err := validateSync(&p)
		if err == nil {
			t.Errorf("validateSync accepted parallel_checkpoints=%d", workers)
			continue
		}
		if err.Error() != "parallel_checkpoints must be 1..8" {
			t.Errorf("message for %d = %q, want %q", workers, err.Error(), "parallel_checkpoints must be 1..8")
		}
	}
	for _, workers := range []int{1, 4, 8} {
		p := validSyncDefaults()
		p.ParallelCheckpoints = workers
		if err := validateSync(&p); err != nil {
			t.Errorf("validateSync rejected parallel_checkpoints=%d: %v", workers, err)
		}
	}
}

// The write path must reach the same verdict as validateSync, with the same
// wording: a stored 0 is never quietly rewritten to 1.
func TestSetRejectsZeroParallelCheckpoints(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	body := `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
		`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],` +
		`"preview_window_sec":60,"parallel_checkpoints":0}`
	err := s.Set(ctx, KeySyncDefaults, json.RawMessage(body))
	if err == nil {
		t.Fatal("Set accepted parallel_checkpoints=0")
	}
	if !strings.Contains(err.Error(), "parallel_checkpoints must be 1..8") {
		t.Fatalf("Set error %q does not carry the validation message", err)
	}
}

func TestSyncDefaultsParallelCheckpointsRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	body := `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
		`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],` +
		`"preview_window_sec":60,"parallel_checkpoints":5}`
	if err := s.Set(ctx, KeySyncDefaults, json.RawMessage(body)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw := mustGet(t, s, ctx, KeySyncDefaults)
	if got := decode[SyncDefaults](t, raw).ParallelCheckpoints; got != 5 {
		t.Fatalf("stored parallel_checkpoints = %d, want 5 (%s)", got, raw)
	}
	// The stored row keeps the key, so a client that re-reads and PATCHes the
	// section back cannot lose it.
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode stored row: %v", err)
	}
	if string(stored["parallel_checkpoints"]) != "5" {
		t.Errorf("stored row parallel_checkpoints = %s, want 5", stored["parallel_checkpoints"])
	}
}

// validSyncDefaults is the settled section, used to exercise one validator rule
// at a time without tripping another.
func validSyncDefaults() SyncDefaults {
	return SyncDefaults{
		MaxOffsetSec:           180,
		CheckpointPositionsPct: []int{10, 25, 40, 55, 70, 85},
		CheckpointWindowSec:    150,
		PreviewPositionsPct:    []int{10, 25, 50, 75, 90},
		PreviewWindowSec:       60,
		ParallelCheckpoints:    DefaultParallelCheckpoints,
		AnalysisSource:         DefaultAnalysisSource,
	}
}

// legacySyncDefaults is the section exactly as every row written before
// analysis_source (and parallel_checkpoints) existed carries it.
const legacySyncDefaults = `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
	`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],"preview_window_sec":60}`

// analysis_source follows the parallel_checkpoints contract: absent is the
// compiled default (now the windowed mode, which falls back to a whole-track
// decode when a plan cannot be built), a present "" stays empty so the
// boundary — not the decoder — decides what it means.
func TestSyncDefaultsAnalysisSourceDecode(t *testing.T) {
	var absent SyncDefaults
	if err := json.Unmarshal([]byte(legacySyncDefaults), &absent); err != nil {
		t.Fatalf("unmarshal legacy row: %v", err)
	}
	if absent.AnalysisSource != DefaultAnalysisSource {
		t.Errorf("absent analysis_source = %q, want %q", absent.AnalysisSource, DefaultAnalysisSource)
	}
	if err := validateSync(&absent); err != nil {
		t.Errorf("legacy row failed validation: %v", err)
	}

	var empty SyncDefaults
	if err := json.Unmarshal([]byte(`{"analysis_source":""}`), &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if empty.AnalysisSource != "" {
		t.Fatalf("present \"\" decoded as %q; it must stay empty for validation to judge", empty.AnalysisSource)
	}

	for _, mode := range []string{analyze.AnalysisSourceWindows, analyze.AnalysisSourceFull} {
		var got SyncDefaults
		if err := json.Unmarshal([]byte(`{"analysis_source":"`+mode+`"}`), &got); err != nil {
			t.Fatalf("unmarshal %q: %v", mode, err)
		}
		if got.AnalysisSource != mode {
			t.Errorf("present %q decoded as %q", mode, got.AnalysisSource)
		}
	}
}

// The compiled default is 'windows': a job analyses windows and falls back to a
// whole-track decode when the plan cannot be built.
func TestDefaultsAnalysisSource(t *testing.T) {
	sd, ok := Defaults("")[KeySyncDefaults].(SyncDefaults)
	if !ok {
		t.Fatalf("Defaults()[%s] is not a SyncDefaults", KeySyncDefaults)
	}
	if sd.AnalysisSource != analyze.AnalysisSourceWindows {
		t.Errorf("default analysis_source = %q, want %q", sd.AnalysisSource, analyze.AnalysisSourceWindows)
	}
	if DefaultAnalysisSource != analyze.AnalysisSourceWindows {
		t.Errorf("DefaultAnalysisSource = %q, want the compiled default %q", DefaultAnalysisSource, analyze.AnalysisSourceWindows)
	}
}

// validateSync accepts "" as "the compiled default" and rejects everything
// outside {windows, full}. Case is not normalised, so "Windows" is a client bug
// the operator is told about instead of one silently repaired.
func TestValidateSyncAnalysisSource(t *testing.T) {
	for _, mode := range []string{"", analyze.AnalysisSourceWindows, analyze.AnalysisSourceFull} {
		p := validSyncDefaults()
		p.AnalysisSource = mode
		if err := validateSync(&p); err != nil {
			t.Errorf("validateSync rejected %q: %v", mode, err)
		}
	}
	for _, mode := range []string{"bogus", "Windows", "FULL", "windows ", " full"} {
		p := validSyncDefaults()
		p.AnalysisSource = mode
		err := validateSync(&p)
		if err == nil {
			t.Errorf("validateSync accepted %q", mode)
			continue
		}
		if err.Error() != "analysis_source must be windows or full" {
			t.Errorf("message for %q = %q, want %q", mode, err.Error(), "analysis_source must be windows or full")
		}
	}
}

// The legacy-row contract in both directions: a stored row without the key is
// served with the compiled default, PATCHing that exact served object neither
// fails nor changes the mode, and neither the read nor the write turns the row
// into an "" one.
func TestSyncDefaultsAnalysisSourceLegacyRow(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		KeySyncDefaults, legacySyncDefaults); err != nil {
		t.Fatalf("insert legacy sync.defaults: %v", err)
	}

	served := mustGet(t, s, ctx, KeySyncDefaults)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(served, &fields); err != nil {
		t.Fatalf("decode served section: %v (%s)", err, served)
	}
	if got := string(fields["analysis_source"]); got != `"windows"` {
		t.Fatalf("served analysis_source = %s, want \"windows\" (%s)", got, served)
	}
	sd := decode[SyncDefaults](t, served)
	if sd.ParallelCheckpoints != DefaultParallelCheckpoints || sd.CheckpointWindowSec != 150 {
		t.Fatalf("served section lost a legacy value: %+v", sd)
	}

	// The read must not rewrite the stored bytes.
	var stored string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, KeySyncDefaults).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != legacySyncDefaults {
		t.Errorf("Get rewrote the stored row:\n got %s\nwant %s", stored, legacySyncDefaults)
	}

	// The PATCH the card sends when the operator saves the untouched section.
	if err := s.Set(ctx, KeySyncDefaults, served); err != nil {
		t.Fatalf("PATCH of the served legacy section: %v", err)
	}
	after := mustGet(t, s, ctx, KeySyncDefaults)
	if got := decode[SyncDefaults](t, after).AnalysisSource; got != DefaultAnalysisSource {
		t.Fatalf("analysis_source after PATCH = %q, want %q (%s)", got, DefaultAnalysisSource, after)
	}
	if strings.Contains(string(after), `"analysis_source":""`) {
		t.Errorf("stored row carries an empty analysis_source: %s", after)
	}
	var rewritten map[string]json.RawMessage
	if err := json.Unmarshal(after, &rewritten); err != nil {
		t.Fatalf("decode rewritten row: %v", err)
	}
	if got := string(rewritten["analysis_source"]); got != `"windows"` {
		t.Errorf("stored analysis_source = %s, want \"windows\"", got)
	}
	round := decode[SyncDefaults](t, after)
	if round.CheckpointWindowSec != 150 || round.MaxOffsetSec != 180 ||
		round.ParallelCheckpoints != DefaultParallelCheckpoints {
		t.Errorf("round trip changed the section: %+v", round)
	}
}

// A client that sends "" (or omits the key) must not leave the DB with a row
// every later reader has to interpret: normalize materialises the default.
func TestSetMaterialisesEmptyAnalysisSource(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	for _, body := range []string{
		legacySyncDefaults,
		`{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
			`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],` +
			`"preview_window_sec":60,"parallel_checkpoints":1,"analysis_source":""}`,
	} {
		if err := s.Set(ctx, KeySyncDefaults, json.RawMessage(body)); err != nil {
			t.Fatalf("Set(%s): %v", body, err)
		}
		raw := mustGet(t, s, ctx, KeySyncDefaults)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("decode stored row: %v (%s)", err, raw)
		}
		if got := string(fields["analysis_source"]); got != `"windows"` {
			t.Errorf("stored analysis_source = %s, want \"windows\" (%s)", got, raw)
		}
	}
}

// A stored "windows" must survive the round trip byte-for-byte in its mode, so
// U-WIN.9's flip cannot silently rewrite an operator's explicit choice.
func TestSyncDefaultsAnalysisSourceRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	body := `{"max_offset_sec":180,"checkpoint_positions_pct":[10,25,40,55,70,85],` +
		`"checkpoint_window_sec":150,"preview_positions_pct":[10,25,50,75,90],` +
		`"preview_window_sec":60,"parallel_checkpoints":1,"analysis_source":"windows"}`
	if err := s.Set(ctx, KeySyncDefaults, json.RawMessage(body)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw := mustGet(t, s, ctx, KeySyncDefaults)
	if got := decode[SyncDefaults](t, raw).AnalysisSource; got != analyze.AnalysisSourceWindows {
		t.Fatalf("stored analysis_source = %q, want windows (%s)", got, raw)
	}
}

// The sync.strategy section: the technique pipeline #82 adds.

// defaultStrategyJSON is the shipped section, byte for byte what a fresh
// install writes. It is a literal, not a re-marshalled constant: the SPA's
// DEFAULT_SETTINGS['sync.strategy'] must match it, and a change here is a
// migration, not a refactor.
//
// #84 added the `piecewise` block to the same section, off at the documented
// numbers: the feature is opt-in (D5), and the numbers are written out because
// the section is the document the operator edits — the UI has to be served the
// values it would be editing. That is why the block is always present here and
// absent from a report while it is off (analyze.Strategy omits it at the shipped
// values; settings.StrategyOptions always writes it).
const defaultStrategyJSON = `{"preset":"balanced","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true}],"piecewise":{"enabled":false,"step_sec":45,"window_sec":60,"split_penalty_ms":250,"min_segment_sec":120,"max_segments":12}}`

func TestKnownKeysIncludesSyncStrategy(t *testing.T) {
	if KeySyncStrategy != "sync.strategy" {
		t.Fatalf("KeySyncStrategy = %q, want %q", KeySyncStrategy, "sync.strategy")
	}
	found := false
	for _, key := range KnownKeys() {
		if key == KeySyncStrategy {
			found = true
		}
	}
	if !found {
		t.Fatalf("KnownKeys() = %v, want it to include %q", KnownKeys(), KeySyncStrategy)
	}
}

// TestSyncStrategyDefaultsAreTheBalancedList pins the shipped section and the
// D4 guarantee behind it: one waveform entry, on every checkpoint.
func TestSyncStrategyDefaultsAreTheBalancedList(t *testing.T) {
	defaults := Defaults(t.TempDir())
	raw, ok := defaults[KeySyncStrategy]
	if !ok {
		t.Fatalf("Defaults() has no %q section", KeySyncStrategy)
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	if string(blob) != defaultStrategyJSON {
		t.Fatalf("shipped %s =\n %s\nwant\n %s", KeySyncStrategy, blob, defaultStrategyJSON)
	}

	strategy := decode[StrategyOptions](t, blob)
	if strategy.Preset != analyze.PresetBalanced {
		t.Fatalf("preset = %q, want %q", strategy.Preset, analyze.PresetBalanced)
	}
	if len(strategy.Techniques) != 1 {
		t.Fatalf("techniques = %+v, want exactly one", strategy.Techniques)
	}
	entry := strategy.Techniques[0]
	if entry.ID != analyze.TechniqueWaveform || !entry.Enabled || entry.When != analyze.WhenAlways {
		t.Fatalf("shipped entry = %+v, want waveform/always/enabled", entry)
	}
	if entry.MinConfidence != 0.10 || entry.MinMatchZ != 5.0 || !entry.ZGate {
		t.Fatalf("shipped thresholds = %v/%v/%v, want 0.10/5.0/true", entry.MinConfidence, entry.MinMatchZ, entry.ZGate)
	}
	// The piecewise block is written at the documented numbers and switched off:
	// a fresh install measures six points and writes 0.4.0's report (D5).
	if got, want := strategy.Piecewise, analyze.DefaultPiecewisePolicy(); got != want {
		t.Fatalf("shipped piecewise = %+v, want the documented default %+v", got, want)
	}
	if strategy.Piecewise.Enabled {
		t.Fatal("the shipped piecewise block is enabled: segmentation is opt-in (D5)")
	}
}

// TestSyncStrategyAbsentSectionMaterialises: a row written before 0.5.0 carries
// neither key. Reading it must serve the whole default list, never
// `"techniques": null` — a pipeline with nothing to run is not a setting.
func TestSyncStrategyAbsentSectionMaterialises(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		KeySyncStrategy, `{}`); err != nil {
		t.Fatalf("insert legacy %s: %v", KeySyncStrategy, err)
	}

	served := mustGet(t, s, ctx, KeySyncStrategy)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(served, &fields); err != nil {
		t.Fatalf("decode served section: %v (%s)", err, served)
	}
	if string(fields["techniques"]) == "null" || len(fields["techniques"]) == 0 {
		t.Fatalf("served techniques = %s, want the default list (%s)", fields["techniques"], served)
	}
	if got := string(fields["preset"]); got != `"balanced"` {
		t.Fatalf("served preset = %s, want \"balanced\" (%s)", got, served)
	}

	// The read must not rewrite the stored bytes.
	var stored string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, KeySyncStrategy).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != `{}` {
		t.Errorf("Get rewrote the stored row: %s", stored)
	}
	if got := len(decode[StrategyOptions](t, served).Techniques); got != 1 {
		t.Fatalf("served techniques = %d entries, want 1", got)
	}
}

// TestSyncStrategyValidationTable is one row per rule in the #82 validation
// table, each through the write path the API uses, so a 400 comes back with the
// message the operator reads.
func TestSyncStrategyValidationTable(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ name, body, wantErr string }{
		{"no techniques", `{"preset":"balanced","techniques":[]}`, "techniques must have 1..4 entries"},
		{"five techniques", `{"preset":"custom","techniques":[` +
			`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5},` +
			`{"id":"mfcc","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5},` +
			`{"id":"waveform","enabled":true,"when":"below_confidence","min_confidence":0.1,"min_match_z":5},` +
			`{"id":"mfcc","enabled":true,"when":"above_drift","min_confidence":0.1,"min_match_z":5},` +
			`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5}]}`, "techniques must have 1..4 entries"},
		{"unknown technique", `{"preset":"custom","techniques":[{"id":"chroma","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5}]}`, `unknown technique "chroma"`},
		{"duplicate technique", `{"preset":"custom","techniques":[` +
			`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5},` +
			`{"id":"waveform","enabled":true,"when":"below_confidence","min_confidence":0.1,"min_match_z":5}]}`, `duplicate technique "waveform"`},
		{"nothing enabled", `{"preset":"custom","techniques":[{"id":"waveform","enabled":false,"when":"always","min_confidence":0.1,"min_match_z":5}]}`, "at least one technique must be enabled"},
		{"the first enabled step is not always", `{"preset":"custom","techniques":[` +
			`{"id":"mfcc","enabled":true,"when":"above_drift","min_confidence":0.1,"min_match_z":5},` +
			`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5}]}`, "the first enabled technique must run always"},
		{"below_confidence needs an earlier step", `{"preset":"custom","techniques":[{"id":"mfcc","enabled":true,"when":"below_confidence","min_confidence":0.1,"min_match_z":5}]}`, `technique "mfcc" needs an earlier step to react to`},
		{"min_confidence above 1", `{"preset":"custom","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":1.5,"min_match_z":5}]}`, "min_confidence must be 0..1"},
		{"min_match_z above 100", `{"preset":"custom","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":101}]}`, "min_match_z must be 0..100"},
		{"unknown preset", `{"preset":"turbo","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5}]}`, "preset must be fast, balanced, maximum_accuracy or custom"},
		{"unknown field", `{"preset":"balanced","technique":[]}`, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			err := s.Set(ctx, KeySyncStrategy, json.RawMessage(tc.body))
			if err == nil {
				t.Fatalf("Set(%s) = nil, want %q", tc.body, tc.wantErr)
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("Set(%s) = %v, want ErrInvalidValue", tc.body, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Set(%s) = %q, want it to contain %q", tc.body, err, tc.wantErr)
			}
		})
	}
}

// TestWaveformEntryWithZeroZGateFailsValidation pins the rule the per-technique
// σ gate needs: a z-gated technique must carry a band to be gated by, so
// `min_match_z: 0` is legal only for an entry that says it is judged on quality
// alone. The reverse is not an error — a quality-gated entry that still carries
// a σ is accepted and the number is ignored, because it is a diagnostic there.
func TestWaveformEntryWithZeroZGateFailsValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ name, body, wantErr string }{
		{
			name:    "a z-gated waveform entry with no band",
			body:    `{"preset":"custom","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":0,"z_gate":true}]}`,
			wantErr: "min_match_z must be above 0",
		},
		{
			name: "an absent z_gate reads as quality-gated, so no band is needed",
			body: `{"preset":"custom","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":0}]}`,
		},
		{
			name: "a quality-gated mfcc entry may carry a band that is ignored",
			body: `{"preset":"custom","techniques":[` +
				`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true},` +
				`{"id":"mfcc","enabled":true,"when":"below_confidence","min_confidence":0.15,"min_match_z":8,"z_gate":false}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			err := s.Set(ctx, KeySyncStrategy, json.RawMessage(tc.body))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Set(%s) = %v, want it accepted", tc.body, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Set(%s) = nil, want %q", tc.body, tc.wantErr)
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("Set(%s) = %v, want ErrInvalidValue", tc.body, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Set(%s) = %q, want it to contain %q", tc.body, err, tc.wantErr)
			}
		})
	}
}

// TestSyncStrategyPresetBecomesCustomWhenEdited: the server never trusts the
// label. The list is authoritative, and a list that is not the named preset's
// canonical one is stored as custom, in one direction only.
func TestSyncStrategyPresetBecomesCustomWhenEdited(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ name, body, wantPreset string }{
		{
			name: "an edited balanced list is custom",
			body: `{"preset":"balanced","techniques":[` +
				`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true},` +
				`{"id":"mfcc","enabled":true,"when":"below_confidence","min_confidence":0.15,"min_match_z":0,"z_gate":false}]}`,
			wantPreset: analyze.PresetCustom,
		},
		{
			name:       "an unedited balanced list keeps its label",
			body:       `{"preset":"balanced","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true}]}`,
			wantPreset: analyze.PresetBalanced,
		},
		{
			// D4a: Fast keeps the waveform matcher and spends fewer
			// checkpoints, so its technique list IS the balanced one. Only the
			// label distinguishes them, and the server keeps that label.
			name:       "fast keeps its own label with the balanced list",
			body:       `{"preset":"fast","techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true}]}`,
			wantPreset: analyze.PresetFast,
		},
		{
			name: "maximum accuracy keeps its canonical list",
			body: `{"preset":"maximum_accuracy","techniques":[` +
				`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true},` +
				`{"id":"mfcc","enabled":true,"when":"below_confidence","min_confidence":0.15,"min_match_z":0,"z_gate":false}]}`,
			wantPreset: analyze.PresetMaximumAccuracy,
		},
		{
			name: "a raised confidence makes maximum accuracy custom",
			body: `{"preset":"maximum_accuracy","techniques":[` +
				`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true},` +
				`{"id":"mfcc","enabled":true,"when":"below_confidence","min_confidence":0.2,"min_match_z":0,"z_gate":false}]}`,
			wantPreset: analyze.PresetCustom,
		},
		{
			name:       "an absent label is derived from the list",
			body:       `{"techniques":[{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true}]}`,
			wantPreset: analyze.PresetBalanced,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			if err := s.Set(ctx, KeySyncStrategy, json.RawMessage(tc.body)); err != nil {
				t.Fatalf("Set(%s): %v", tc.body, err)
			}
			stored := decode[StrategyOptions](t, mustGet(t, s, ctx, KeySyncStrategy))
			if stored.Preset != tc.wantPreset {
				t.Fatalf("stored preset = %q, want %q", stored.Preset, tc.wantPreset)
			}
			if err := analyze.Strategy(stored).Validate(); err != nil {
				t.Fatalf("stored section does not validate: %v", err)
			}
		})
	}
}

// TestPreset pins the three canonical sections. `fast` is deliberately
// identical to `balanced` (D4a): the paid-for saving is fewer checkpoints, not
// a coarser matcher, and MFCC is never a preset.
//
// The preset covers the whole section — techniques *and* the piecewise block —
// because a preset is one statement about the job. No preset switches
// segmentation on: it is the one setting that changes what a job does to the
// operator's dub copy, so it is only ever on because someone said so, never
// because a stored preset was redefined under them (D5).
func TestPreset(t *testing.T) {
	balanced := Preset(analyze.PresetBalanced)
	fast := Preset(analyze.PresetFast)
	maximum := Preset(analyze.PresetMaximumAccuracy)

	if balanced.Preset != analyze.PresetBalanced {
		t.Fatalf("balanced label = %q, want %q", balanced.Preset, analyze.PresetBalanced)
	}
	if len(balanced.Techniques) != 1 || balanced.Techniques[0].ID != analyze.TechniqueWaveform ||
		balanced.Techniques[0].When != analyze.WhenAlways {
		t.Fatalf("balanced = %+v, want one waveform/always entry", balanced.Techniques)
	}
	if !balanced.Techniques[0].ZGate {
		t.Fatalf("balanced = %+v, want its waveform entry z-gated", balanced.Techniques[0])
	}
	if len(fast.Techniques) != 1 || fast.Techniques[0] != balanced.Techniques[0] || fast.Piecewise != balanced.Piecewise {
		t.Fatalf("fast = %+v, want the balanced section (D4a)", fast)
	}
	if len(maximum.Techniques) != 2 {
		t.Fatalf("maximum_accuracy = %+v, want two entries", maximum.Techniques)
	}
	if maximum.Techniques[1].ID != analyze.TechniqueMFCC || maximum.Techniques[1].When != analyze.WhenBelowConfidence {
		t.Fatalf("maximum_accuracy second entry = %+v, want mfcc/below_confidence", maximum.Techniques[1])
	}
	// The σ gate is per technique: the waveform step keeps its 5σ band, the
	// mel-cepstral step is judged on quality alone (its z is a diagnostic on a
	// different curve), which is why it carries no band to be gated by.
	if !maximum.Techniques[0].ZGate || maximum.Techniques[1].ZGate {
		t.Fatalf("maximum_accuracy gates = %v/%v, want waveform gated and mfcc not",
			maximum.Techniques[0].ZGate, maximum.Techniques[1].ZGate)
	}
	if maximum.Techniques[1].MinConfidence != 0.15 || maximum.Techniques[1].MinMatchZ != 0 {
		t.Fatalf("maximum_accuracy thresholds = %v/%v, want 0.15/0 (quality-gated)",
			maximum.Techniques[1].MinConfidence, maximum.Techniques[1].MinMatchZ)
	}
	// No preset segments: the block is off at the documented numbers in all
	// three, so saving a preset can never start re-encoding a dub copy.
	for _, section := range []analyze.Strategy{balanced, fast, maximum} {
		if section.Piecewise.Enabled {
			t.Fatalf("preset %q segments: D5 makes segmentation opt-in", section.Preset)
		}
		if section.Piecewise != analyze.DefaultPiecewisePolicy() {
			t.Fatalf("preset %q piecewise = %+v, want the documented default", section.Preset, section.Piecewise)
		}
	}
	// The one preset that segments still has to validate as a stored section.
	for _, name := range []string{analyze.PresetFast, analyze.PresetBalanced, analyze.PresetMaximumAccuracy} {
		if err := Preset(name).Validate(); err != nil {
			t.Fatalf("preset %q does not validate: %v", name, err)
		}
	}
	if got := Preset(analyze.PresetCustom); !reflect.DeepEqual(got, analyze.Strategy{}) {
		t.Fatalf("custom = %+v, want the zero section", got)
	}
	// No preset may name MFCC first or alone: D4a forbids a coarser default.
	for _, preset := range []string{analyze.PresetFast, analyze.PresetBalanced} {
		for _, entry := range Preset(preset).Techniques {
			if entry.ID != analyze.TechniqueWaveform {
				t.Fatalf("preset %q names %q, want waveform only", preset, entry.ID)
			}
		}
	}
}

// spaDefaultsPath is the SPA's seeded defaults, relative to this package. A
// Go-only checkout has no frontend directory, which is why the test below skips
// rather than fails when it is absent.
const spaDefaultsPath = "../../../frontend/src/lib/defaults.ts"

// TestDefaultStrategyMatchesTheSPADefault is the cross-language half of D4.
//
// The SPA renders DEFAULT_SETTINGS['sync.strategy'] while the settings load, and
// [PresetTechniques] promises a test that pins that value to the Go default, so
// this is it: the same object literal is decoded through the same schema the
// server saves it with, and must equal Defaults() and pass the Go validation.
//
// The literal is TypeScript, so it is extracted with a brace scan and normalised
// to JSON here — no browser, no npm, and no second copy of the defaults to drift.
func TestDefaultStrategyMatchesTheSPADefault(t *testing.T) {
	source, err := os.ReadFile(spaDefaultsPath)
	if err != nil {
		t.Skipf("SPA defaults unreadable (%v): nothing to compare in a Go-only lane", err)
	}
	literal := spaObjectLiteral(t, string(source), KeySyncStrategy)
	raw := tsObjectLiteralToJSON(t, literal)

	var got StrategyOptions
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the SPA's %s as StrategyOptions: %v (%s)", KeySyncStrategy, err, raw)
	}
	// StrategyOptions.UnmarshalJSON fills an absent key from the Go default, so
	// the same literal is decoded once more without that safety net: a key the
	// SPA forgot must fail below instead of being papered over with the value it
	// was supposed to assert.
	//
	// The comparison is made on normalised sections because the `piecewise`
	// block (#84) is legitimately absent from either side: a SPA build from
	// before the block existed posts a section without it, and the server fills
	// it from the default. Normalising both sides keeps "a key the SPA forgot
	// must fail" true for the techniques list — which is what this test is for —
	// without failing on the one block that has a defined absent meaning.
	plain := normaliseStrategy(decodeStrict[analyze.Strategy](t, raw))
	if !reflect.DeepEqual(normaliseStrategy(analyze.Strategy(got)), plain) {
		t.Fatalf("the SPA's literal is not the whole section:\n with Go defaults %+v\n as written %+v",
			analyze.Strategy(got), plain)
	}

	want, ok := Defaults("")[KeySyncStrategy].(StrategyOptions)
	if !ok {
		t.Fatalf("Defaults()[%q] is %T, want StrategyOptions", KeySyncStrategy, Defaults("")[KeySyncStrategy])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the SPA's %s = %+v, want the Go default %+v", KeySyncStrategy, got, want)
	}
	if err := analyze.Strategy(got).Validate(); err != nil {
		t.Fatalf("the SPA's %s fails the Go validation: %v", KeySyncStrategy, err)
	}
}

// normaliseStrategy is the canonical form the SPA parity test compares: the
// piecewise block at its documented numbers, so a section that omits the block
// and one that writes the defaults are the same statement.
func normaliseStrategy(s analyze.Strategy) analyze.Strategy {
	s.Piecewise = s.Piecewise.Normalised()
	return s
}

// decodeStrict decodes raw into T with unknown fields rejected, mirroring the
// server's own decoder.
func decodeStrict[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if dec.More() {
		t.Fatalf("%s: trailing data", raw)
	}
	return v
}

// spaObjectLiteral returns the object literal that follows key in the SPA's
// DEFAULT_SETTINGS, braces included.
//
// It is a balanced-brace scan that knows about string literals, so a brace in a
// value (or in a comment) cannot end the object early. The scan starts at the
// first `{` after the key and stops at the brace that closes it.
func spaObjectLiteral(t *testing.T, source, key string) string {
	t.Helper()
	marker := "'" + key + "'"
	at := strings.Index(source, marker)
	if at < 0 {
		t.Fatalf("%s: no %s entry", spaDefaultsPath, marker)
	}
	rest := source[at+len(marker):]
	open := strings.Index(rest, "{")
	if open < 0 {
		t.Fatalf("%s: %s is not followed by an object literal", spaDefaultsPath, marker)
	}
	depth := 0
	var quote byte
	for i := open; i < len(rest); i++ {
		c := rest[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open : i+1]
			}
		}
	}
	t.Fatalf("%s: the %s object literal is not closed", spaDefaultsPath, marker)
	return ""
}

// tsObjectLiteralToJSON normalises one TypeScript object literal into JSON:
// unquoted keys are quoted, single-quoted strings become double-quoted, and
// comments and trailing commas are dropped.
//
// Only the constructs DEFAULT_SETTINGS uses are handled. Anything else is left
// as it stands, so json.Unmarshal — not this helper — rejects a literal the
// normaliser did not understand.
func tsObjectLiteralToJSON(t *testing.T, literal string) []byte {
	t.Helper()
	var out strings.Builder
	out.Grow(len(literal))
	for i := 0; i < len(literal); i++ {
		c := literal[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			value, next := tsString(t, literal, i)
			out.WriteString(value)
			i = next - 1
		case c == '/' && i+1 < len(literal) && literal[i+1] == '/':
			for i < len(literal) && literal[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		case c == '/' && i+1 < len(literal) && literal[i+1] == '*':
			end := strings.Index(literal[i+2:], "*/")
			if end < 0 {
				t.Fatalf("unterminated block comment in %s", literal)
			}
			i += end + 3
			out.WriteByte(' ')
		case c == ',':
			// A trailing comma is legal TypeScript and illegal JSON.
			j := i + 1
			for j < len(literal) && isTSSpace(literal[j]) {
				j++
			}
			if j < len(literal) && (literal[j] == '}' || literal[j] == ']') {
				continue
			}
			out.WriteByte(c)
		case isTSIdentStart(c):
			j := i
			for j < len(literal) && isTSIdentPart(literal[j]) {
				j++
			}
			word := literal[i:j]
			k := j
			for k < len(literal) && isTSSpace(literal[k]) {
				k++
			}
			if k < len(literal) && literal[k] == ':' {
				// An unquoted key: JSON wants it quoted.
				out.WriteByte('"')
				out.WriteString(word)
				out.WriteByte('"')
			} else {
				// true, false and null: the only bare words a JSON value allows.
				out.WriteString(word)
			}
			i = j - 1
		default:
			out.WriteByte(c)
		}
	}
	return []byte(out.String())
}

// tsString converts the string literal starting at literal[at] to its JSON form
// and returns it with the index just past the closing quote.
func tsString(t *testing.T, literal string, at int) (string, int) {
	t.Helper()
	quote := literal[at]
	var out strings.Builder
	out.WriteByte('"')
	for i := at + 1; i < len(literal); i++ {
		c := literal[i]
		switch {
		case c == '\\':
			if i+1 >= len(literal) {
				t.Fatalf("unterminated escape in %s", literal)
			}
			if literal[i+1] == '\'' {
				// `\'` is not a JSON escape; the apostrophe is plain there.
				out.WriteByte('\'')
			} else {
				out.WriteByte(c)
				out.WriteByte(literal[i+1])
			}
			i++
		case c == quote:
			out.WriteByte('"')
			return out.String(), i + 1
		case c == '"':
			out.WriteString(`\"`)
		default:
			out.WriteByte(c)
		}
	}
	t.Fatalf("unterminated string in %s", literal)
	return "", len(literal)
}

func isTSSpace(c byte) bool {
	return c == ' ' || c == '	' || c == '\n' || c == '\r'
}

func isTSIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isTSIdentPart(c byte) bool {
	return isTSIdentStart(c) || (c >= '0' && c <= '9')
}

// TestSyncStrategyAlwaysServesThePiecewiseBlock pins the settings-side wire
// shape, which is deliberately the opposite of the report's: the section is the
// document the operator edits, so the UI is served the numbers it would be
// editing even on a fresh install, while a report omits the block for a job that
// never ran it (analyze.Strategy's omitzero).
func TestSyncStrategyAlwaysServesThePiecewiseBlock(t *testing.T) {
	raw, err := json.Marshal(Defaults("")[KeySyncStrategy])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	block, ok := fields["piecewise"]
	if !ok {
		t.Fatalf("the served section has no piecewise block: %s", raw)
	}
	var policy analyze.PiecewisePolicy
	if err := json.Unmarshal(block, &policy); err != nil {
		t.Fatalf("piecewise block is not a policy: %v (%s)", err, block)
	}
	if want := analyze.DefaultPiecewisePolicy(); policy != want {
		t.Fatalf("served piecewise = %+v, want the documented default %+v", policy, want)
	}
}

// TestSyncStrategyPiecewiseRoundTrip: a section the operator switched on comes
// back switched on, with its numbers, and the preset label follows the section.
func TestSyncStrategyPiecewiseRoundTrip(t *testing.T) {
	blob := `{"preset":"maximum_accuracy","techniques":[` +
		`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true},` +
		`{"id":"mfcc","enabled":true,"when":"below_confidence","min_confidence":0.15,"min_match_z":0,"z_gate":false}],` +
		`"piecewise":{"enabled":true,"step_sec":60,"window_sec":60,"split_penalty_ms":400,"min_segment_sec":180,"max_segments":6}}`
	stored := decode[StrategyOptions](t, []byte(blob))
	if err := validateStrategy(&stored); err != nil {
		t.Fatalf("validateStrategy: %v", err)
	}
	want := analyze.PiecewisePolicy{
		Enabled: true, StepSec: 60, WindowSec: 60,
		SplitPenaltyMS: 400, MinSegmentSec: 180, MaxSegments: 6,
	}
	if stored.Piecewise != want {
		t.Fatalf("stored piecewise = %+v, want %+v", stored.Piecewise, want)
	}
	// Turning the block on is an edit like any other, so the label is `custom`:
	// no preset segments on the operator's behalf (D5).
	if stored.Preset != analyze.PresetCustom {
		t.Fatalf("preset = %q, want %q: a section with segmentation on is not any preset",
			stored.Preset, analyze.PresetCustom)
	}

	// Switching it back off — the numbers as well, since the block was tuned —
	// makes the section the canonical `maximum_accuracy` one, piecewise block
	// included: [Preset] ships `piecewise` at the documented default, so a body
	// that restores the default techniques but leaves the block on is
	// legitimately `custom` (asserted above), and one that restores both is
	// `maximum_accuracy`. The label is posted with it because the server never
	// promotes a section: a body still labelled `custom` stays `custom`, which
	// is what makes an edit visible.
	back := stored
	back.Preset = analyze.PresetMaximumAccuracy
	back.Piecewise = analyze.DefaultPiecewisePolicy()
	if err := validateStrategy(&back); err != nil {
		t.Fatalf("validateStrategy: %v", err)
	}
	// Like with like: the restored section is field-for-field the preset's own
	// section, which is the only thing a label is allowed to name.
	if want := Preset(analyze.PresetMaximumAccuracy); !sameStrategy(analyze.Strategy(back), want) {
		t.Fatalf("restored section = %+v, want the canonical %+v", back, want)
	}
	if back.Preset != analyze.PresetMaximumAccuracy {
		t.Fatalf("preset = %q, want %q once the block is back at the default",
			back.Preset, analyze.PresetMaximumAccuracy)
	}
}

// TestSyncStrategyAbsentPiecewiseMaterialises: a row written before #84 has no
// `piecewise` key. Nothing is invented beyond the documented default, and the
// section is stored with the numbers written out — which is what D1 means by
// "no migration".
func TestSyncStrategyAbsentPiecewiseMaterialises(t *testing.T) {
	blob := `{"preset":"balanced","techniques":[` +
		`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true}]}`
	stored := decode[StrategyOptions](t, []byte(blob))
	if err := validateStrategy(&stored); err != nil {
		t.Fatalf("validateStrategy: %v", err)
	}
	if want := analyze.DefaultPiecewisePolicy(); stored.Piecewise != want {
		t.Fatalf("piecewise = %+v, want the documented default %+v", stored.Piecewise, want)
	}
	if stored.Piecewise.Enabled {
		t.Fatal("an absent block came back enabled: segmentation must stay opt-in (D5)")
	}
	if stored.Preset != analyze.PresetBalanced {
		t.Fatalf("preset = %q, want %q", stored.Preset, analyze.PresetBalanced)
	}
}

// TestSyncStrategyPiecewiseRangesRejected: the ranges are enforced on the way in,
// with the section key prefix the settings API returns.
func TestSyncStrategyPiecewiseRangesRejected(t *testing.T) {
	cases := []struct {
		name   string
		block  string
		wantIn string
	}{
		{"step", `{"enabled":true,"step_sec":5}`, "piecewise.step_sec must be 15..300"},
		{"window", `{"enabled":true,"step_sec":45,"window_sec":19}`, "piecewise.window_sec must be 20..190"},
		{"overlap", `{"enabled":true,"step_sec":20,"window_sec":60}`, "piecewise.window_sec must be at most twice step_sec"},
		{"penalty", `{"enabled":true,"split_penalty_ms":10}`, "piecewise.split_penalty_ms must be 50..5000"},
		{"segment", `{"enabled":true,"min_segment_sec":10}`, "piecewise.min_segment_sec must be 30..3600"},
		{"count", `{"enabled":true,"max_segments":1}`, "piecewise.max_segments must be 2..40"},
		{"search bound", `{"enabled":true,"step_sec":90,"window_sec":180}`, "when piecewise is enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob := `{"preset":"custom","techniques":[` +
				`{"id":"waveform","enabled":true,"when":"always","min_confidence":0.1,"min_match_z":5,"z_gate":true}],` +
				`"piecewise":` + tc.block + `}`
			stored := decode[StrategyOptions](t, []byte(blob))
			err := validateStrategy(&stored)
			if err == nil {
				t.Fatalf("validateStrategy(%s) = nil, want an error", tc.block)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantIn)
			}
			if !strings.Contains(err.Error(), KeySyncStrategy) {
				t.Fatalf("error = %v, want it prefixed with %q", err, KeySyncStrategy)
			}
		})
	}
}

// TestVaultNamingDefaultsRenderTheDocumentedPaths pins acceptance criterion 1:
// the seeded patterns render the two example vault paths byte for byte.
func TestVaultNamingDefaultsRenderTheDocumentedPaths(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	raw, ok, err := s.Get(ctx, KeyVaultNaming)
	if err != nil || !ok {
		t.Fatalf("Get(%s) = (ok=%v, err=%v)", KeyVaultNaming, ok, err)
	}
	section := decode[VaultNaming](t, raw)
	if section.MovieTemplate != DefaultMovieTemplate ||
		section.EpisodeTemplate != DefaultEpisodeTemplate ||
		section.SeasonFolder != DefaultSeasonFolder {
		t.Fatalf("seeded section = %+v", section)
	}

	movieTmpl, err := naming.Parse(section.MovieTemplate)
	if err != nil {
		t.Fatalf("Parse(movie_template): %v", err)
	}
	movie := naming.Track{
		Title: "Spirited Away", Year: 2001, Variant: "dubbing", Codec: "eac3",
		Channels: "5.1", LangISO2: "pl",
	}
	stem, err := movieTmpl.Render(movie)
	if err != nil {
		t.Fatalf("render movie: %v", err)
	}
	moviePath := naming.VaultPath("/vault/Spirited Away (2001)", "", movie, stem)
	if want := "/vault/Spirited Away (2001)/Spirited Away (2001) [Dubbing EAC3 5.1].pl.mka"; moviePath != want {
		t.Fatalf("movie path = %q, want %q", moviePath, want)
	}

	episodeTmpl, err := naming.Parse(section.EpisodeTemplate)
	if err != nil {
		t.Fatalf("Parse(episode_template): %v", err)
	}
	seasonTmpl, err := naming.Parse(section.SeasonFolder)
	if err != nil {
		t.Fatalf("Parse(season_folder): %v", err)
	}
	episode := naming.Track{
		Title: "The Office", Year: 2005, Season: 2, Episode: 3,
		EpisodeTitle: "Office Olympics", Variant: "lektor", Codec: "eac3",
		Channels: "2.0", LangISO2: "pl",
	}
	stem, err = episodeTmpl.Render(episode)
	if err != nil {
		t.Fatalf("render episode: %v", err)
	}
	folder, err := seasonTmpl.RenderFolder(episode)
	if err != nil {
		t.Fatalf("render season folder: %v", err)
	}
	episodePath := naming.VaultPath("/vault/The Office (2005)", folder, episode, stem)
	want := "/vault/The Office (2005)/Season 02/The Office - S02E03 - Office Olympics [Lektor EAC3 2.0].pl.mka"
	if episodePath != want {
		t.Fatalf("episode path = %q, want %q", episodePath, want)
	}
}

// TestVaultNamingRejectsLanguageTemplate is acceptance criterion 2: the settings
// write is the gate that refuses {Language}, at the same place every other
// invalid section is refused, and the stored row is untouched.
func TestVaultNamingRejectsLanguageTemplate(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	before, _, err := s.Get(ctx, KeyVaultNaming)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	bad := `{"movie_template":"{Title} ({Year}) {Language}","episode_template":"` +
		DefaultEpisodeTemplate + `","season_folder":"` + DefaultSeasonFolder + `"}`
	err = s.Set(ctx, KeyVaultNaming, json.RawMessage(bad))
	if err == nil {
		t.Fatal("a {Language} pattern was accepted")
	}
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("error = %v, want ErrInvalidValue", err)
	}
	if !strings.Contains(err.Error(), KeyVaultNaming) || !strings.Contains(err.Error(), "{Language}") {
		t.Fatalf("error = %v, want it to name the section and the token", err)
	}

	after, _, err := s.Get(ctx, KeyVaultNaming)
	if err != nil {
		t.Fatalf("Get after refusal: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("stored section changed to %s after a refused write", after)
	}
}

func TestVaultNamingRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	// A blank season folder means "keep episodes beside the show folder" and
	// must round-trip as blank rather than being defaulted back.
	body := `{"movie_template":"{Title} ({Year})","episode_template":"` +
		DefaultEpisodeTemplate + `","season_folder":"  "}`
	if err := s.Set(ctx, KeyVaultNaming, json.RawMessage(body)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, _, err := s.Get(ctx, KeyVaultNaming)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := decode[VaultNaming](t, raw)
	if got.SeasonFolder != "" {
		t.Fatalf("season_folder = %q, want blank", got.SeasonFolder)
	}
	if got.MovieTemplate != "{Title} ({Year})" {
		t.Fatalf("movie_template = %q", got.MovieTemplate)
	}

	// A season folder is one directory name, never a path.
	bad := `{"movie_template":"` + DefaultMovieTemplate + `","episode_template":"` +
		DefaultEpisodeTemplate + `","season_folder":"Season/{Season:00}"}`
	if err := s.Set(ctx, KeyVaultNaming, json.RawMessage(bad)); err == nil {
		t.Fatal("a season folder containing a path separator was accepted")
	}
}

func TestVaultNamingRejectsUnknownField(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.Seed(ctx, t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	body := `{"movie_template":"` + DefaultMovieTemplate + `","episode_template":"` +
		DefaultEpisodeTemplate + `","season_folder":"` + DefaultSeasonFolder + `","language":"pl"}`
	if err := s.Set(ctx, KeyVaultNaming, json.RawMessage(body)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}
