package probe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// requireTool skips the test when a host binary the fixture needs is missing.
func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH: %v", name, err)
	}
	return p
}

// makeFixture renders a short Matroska file with the given audio tracks into
// dir. Each track is a 0.5 s sine tone so the render stays well under a second.
type fixtureTrack struct {
	lang  string
	title string
	freq  string
}

func makeFixture(t *testing.T, dir string, tracks ...fixtureTrack) string {
	t.Helper()
	ffmpeg := requireTool(t, "ffmpeg")
	out := filepath.Join(dir, "fixture.mkv")
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	for _, tr := range tracks {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency="+tr.freq+":duration=0.5")
	}
	args = append(args, "-c:a", "flac")
	for i, tr := range tracks {
		args = append(args, "-map", itoa(i)+":a")
		if tr.lang != "" {
			args = append(args, "-metadata:s:a:"+itoa(i), "language="+tr.lang)
		}
		if tr.title != "" {
			args = append(args, "-metadata:s:a:"+itoa(i), "title="+tr.title)
		}
		disp := "0"
		if i == 0 {
			disp = "default"
		}
		args = append(args, "-disposition:a:"+itoa(i), disp)
	}
	args = append(args, out)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture failed: %v\n%s", err, combined)
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestProbeSyntheticMKV(t *testing.T) {
	p := New(Dependencies{})
	path := makeFixture(t, t.TempDir(),
		fixtureTrack{lang: "pol", title: "Polski dubbing", freq: "440"},
		fixtureTrack{lang: "eng", title: "English 5.1", freq: "880"},
	)

	info, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Path != path {
		t.Errorf("Path = %q, want %q", info.Path, path)
	}
	if info.Format == "" || !strings.Contains(info.Format, "matroska") {
		t.Errorf("Format = %q, want a matroska format name", info.Format)
	}
	if info.Size <= 0 {
		t.Errorf("Size = %d, want > 0", info.Size)
	}
	if info.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", info.Duration)
	}
	if len(info.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2: %+v", len(info.Tracks), info.Tracks)
	}

	first := info.Tracks[0]
	if first.Index != 0 {
		t.Errorf("track 0 Index = %d, want 0", first.Index)
	}
	if first.Codec != "flac" {
		t.Errorf("track 0 Codec = %q, want flac", first.Codec)
	}
	if first.Channels != 1 || first.ChannelLayout != "mono" {
		t.Errorf("track 0 channels = %d/%q, want 1/mono", first.Channels, first.ChannelLayout)
	}
	if first.Language != "pol" || first.LanguageISO1 != "pl" || first.LanguageISO2B != "pol" {
		t.Errorf("track 0 language = %q/%q/%q, want pol/pl/pol",
			first.Language, first.LanguageISO1, first.LanguageISO2B)
	}
	if first.Title != "Polski dubbing" {
		t.Errorf("track 0 Title = %q", first.Title)
	}
	if !first.Disposition.Default {
		t.Error("track 0 should be the default disposition")
	}

	second := info.Tracks[1]
	if second.LanguageISO1 != "en" || second.LanguageISO2B != "eng" {
		t.Errorf("track 1 language = %q/%q, want en/eng", second.LanguageISO1, second.LanguageISO2B)
	}
	if second.Disposition.Default {
		t.Error("track 1 should not be default")
	}
}

func TestProbeUnknownLanguageBecomesUnd(t *testing.T) {
	p := New(Dependencies{})
	path := makeFixture(t, t.TempDir(), fixtureTrack{freq: "440"})

	info, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(info.Tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(info.Tracks))
	}
	tr := info.Tracks[0]
	if tr.Language != "" {
		t.Errorf("Language = %q, want empty", tr.Language)
	}
	if tr.LanguageISO1 != "" || tr.LanguageISO2B != "und" {
		t.Errorf("language = %q/%q, want \"\"/und", tr.LanguageISO1, tr.LanguageISO2B)
	}
}

func TestProbeMissingFile(t *testing.T) {
	p := New(Dependencies{})
	if _, err := p.Probe(context.Background(), filepath.Join(t.TempDir(), "nope.mkv")); err == nil {
		t.Fatal("Probe on a missing file returned nil error")
	}
}

func TestProbeFFprobeUnavailable(t *testing.T) {
	p := New(Dependencies{
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("not really media"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := p.Probe(context.Background(), path)
	if !errors.Is(err, ErrFFprobeUnavailable) {
		t.Fatalf("Probe error = %v, want ErrFFprobeUnavailable", err)
	}
}

// fakeRunner records exec calls and serves canned ffprobe JSON.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	out   []byte
	err   error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return f.out, f.err
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const fakeProbeJSON = `{
  "streams": [
    {"index":0,"codec_name":"aac","codec_type":"audio","channels":2,"channel_layout":"stereo",
     "tags":{"language":"pol","title":"Dubbing"},"disposition":{"default":1,"dub":0}},
    {"index":1,"codec_name":"ac3","codec_type":"video","channels":0,"tags":{"language":"eng"}}
  ],
  "format":{"format_name":"matroska,webm","duration":"123.456000","size":"42"}
}`

func TestProbeCacheHitOnSameStat(t *testing.T) {
	fr := &fakeRunner{out: []byte(fakeProbeJSON)}
	p := New(Dependencies{Run: fr.run})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		info, err := p.Probe(context.Background(), path)
		if err != nil {
			t.Fatalf("Probe #%d: %v", i, err)
		}
		if len(info.Tracks) != 1 {
			t.Fatalf("got %d audio tracks, want 1", len(info.Tracks))
		}
		if info.Tracks[0].LanguageISO1 != "pl" {
			t.Errorf("language = %q, want pl", info.Tracks[0].LanguageISO1)
		}
		if info.Duration != 123456*time.Millisecond {
			t.Errorf("Duration = %v, want 123.456s", info.Duration)
		}
	}
	if got := fr.count(); got != 1 {
		t.Errorf("ffprobe ran %d times, want 1 (cache hit)", got)
	}
	if got := p.CacheLen(); got != 1 {
		t.Errorf("cache entries = %d, want 1", got)
	}
}

func TestProbeCacheInvalidatedByMtimeAndSize(t *testing.T) {
	fr := &fakeRunner{out: []byte(fakeProbeJSON)}
	p := New(Dependencies{Run: fr.run})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	// Same size, different mtime.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if got := fr.count(); got != 2 {
		t.Fatalf("ffprobe ran %d times after mtime change, want 2", got)
	}

	// Different size, same mtime.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("media-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if got := fr.count(); got != 3 {
		t.Fatalf("ffprobe ran %d times after size change, want 3", got)
	}
}

func TestProbeCacheLRUEviction(t *testing.T) {
	fr := &fakeRunner{out: []byte(fakeProbeJSON)}
	p := New(Dependencies{Run: fr.run, CacheSize: 1})
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mkv")
	b := filepath.Join(dir, "b.mkv")
	for _, path := range []string{a, b} {
		if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Probe(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := fr.count(); got != 3 {
		t.Errorf("ffprobe ran %d times, want 3 (a evicted by b)", got)
	}
}

func TestProbeInvalidate(t *testing.T) {
	fr := &fakeRunner{out: []byte(fakeProbeJSON)}
	p := New(Dependencies{Run: fr.run})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	p.Invalidate(path)
	if _, err := p.Probe(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if got := fr.count(); got != 2 {
		t.Errorf("ffprobe ran %d times, want 2 after Invalidate", got)
	}
}

func TestProbeFailureAndBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("exit status 1")
	p := New(Dependencies{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("boom"), boom
	}})
	if _, err := p.Probe(context.Background(), path); err == nil || !errors.Is(err, boom) {
		t.Errorf("Probe error = %v, want wrapping %v", err, boom)
	}
	if got := p.CacheLen(); got != 0 {
		t.Errorf("cache entries = %d, want 0 after failure", got)
	}

	bad := New(Dependencies{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("<html>nope</html>"), nil
	}})
	if _, err := bad.Probe(context.Background(), path); err == nil {
		t.Error("Probe accepted invalid JSON")
	}
}

func TestProbeCommandShape(t *testing.T) {
	fr := &fakeRunner{out: []byte(fakeProbeJSON)}
	p := New(Dependencies{FFprobePath: "/opt/bin/ffprobe", Run: fr.run})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	fr.mu.Lock()
	call := fr.calls[0]
	fr.mu.Unlock()
	for _, want := range []string{"/opt/bin/ffprobe", "-show_streams", "-show_format", "-of json", path} {
		if !strings.Contains(call, want) {
			t.Errorf("ffprobe call %q lacks %q", call, want)
		}
	}
}

// TestProbeZeroAudioTracksIsEmptySlice keeps the JSON contract stable: a file
// with no audio stream must emit audio_tracks: [] and never null.
func TestProbeZeroAudioTracksIsEmptySlice(t *testing.T) {
	const videoOnly = `{
	  "streams": [
	    {"index":0,"codec_name":"h264","codec_type":"video","channels":0}
	  ],
	  "format":{"format_name":"matroska,webm","duration":"10.000000"}
	}`
	p := New(Dependencies{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(videoOnly), nil
	}})
	path := filepath.Join(t.TempDir(), "silent.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}

	info, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Tracks == nil {
		t.Fatal("Tracks is nil for a zero-audio file, want an empty slice")
	}
	if len(info.Tracks) != 0 {
		t.Fatalf("got %d tracks, want 0: %+v", len(info.Tracks), info.Tracks)
	}

	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"audio_tracks":[]`) {
		t.Fatalf("JSON = %s, want audio_tracks:[]", raw)
	}

	// The cache-hit path must preserve the empty-slice contract too; a clone
	// built with append(nil, ...) would turn a zero-length slice into nil.
	cached, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("cached Probe: %v", err)
	}
	if cached.Tracks == nil {
		t.Fatal("cached Tracks is nil, want an empty slice")
	}
	cachedRaw, err := json.Marshal(cached)
	if err != nil {
		t.Fatalf("cached Marshal: %v", err)
	}
	if !strings.Contains(string(cachedRaw), `"audio_tracks":[]`) {
		t.Fatalf("cached JSON = %s, want audio_tracks:[]", cachedRaw)
	}
}

func TestProbeReturnsCopyOfCachedResult(t *testing.T) {
	fr := &fakeRunner{out: []byte(fakeProbeJSON)}
	p := New(Dependencies{Run: fr.run})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	first.Tracks[0].Title = "mutated"
	second, err := p.Probe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tracks[0].Title != "Dubbing" {
		t.Errorf("cached Title = %q, want Dubbing (mutation leaked)", second.Tracks[0].Title)
	}
}
