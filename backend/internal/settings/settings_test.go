package settings

import (
	"context"
	"encoding/json"
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
	if len(tf.RequiredFilenameTags) != 1 || tf.RequiredFilenameTags[0] != "[ArcA310]" {
		t.Errorf("required tags = %v, want [ArcA310]", tf.RequiredFilenameTags)
	}
	if tf.TagMatchMode != "ANY" || tf.TagCaseSensitive || !tf.EnforceTagFilter {
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
	if !lp.ExcludeCommentary || !lp.OriginalLanguageFromArr || lp.ExportMode != "rule_based" {
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
