// Package probe inspects media files with ffprobe (IDEA.md §3.3, PLAN.md M2-4).
//
// It wraps the host ffprobe binary (`ffprobe -show_streams -show_format -of
// json`) and extracts the fields Dubarr needs from every audio stream: codec,
// channel count/layout, language tags, title and disposition flags. Language
// tags are additionally normalised to ISO 639-1 and ISO 639-2/B via the
// built-in table in iso639.go, so callers never have to interpret Matroska's
// mixed `pol`/`eng`/`und` vocabulary themselves.
//
// Results are cached in memory keyed by path + size + mtime (a bounded LRU).
// The cache is deliberately process-local: probe output is derived data, cheap
// to rebuild, and must not outlive the file it describes, so persisting it in
// SQLite would only add staleness risk. Restarting the backend re-probes.
package probe

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrFFprobeUnavailable reports that no ffprobe binary could be resolved on
// PATH. It is a sentinel so callers (health/diagnostics) can distinguish a
// missing tool from a corrupt or unsupported media file.
var ErrFFprobeUnavailable = errors.New("probe: ffprobe not found on PATH")

const (
	// DefaultCacheSize bounds the in-memory probe cache. 256 entries covers
	// a typical approval-queue browsing session while keeping memory trivial.
	DefaultCacheSize = 256
	// DefaultTimeout bounds a single ffprobe invocation. Generous enough for
	// large network-backed files on a loaded appliance, short enough that a
	// wedged child cannot pin a request forever.
	DefaultTimeout = 30 * time.Second
)

// Dependencies configures a Prober. All fields are optional.
type Dependencies struct {
	// FFprobePath pins the ffprobe binary; empty resolves "ffprobe" on PATH.
	FFprobePath string
	// LookPath overrides binary discovery (defaults to exec.LookPath).
	LookPath func(string) (string, error)
	// Run executes the probe command and returns its stdout. It exists so
	// tests (and future sandboxing) can substitute the exec call; nil uses
	// exec.CommandContext.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// CacheSize is the LRU capacity; 0 selects DefaultCacheSize and a
	// negative value disables caching entirely.
	CacheSize int
	// Timeout bounds a single ffprobe run; 0 selects DefaultTimeout.
	Timeout time.Duration
}

// Disposition mirrors the ffprobe stream disposition block, restricted to the
// flags Dubarr's policy and UI care about.
type Disposition struct {
	Default         bool `json:"default"`
	Dub             bool `json:"dub"`
	Original        bool `json:"original"`
	Comment         bool `json:"comment"`
	Forced          bool `json:"forced"`
	HearingImpaired bool `json:"hearing_impaired"`
	VisualImpaired  bool `json:"visual_impaired"`
	Lyrics          bool `json:"lyrics"`
	Karaoke         bool `json:"karaoke"`
	Descriptions    bool `json:"descriptions"`
	Metadata        bool `json:"metadata"`
}

// AudioTrack is one audio stream of a media file.
type AudioTrack struct {
	// Index is the container-wide stream index (ffprobe's `index`), which is
	// what extraction commands address via -map 0:<index>.
	Index         int    `json:"stream_index"`
	Codec         string `json:"codec"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	// Language is the raw tag as reported by ffprobe (e.g. "pol", "eng",
	// "und"), preserved so callers can show what the file actually says.
	Language string `json:"language"`
	// LanguageISO1 is the normalised ISO 639-1 code, or "" if the file has no
	// usable language tag.
	LanguageISO1 string `json:"language_iso1"`
	// LanguageISO2B is the normalised ISO 639-2/B code, "und" when unknown.
	LanguageISO2B string      `json:"language_iso2b"`
	Title         string      `json:"title"`
	Disposition   Disposition `json:"disposition"`
	// BitrateBPS is the stream's nominal bitrate (ffprobe `bit_rate`, bps) and
	// SampleRate its sample rate in Hz. Both are 0 when the container omits
	// them; Transcode Rules treat a configured threshold on an unknown value as
	// non-matching rather than guessing.
	BitrateBPS int `json:"bitrate_bps"`
	SampleRate int `json:"sample_rate"`
}

// IsCommentary reports whether the track is commentary: either the muxer set
// the disposition flag or the title says so (some muxers drop the flag). Both
// the reference resolver and the export policy share this predicate so
// title-only commentary is excluded identically on every path.
func (t AudioTrack) IsCommentary() bool {
	return t.Disposition.Comment || strings.Contains(strings.ToLower(t.Title), "commentary")
}

// MediaInfo is the probe result for one file.
type MediaInfo struct {
	Path       string        `json:"path"`
	Format     string        `json:"format"`
	Duration   time.Duration `json:"-"`
	DurationMS int64         `json:"duration_ms"`
	Size       int64         `json:"size"`
	Tracks     []AudioTrack  `json:"audio_tracks"`
}

// cacheKey identifies a file version. Path+size+mtime is the settled cache
// key (PLAN.md M2-4): touching or replacing a file invalidates its entry.
type cacheKey struct {
	path    string
	size    int64
	modTime int64
}

// Prober is a concurrency-safe ffprobe wrapper with an in-memory LRU cache.
type Prober struct {
	lookPath    func(string) (string, error)
	run         func(ctx context.Context, name string, args ...string) ([]byte, error)
	timeout     time.Duration
	cacheSize   int
	ffprobePath string

	mu      sync.Mutex
	order   *list.List
	entries map[cacheKey]*list.Element
}

// cacheEntry is the LRU element payload; it retains the key so eviction can
// delete the map entry.
type cacheEntry struct {
	key  cacheKey
	info *MediaInfo
}

// New builds a Prober. It never fails for a missing ffprobe: discovery is lazy
// so the backend still starts (and reports degraded health) without the tool.
func New(deps Dependencies) *Prober {
	p := &Prober{
		lookPath:  deps.LookPath,
		run:       deps.Run,
		timeout:   deps.Timeout,
		cacheSize: deps.CacheSize,
		order:     list.New(),
		entries:   make(map[cacheKey]*list.Element),
	}
	if p.lookPath == nil {
		p.lookPath = exec.LookPath
	}
	if p.run == nil {
		p.run = execRun
	}
	if p.timeout <= 0 {
		p.timeout = DefaultTimeout
	}
	if p.cacheSize == 0 {
		p.cacheSize = DefaultCacheSize
	}
	p.ffprobePath = deps.FFprobePath
	return p
}

// Probe returns the audio-track metadata for path, serving a cached result
// when the file's size and mtime are unchanged.
func (p *Prober) Probe(ctx context.Context, path string) (*MediaInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("probe: stat %s: %w", path, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("probe: %s is a directory", path)
	}

	key := cacheKey{path: path, size: st.Size(), modTime: st.ModTime().UnixNano()}
	if info := p.cacheGet(key); info != nil {
		return info, nil
	}

	bin, err := p.ffprobe()
	if err != nil {
		return nil, err
	}
	out, err := p.runProbe(ctx, bin, path)
	if err != nil {
		return nil, err
	}
	info, err := parseFFprobe(path, out)
	if err != nil {
		return nil, err
	}
	info.Size = st.Size()
	p.cachePut(key, info)
	return info, nil
}

// Invalidate drops every cached entry for path (all observed stat versions).
func (p *Prober) Invalidate(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, el := range p.entries {
		if key.path == path {
			p.order.Remove(el)
			delete(p.entries, key)
		}
	}
}

// CacheLen reports the number of cached probe results (diagnostics/tests).
func (p *Prober) CacheLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// ffprobe resolves the binary once and remembers it. A miss is not cached so
// installing ffprobe later (e.g. into a running container) is picked up.
func (p *Prober) ffprobe() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ffprobePath != "" {
		return p.ffprobePath, nil
	}
	path, err := p.lookPath("ffprobe")
	if err != nil || path == "" {
		return "", ErrFFprobeUnavailable
	}
	p.ffprobePath = path
	return path, nil
}

func (p *Prober) runProbe(ctx context.Context, bin, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	args := []string{"-v", "error", "-show_streams", "-show_format", "-of", "json", path}
	out, err := p.run(ctx, bin, args...)
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			if len(msg) > 500 {
				msg = msg[:500]
			}
			return nil, fmt.Errorf("probe: ffprobe %s: %w: %s", path, err, msg)
		}
		return nil, fmt.Errorf("probe: ffprobe %s: %w", path, err)
	}
	return out, nil
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

// ffprobeNumber accepts ffprobe's quoted numeric fields and the bare numbers
// used by fixtures, storing the raw text for parsePositiveInt.
type ffprobeNumber string

func (n *ffprobeNumber) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*n = ""
		return nil
	}
	if raw[0] != '"' {
		*n = ffprobeNumber(raw)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*n = ffprobeNumber(text)
	return nil
}

// ffprobeDoc is the subset of ffprobe's JSON output Dubarr consumes.
type ffprobeDoc struct {
	Streams []struct {
		Index         int    `json:"index"`
		CodecName     string `json:"codec_name"`
		CodecType     string `json:"codec_type"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`
		// BitRate/SampleRate accept either JSON form: ffprobe prints them as
		// quoted strings, while hand-written fixtures often use bare numbers.
		BitRate     ffprobeNumber     `json:"bit_rate"`
		SampleRate  ffprobeNumber     `json:"sample_rate"`
		Tags        map[string]string `json:"tags"`
		Disposition map[string]int    `json:"disposition"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
}

func parseFFprobe(path string, raw []byte) (*MediaInfo, error) {
	var doc ffprobeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("probe: decode ffprobe output for %s: %w", path, err)
	}

	info := &MediaInfo{
		Path:   path,
		Format: doc.Format.FormatName,
		Tracks: []AudioTrack{},
	}
	if d, err := strconv.ParseFloat(doc.Format.Duration, 64); err == nil && d > 0 {
		info.Duration = time.Duration(math.Round(d * float64(time.Second)))
		info.DurationMS = info.Duration.Milliseconds()
	}

	for _, s := range doc.Streams {
		if !strings.EqualFold(s.CodecType, "audio") {
			continue
		}
		tags := lowerTags(s.Tags)
		lang := tags["language"]
		info.Tracks = append(info.Tracks, AudioTrack{
			Index:         s.Index,
			Codec:         s.CodecName,
			Channels:      s.Channels,
			ChannelLayout: s.ChannelLayout,
			Language:      lang,
			LanguageISO1:  ISO639_1(lang),
			LanguageISO2B: ISO639_2B(lang),
			Title:         tags["title"],
			Disposition:   dispositionFrom(s.Disposition),
			BitrateBPS:    parsePositiveInt(string(s.BitRate)),
			SampleRate:    parsePositiveInt(string(s.SampleRate)),
		})
	}
	return info, nil
}

// parsePositiveInt parses an optional numeric field ("" or "N/A" -> 0). A
// non-negative integer is kept; anything else is treated as unknown so callers
// fail closed on a configured threshold instead of guessing zero.
func parsePositiveInt(raw string) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// lowerTags lower-cases tag keys: Matroska writers disagree on LANGUAGE vs
// language and ffprobe passes the container's casing through verbatim.
func lowerTags(tags map[string]string) map[string]string {
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[lowerASCII(k)] = v
	}
	return out
}

func dispositionFrom(d map[string]int) Disposition {
	return Disposition{
		Default:         d["default"] != 0,
		Dub:             d["dub"] != 0,
		Original:        d["original"] != 0,
		Comment:         d["comment"] != 0,
		Forced:          d["forced"] != 0,
		HearingImpaired: d["hearing_impaired"] != 0,
		VisualImpaired:  d["visual_impaired"] != 0,
		Lyrics:          d["lyrics"] != 0,
		Karaoke:         d["karaoke"] != 0,
		Descriptions:    d["descriptions"] != 0,
		Metadata:        d["metadata"] != 0,
	}
}

func (p *Prober) cacheGet(key cacheKey) *MediaInfo {
	if p.cacheSize < 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	el, ok := p.entries[key]
	if !ok {
		return nil
	}
	p.order.MoveToFront(el)
	return cloneInfo(el.Value.(*cacheEntry).info)
}

func (p *Prober) cachePut(key cacheKey, info *MediaInfo) {
	if p.cacheSize < 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if el, ok := p.entries[key]; ok {
		el.Value = &cacheEntry{key: key, info: cloneInfo(info)}
		p.order.MoveToFront(el)
		return
	}
	el := p.order.PushFront(&cacheEntry{key: key, info: cloneInfo(info)})
	p.entries[key] = el
	for p.order.Len() > p.cacheSize {
		back := p.order.Back()
		if back == nil {
			break
		}
		p.order.Remove(back)
		delete(p.entries, back.Value.(*cacheEntry).key)
	}
}

func cloneInfo(in *MediaInfo) *MediaInfo {
	out := *in
	out.Tracks = make([]AudioTrack, len(in.Tracks))
	copy(out.Tracks, in.Tracks)
	return &out
}
