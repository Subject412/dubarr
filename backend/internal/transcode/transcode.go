package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds one ffmpeg encode. Audio-only work is much cheaper than
// a video transcode, but the hang guard is generous because appliances run
// loaded and a feature-length lossless PCM track can take a while to encode.
const DefaultTimeout = 10 * time.Minute

// Typed failures so the sync worker can map them onto a clear job error.
var (
	// ErrTool reports a missing or failing ffmpeg/ffprobe binary.
	ErrTool = errors.New("transcode: media tool failure")
	// ErrEncoder reports that the encoder a rule needs is unavailable.
	ErrEncoder = errors.New("transcode: required encoder is unavailable")
	// ErrInvalidPlan reports a plan that cannot be encoded.
	ErrInvalidPlan = errors.New("transcode: invalid plan")
	// ErrVerify reports that the encoded file does not carry what was asked
	// for (codec, channels or duration). It is a hard job failure.
	ErrVerify = errors.New("transcode: verification failed")
)

// Dependencies configure a Transcoder. All fields are optional.
type Dependencies struct {
	// FFmpegPath/FFprobePath pin the binaries; empty resolves them on PATH.
	FFmpegPath  string
	FFprobePath string
	// LookPath overrides binary discovery (tests).
	LookPath func(string) (string, error)
	// Run executes a tool and returns combined output; nil uses
	// exec.CommandContext.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Timeout bounds one invocation; 0 selects DefaultTimeout.
	Timeout time.Duration
}

// Transcoder runs the single-pass encode and verifies its result.
type Transcoder struct {
	ffmpegPath  string
	ffprobePath string
	lookPath    func(string) (string, error)
	run         func(ctx context.Context, name string, args ...string) ([]byte, error)
	timeout     time.Duration
}

// New builds a Transcoder. A missing tool is reported when it is used, not
// here, so the backend still starts and reports degraded health.
func New(deps Dependencies) *Transcoder {
	t := &Transcoder{
		ffmpegPath:  deps.FFmpegPath,
		ffprobePath: deps.FFprobePath,
		lookPath:    deps.LookPath,
		run:         deps.Run,
		timeout:     deps.Timeout,
	}
	if t.lookPath == nil {
		t.lookPath = exec.LookPath
	}
	if t.run == nil {
		t.run = execRun
	}
	if t.timeout <= 0 {
		t.timeout = DefaultTimeout
	}
	return t
}

// Request is one encode of a copy in the temp folder. Input must already be the
// verified Copy-on-Work artifact; Output must live inside the job's temp folder
// (the caller enforces that, ADR-0002).
type Request struct {
	Input  string
	Output string
	Plan   Plan
	// SourceChannels/SourceDurationMS describe the input when known (from
	// audio_tracks); 0 makes Transcode probe the input itself.
	SourceChannels   int
	SourceDurationMS int64
	// LangISO2/Title are copied onto the encoded stream so a later mux keeps
	// the archived language tag and the configured track title even if the
	// muxer ever stops writing them itself.
	LangISO2 string
	Title    string
	// Segments, when non-empty, is the #84 piecewise render: the dub is cut per
	// segment, each run placed at its absolute position, and the result
	// concatenated in the same single encode ([BuildSegmentedArgs]). Empty means
	// the ordinary single pass ([BuildArgs]).
	Segments []Segment
}

// Result describes a successful encode.
type Result struct {
	Path       string
	Codec      string
	Channels   int
	BitrateBPS int
	DurationMS int64
	// Command is the exact ffmpeg argv (including argv[0]) for job logs.
	Command []string
}

// EncoderAvailable checks the encoder before a job starts, so a missing libopus
// fails with a clear message instead of halfway through an encode. detail
// carries ffmpeg's diagnostic for the log.
func (t *Transcoder) EncoderAvailable(ctx context.Context, encoder string) (bool, string) {
	if strings.TrimSpace(encoder) == "" {
		return false, "no encoder requested"
	}
	bin, err := t.ffmpeg()
	if err != nil {
		return false, err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, runErr := t.run(ctx, bin, "-hide_banner", "-h", "encoder="+encoder)
	// ffmpeg exits 0 even for an unknown encoder ("Codec 'x' is not
	// recognized"), so availability must be read from the banner:
	// "Encoder libopus [libopus Opus]:".
	known := strings.Contains(string(out), "Encoder "+encoder+" ")
	if runErr != nil || !known {
		return false, firstLine(out)
	}
	return true, ""
}

// Transcode runs exactly one ffmpeg encode (tempo + channel/rate conversion +
// codec/bitrate all in one command, see [BuildArgs]) and then re-probes the
// result. A piecewise render cuts, pads and concatenates the segments in that
// same single command instead (see [BuildSegmentedArgs]). A codec/channel/
// duration mismatch is ErrVerify: the caller must fail the job rather than mux
// an unexpected stream.
func (t *Transcoder) Transcode(ctx context.Context, req Request) (Result, error) {
	if !req.Plan.Encode() {
		return Result{}, fmt.Errorf("%w: plan requests no encode (%q)", ErrInvalidPlan, req.Plan.Codec)
	}
	if strings.TrimSpace(req.Input) == "" || strings.TrimSpace(req.Output) == "" {
		return Result{}, fmt.Errorf("%w: input and output are required", ErrInvalidPlan)
	}
	if err := ValidateTempo(req.Plan.TempoFactor); err != nil {
		return Result{}, err
	}
	ffmpeg, err := t.ffmpeg()
	if err != nil {
		return Result{}, err
	}
	probeBin, err := t.ffprobe()
	if err != nil {
		return Result{}, err
	}

	src, err := t.probe(ctx, probeBin, req.Input)
	if err != nil {
		return Result{}, fmt.Errorf("%w: probe input: %v", ErrTool, err)
	}
	channels := req.SourceChannels
	if channels == 0 {
		channels = src.Channels
	}
	// Prefer the freshly probed input duration: the temp folder copy is the
	// artifact ffmpeg actually reads, so a stale legacy audio_tracks.duration_ms
	// must not fail verification.
	durationMS := src.DurationMS
	if durationMS == 0 {
		durationMS = req.SourceDurationMS
	}
	// The requested channel count is only known here when the caller did not
	// resolve it; ProbePlan fills it from the source for verification.
	plan := req.Plan
	outChannels := plan.Channels
	if outChannels == 0 {
		outChannels = channels
	}
	if outChannels == 0 {
		outChannels = src.Channels
	}

	build := BuildArgs
	if len(req.Segments) > 0 {
		build = BuildSegmentedArgs
	}
	args, err := build(req)
	if err != nil {
		return Result{}, err
	}
	if err := t.invoke(ctx, ffmpeg, args...); err != nil {
		return Result{}, err
	}
	if fi, statErr := os.Stat(req.Output); statErr != nil || fi.Size() == 0 {
		return Result{}, fmt.Errorf("%w: ffmpeg produced no output at %s", ErrTool, req.Output)
	}

	got, err := t.probe(ctx, probeBin, req.Output)
	if err != nil {
		return Result{}, fmt.Errorf("%w: probe output: %v", ErrVerify, err)
	}
	expectedDuration := durationMS
	switch {
	case len(req.Segments) > 0:
		// A segmented render's length is what the segments add up to, not the
		// input's: cutting deliberately drops film and padding deliberately adds
		// silence, so comparing against the dub's duration would both hide a
		// wrong cut and reject a right one. The audio stream must be exactly the
		// planned total or the job fails before any mux.
		expectedDuration = SegmentsTotalMS(req.Segments)
	case plan.TempoFactor > 0 && plan.TempoFactor != 1:
		expectedDuration = int64(float64(durationMS) / plan.TempoFactor)
	}
	if err := Verify(got, Expect{
		Codec:      plan.ExpectedCodec(),
		Channels:   outChannels,
		DurationMS: expectedDuration,
	}); err != nil {
		return Result{}, err
	}
	return Result{
		Path:       req.Output,
		Codec:      got.Codec,
		Channels:   got.Channels,
		BitrateBPS: got.BitrateBPS,
		DurationMS: got.DurationMS,
		Command:    append([]string{ffmpeg}, args...),
	}, nil
}

// BuildArgs renders the single ffmpeg invocation. Exported so the single-pass
// precedence can be asserted without running a tool.
//
// Precedence (ADR-0010, Q20): the tempo filter runs first, then channel-layout
// conversion, then the codec/bitrate/sample-rate output options. Everything is
// one encode; the mux path's timing offset is an mkvmerge `--sync` flag, not a
// second audio encode.
func BuildArgs(req Request) ([]string, error) {
	p := req.Plan
	if !p.Encode() {
		return nil, fmt.Errorf("%w: plan requests no encode", ErrInvalidPlan)
	}
	encoder := p.Encoder()
	if encoder == "" {
		return nil, fmt.Errorf("%w: unknown codec %q", ErrInvalidPlan, p.Codec)
	}
	if err := ValidateTempo(p.TempoFactor); err != nil {
		return nil, err
	}
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", req.Input,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-map_metadata", "0",
	}
	var filters []string
	if f := p.TempoFactor; f > 0 && f != 1 {
		filters = append(filters, "atempo="+strconv.FormatFloat(f, 'f', 6, 64))
	}
	if p.ChannelLayout != "" {
		filters = append(filters, "aformat=channel_layouts="+p.ChannelLayout)
	}
	if len(filters) > 0 {
		args = append(args, "-af", strings.Join(filters, ","))
	}
	args = append(args, "-c:a", encoder)
	if p.BitrateBPS > 0 {
		args = append(args, "-b:a", strconv.Itoa(p.BitrateBPS))
	}
	if p.SampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(p.SampleRate))
	}
	if lang := strings.ToLower(strings.TrimSpace(req.LangISO2)); lang != "" {
		args = append(args, "-metadata:s:a:0", "language="+lang)
	}
	if title := strings.TrimSpace(req.Title); title != "" {
		args = append(args, "-metadata:s:a:0", "title="+title)
	}
	args = append(args, "-f", "matroska", req.Output)
	return args, nil
}

// StreamInfo is the subset of ffprobe output verification needs.
type StreamInfo struct {
	Codec      string
	Channels   int
	BitrateBPS int
	DurationMS int64
}

// Expect is the contract an encoded stream must satisfy.
type Expect struct {
	Codec      string
	Channels   int
	DurationMS int64
}

// Verify compares a probed stream against Expect. Duration is allowed to differ
// by max(1 s, 2%) — encoders pad, mkvmerge rounds, and the source duration is
// itself probed.
func Verify(got StreamInfo, want Expect) error {
	if want.Codec != "" && !strings.EqualFold(got.Codec, want.Codec) {
		return fmt.Errorf("%w: codec %q, want %q", ErrVerify, got.Codec, want.Codec)
	}
	if want.Channels > 0 && got.Channels != want.Channels {
		return fmt.Errorf("%w: %d channels, want %d", ErrVerify, got.Channels, want.Channels)
	}
	if want.DurationMS > 0 && got.DurationMS > 0 {
		tolerance := want.DurationMS / 50 // 2%
		if tolerance < 1000 {
			tolerance = 1000
		}
		diff := got.DurationMS - want.DurationMS
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			return fmt.Errorf("%w: duration %d ms, want %d ms (tolerance %d ms)",
				ErrVerify, got.DurationMS, want.DurationMS, tolerance)
		}
	}
	return nil
}

// probeStreamDoc is the ffprobe JSON subset used for verification.
type probeStreamDoc struct {
	Streams []struct {
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
		Channels  int    `json:"channels"`
		BitRate   string `json:"bit_rate"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

func (t *Transcoder) probe(ctx context.Context, bin, path string) (StreamInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, err := t.run(ctx, bin, "-v", "error",
		"-show_entries", "stream=codec_name,codec_type,channels,bit_rate,duration:format=duration,bit_rate",
		"-of", "json", path)
	if err != nil {
		return StreamInfo{}, fmt.Errorf("%v: %s", err, firstLine(out))
	}
	var doc probeStreamDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return StreamInfo{}, fmt.Errorf("decode ffprobe output: %v", err)
	}
	info := StreamInfo{}
	for _, s := range doc.Streams {
		if !strings.EqualFold(s.CodecType, "audio") {
			continue
		}
		info.Codec = s.CodecName
		info.Channels = s.Channels
		info.BitrateBPS = parseInt(s.BitRate)
		info.DurationMS = int64(parseFloat(s.Duration) * 1000)
		break
	}
	if info.DurationMS == 0 {
		info.DurationMS = int64(parseFloat(doc.Format.Duration) * 1000)
	}
	if info.BitrateBPS == 0 {
		// Matroska often omits per-stream bit_rate; for an audio-only .mka the
		// container bitrate is the stream bitrate.
		info.BitrateBPS = parseInt(doc.Format.BitRate)
	}
	return info, nil
}

func (t *Transcoder) ffmpeg() (string, error) {
	if t.ffmpegPath != "" {
		return t.ffmpegPath, nil
	}
	path, err := t.lookPath("ffmpeg")
	if err != nil || path == "" {
		return "", fmt.Errorf("%w: ffmpeg not found on PATH: %v", ErrTool, err)
	}
	t.ffmpegPath = path
	return path, nil
}

func (t *Transcoder) ffprobe() (string, error) {
	if t.ffprobePath != "" {
		return t.ffprobePath, nil
	}
	path, err := t.lookPath("ffprobe")
	if err != nil || path == "" {
		return "", fmt.Errorf("%w: ffprobe not found on PATH: %v", ErrTool, err)
	}
	t.ffprobePath = path
	return path, nil
}

// invoke runs ffmpeg under the hang guard. Unlike mkvmerge, ffmpeg exit code 1
// is not a "warning success": a non-zero exit fails the encode.
func (t *Transcoder) invoke(ctx context.Context, bin string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, err := t.run(ctx, bin, args...)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: ffmpeg timed out after %s", ErrTool, t.timeout)
	}
	return fmt.Errorf("%w: ffmpeg: %v: %s", ErrTool, err, firstLine(out))
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func parseInt(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func firstLine(out []byte) string {
	msg := strings.TrimSpace(string(out))
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
