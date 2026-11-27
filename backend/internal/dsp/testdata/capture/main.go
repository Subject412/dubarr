//go:build ignore

// Command capture records the Rust engine's numbers as committed goldens, so
// the pure-Go DSP port (P-DSP) can be proved 1:1 against the reference engine
// before `sync-engine/` is deleted (U-DSP.1).
//
// It is a developer tool, never compiled by `go build ./...`: the
// `//go:build ignore` tag excludes it from every package build, and it lives
// under `testdata/`, which the go tool does not descend into. Run it with an
// explicit file argument:
//
//	cd backend && go run ./internal/dsp/testdata/capture/main.go
//	cd backend && go run ./internal/dsp/testdata/capture/main.go -check
//	cd backend && go run ./internal/dsp/testdata/capture/main.go -write-window-fixture /tmp/win
//
// `-write-window-fixture` is not a capture at all: it emits the synthetic
// `start_time = 0.5 s` MKV and its full-decode reference WAV that U-WIN.3's seek
// correction is proved against, and exits. Nothing is committed from it — the
// window-decode test builds the same pair for itself — so it exists for
// inspecting the fixture by hand.
//
// Two tiers are captured (see GOLDEN.md):
//
//   - Tier A, synthetic: the six scenarios of `sync-engine/tests/golden.rs`
//     (five `wav_case` programs plus the stereo-mixdown case). The program is
//     produced by the Rust harness's *own* generator — this tool copies
//     `sync-engine/` into a scratch directory, drops the sibling `emit.rs` in
//     as `tests/emit.rs` and runs `cargo test --test emit`. Nothing here
//     re-implements the generator, so the goldens cannot drift from it.
//   - Tier B, real material: for the Shrek pair (same-language band) and the
//     Minions pair (cross-language low-pass band), the production decode argv
//     from `syncjob.go` is run verbatim over each fixture, a short excerpt per
//     checkpoint is cut and `dubarr-sync` measures it.
//
// Every golden records the full `SyncResult` plus provenance (Rust commit,
// Cargo.lock digest, engine/ffmpeg versions, host, date, argv). `-check`
// re-runs the whole capture into a scratch directory and exits non-zero if any
// number, excerpt byte or GOLDEN.md row differs from what is committed.
//
// The committed WAVs are trimmed to what the engine actually reads (see the
// size section of GOLDEN.md): the analysed samples are identical either way,
// and the repository's 8 MB / 640 KB testdata budget leaves no room for
// full-length files. This tool measures the *committed* files, never a scratch
// copy, so a golden always describes bytes that exist in the repository.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// syntheticTrimSec is how much of each generator WAV is committed. The
	// harness analyses [slice_start_sec, slice_start_sec+slice_len_sec) =
	// [5,15) s of these files, so the tail past 15 s is never read and
	// truncating it cannot move a number; it only keeps the committed set
	// inside the size budget.
	syntheticTrimSec = 15.0

	// defaultExcerptSec is the Tier B excerpt length. The plan asks for 30 s,
	// which does not fit the committed-testdata budget in any encoding
	// (30 s x 8 kHz x 2 B x 24 files = 11.5 MB on its own, before the synthetic
	// tier). GOLDEN.md records the arithmetic; this length is what the 8 MB /
	// 640 KB caps allow with headroom.
	defaultExcerptSec = 10.0

	// lowQualityThreshold mirrors analyze.LowQualityThreshold: the margin
	// column in GOLDEN.md is measured against it.
	lowQualityThreshold = 0.10
	// thinQuality is the plan's sign-off marker: a checkpoint below it is
	// flagged **thin** in GOLDEN.md.
	thinQuality = 0.15

	engineName = "rust dubarr-sync (reference engine, sync-engine/)"
)

// ---------------------------------------------------------------------------
// recorded shapes

type provenance struct {
	RustCommit        string   `json:"rust_commit"`
	CargoLockSHA256   string   `json:"cargo_lock_sha256"`
	DubarrSyncVersion string   `json:"dubarr_sync_version"`
	FFmpegVersion     string   `json:"ffmpeg_version"`
	Host              string   `json:"host"`
	Date              string   `json:"date"`
	Argv              argvSpec `json:"argv"`
}

type argvSpec struct {
	Emit    []string     `json:"emit"`
	Decode  []decodeArgv `json:"decode"`
	Excerpt []string     `json:"excerpt"`
	Sync    []string     `json:"sync"`
}

type decodeArgv struct {
	Source string   `json:"source"`
	Stream int      `json:"stream"`
	Argv   []string `json:"argv"`
}

type golden struct {
	Case       string     `json:"case"`
	Tier       string     `json:"tier"`
	Engine     string     `json:"engine"`
	Provenance provenance `json:"provenance"`
	Expected   *expected  `json:"expected,omitempty"`
	Entries    []entry    `json:"entries"`
}

// expected carries the assertion the Rust suite already makes for a synthetic
// case (tests/golden.rs), so the Go port can reuse it verbatim.
type expected struct {
	OffsetMS    float64 `json:"offset_ms"`
	ToleranceMS float64 `json:"tolerance_ms"`
	MinQuality  float64 `json:"min_quality"`
}

type entry struct {
	ID             string     `json:"id"`
	PositionPct    int        `json:"position_pct,omitempty"`
	SourceStartSec float64    `json:"source_start_sec,omitempty"`
	TargetWAV      string     `json:"target_wav"`
	DonorWAV       string     `json:"donor_wav"`
	TargetSHA256   string     `json:"target_wav_sha256"`
	DonorSHA256    string     `json:"donor_wav_sha256"`
	Payload        string     `json:"payload"`
	Result         syncResult `json:"result"`
	MarginTo010    float64    `json:"margin_to_0_10"`
	Thin           bool       `json:"thin"`
}

// syncResult is the CLI contract, one JSON object on stdout (job.rs).
type syncResult struct {
	OffsetMS           float64  `json:"offset_ms"`
	CorrelationQuality float64  `json:"correlation_quality"`
	SampleRate         uint32   `json:"sample_rate"`
	Analyzed           analyzed `json:"analyzed"`
}

type analyzed struct {
	Start float64 `json:"start"`
	Len   float64 `json:"len"`
}

// ---------------------------------------------------------------------------
// scenario tables (mirrors of tests/golden.rs and analyze.DefaultCheckpointPositionsPct)

type synthCase struct {
	name        string
	offsetMS    float64
	toleranceMS float64
	minQuality  float64
	sliceStart  float64
	sliceLen    float64
	window      float64
}

// Order and values are tests/golden.rs verbatim, including the assertion
// tolerances, so the Go port inherits the Rust suite's own bounds.
var synthCases = []synthCase{
	{"pos1500", 1500, 1.0, 0.5, 5.0, 10.0, 5.5},
	{"neg800", -800, 1.0, 0.5, 5.0, 10.0, 5.5},
	{"pos5s", 5000, 1.0, 0.3, 5.0, 10.0, 5.0},
	{"neg5s", -5000, 1.0, 0.3, 5.0, 10.0, 5.0},
	{"noise6db", 1500, 20.0, 0.15, 5.0, 10.0, 5.5},
	{"stereo", 1000, 2.0, 0.4, 5.0, 10.0, 2.0},
}

// checkpointPcts is analyze.DefaultCheckpointPositionsPct.
var checkpointPcts = []int{10, 25, 40, 55, 70, 85}

type fixture struct {
	name   string
	method string
	filter string
	target fixtureSource
	donor  fixtureSource
}

type fixtureSource struct {
	file   string
	stream int
}

// The pairs the production pipeline resolves for these fixtures: the target's
// English track against the donor's English track (Shrek: same language,
// reflang priority 1/2) and against the Polish dub (Minions: no shared
// reference language, low-pass fallback). Stream indices are what ffprobe
// reports for each container.
var fixtures = []fixture{
	{
		name:   "shrek",
		method: "same_lang_reference",
		filter: "highpass=f=80,lowpass=f=3500",
		target: fixtureSource{"Shrek (2001) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt0126029]-FraMeSToR.mkv", 1},
		donor:  fixtureSource{"Shrek.2001.1080p.SKST.WEB-DL.MULTi.DDP5.1.H.264-KIFR.mkv", 2},
	},
	{
		name:   "minions",
		method: "lowpass_fallback",
		filter: "highpass=f=30,lowpass=f=200",
		target: fixtureSource{"Minions and Monsters (2026) - [ArcA310][Remux-2160p][HDR10][Opus 7.1][AV1][imdb-tt32890033]-FraMeSToR.mkv", 1},
		donor:  fixtureSource{"Minions and Monsters 2026 PL.DUB.MD.1080p.AMZN.WEB-DL.XviD-MAXX.mp4", 1},
	},
}

// ---------------------------------------------------------------------------
// main

type options struct {
	root      string
	syncBin   string
	ffmpeg    string
	testdata  string
	wavDir    string
	goldenDir string
	scratch   string
	excerpt   float64
	check     bool
	keep      bool
}

func main() {
	root := flag.String("repo", "", "repository root (default: derived from this file's path)")
	syncBin := flag.String("sync", "", "path to the Rust dubarr-sync binary (default: <repo>/sync-engine/target/release/dubarr-sync)")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary")
	check := flag.Bool("check", false, "re-run and exit non-zero if anything drifts from the committed goldens")
	out := flag.String("out", "", "scratch directory (default: a fresh directory beside the module)")
	excerpt := flag.Float64("excerpt", defaultExcerptSec, "Tier B excerpt length in seconds")
	keep := flag.Bool("keep", false, "keep the scratch directory")
	fixtureDir := flag.String("write-window-fixture", "", "write the synthetic start_time=0.5 s MKV and its full-decode reference WAV into this directory, then exit (needs ffmpeg and mkvmerge)")
	flag.Parse()

	if *fixtureDir != "" {
		if err := writeWindowFixture(*ffmpeg, *fixtureDir); err != nil {
			fmt.Fprintf(os.Stderr, "capture: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*root, *syncBin, *ffmpeg, *check, *out, *excerpt, *keep); err != nil {
		fmt.Fprintf(os.Stderr, "capture: %v\n", err)
		os.Exit(1)
	}
}

func run(rootArg, syncArg, ffmpeg string, check bool, out string, excerptSec float64, keep bool) error {
	root, err := repoRoot(rootArg)
	if err != nil {
		return err
	}
	testdata := filepath.Join(root, "backend", "internal", "dsp", "testdata")
	committedWAV := committedWAVDir(root)
	committedGolden := filepath.Join(testdata, "golden")

	if out == "" {
		out, err = os.MkdirTemp("", "dubarr-golden-")
		if err != nil {
			return err
		}
	} else if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(out)
	}

	opts := options{
		root:      root,
		ffmpeg:    ffmpeg,
		testdata:  testdata,
		scratch:   out,
		excerpt:   excerptSec,
		check:     check,
		keep:      keep,
		goldenDir: committedGolden,
	}
	if syncArg == "" {
		syncArg = filepath.Join(root, "sync-engine", "target", "release", "dubarr-sync")
	}
	if _, err := os.Stat(syncArg); err != nil {
		return fmt.Errorf("dubarr-sync binary %s: %w (build it with `cd sync-engine && cargo build --release`)", syncArg, err)
	}
	opts.syncBin = syncArg
	if opts.wavDir = committedWAV; check {
		opts.wavDir = filepath.Join(out, "wav")
	}
	if err := os.MkdirAll(opts.wavDir, 0o755); err != nil {
		return err
	}

	prov, err := collectProvenance(opts)
	if err != nil {
		return err
	}
	goldens, err := captureAll(opts, prov)
	if err != nil {
		return err
	}

	if !check {
		if err := writeGoldens(committedGolden, goldens); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(testdata, "GOLDEN.md"), []byte(renderGoldenMD(goldens)), 0o644); err != nil {
			return err
		}
		printTable(goldens)
		total, err := testdataSize(testdata)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "capture: wrote %d golden files, testdata is %d bytes (%.2f MiB)\n",
			len(goldens), total, float64(total)/(1<<20))
		return nil
	}

	printTable(goldens)
	return checkCommitted(opts, testdata, committedGolden, goldens)
}

// ---------------------------------------------------------------------------
// capture driver

func captureAll(opts options, prov provenance) ([]golden, error) {
	var all []golden

	synthetic, err := captureSynthetic(opts, prov)
	if err != nil {
		return nil, err
	}
	all = append(all, synthetic...)

	real, err := captureReal(opts, prov)
	if err != nil {
		return nil, err
	}
	all = append(all, real...)

	return all, nil
}

func captureSynthetic(opts options, prov provenance) ([]golden, error) {
	genDir := filepath.Join(opts.scratch, "synthetic")
	fmt.Fprintf(os.Stderr, "capture: generating the synthetic tier with the Rust harness generator into %s\n", genDir)
	if err := os.RemoveAll(genDir); err != nil {
		return nil, err
	}
	if err := runEmitter(opts, prov.Argv.Emit); err != nil {
		return nil, err
	}

	out := make([]golden, 0, len(synthCases))
	for _, sc := range synthCases {
		target := filepath.Join(opts.wavDir, sc.name+"-target.wav")
		donor := filepath.Join(opts.wavDir, sc.name+"-donor.wav")
		srcTarget := filepath.Join(genDir, sc.name, "target.wav")
		srcDonor := filepath.Join(genDir, sc.name, "donor.wav")

		if err := commitWAV(srcTarget, target, syntheticTrimSec); err != nil {
			return nil, fmt.Errorf("%s target: %w", sc.name, err)
		}
		if err := commitWAV(srcDonor, donor, syntheticTrimSec); err != nil {
			return nil, fmt.Errorf("%s donor: %w", sc.name, err)
		}

		e, err := measure(opts, entryInput{
			id:           sc.name,
			target:       target,
			donor:        donor,
			recordTarget: relTo(opts.root, filepath.Join(committedWAVDir(opts.root), sc.name+"-target.wav")),
			recordDonor:  relTo(opts.root, filepath.Join(committedWAVDir(opts.root), sc.name+"-donor.wav")),
			sliceStart:   sc.sliceStart,
			sliceLen:     sc.sliceLen,
			window:       sc.window,
		})
		if err != nil {
			return nil, fmt.Errorf("synthetic %s: %w", sc.name, err)
		}

		out = append(out, golden{
			Case:       "synthetic/" + sc.name,
			Tier:       "synthetic",
			Engine:     engineName,
			Provenance: prov,
			Expected: &expected{
				OffsetMS:    sc.offsetMS,
				ToleranceMS: sc.toleranceMS,
				MinQuality:  sc.minQuality,
			},
			Entries: []entry{e},
		})
	}
	return out, nil
}

func captureReal(opts options, prov provenance) ([]golden, error) {
	fullDir := filepath.Join(opts.scratch, "full")
	if err := os.MkdirAll(fullDir, 0o755); err != nil {
		return nil, err
	}
	fixturesDir := filepath.Join(opts.root, "testfiles")

	var out []golden
	for _, fx := range fixtures {
		targetIn := filepath.Join(fixturesDir, fx.target.file)
		donorIn := filepath.Join(fixturesDir, fx.donor.file)
		for _, in := range []string{targetIn, donorIn} {
			if _, err := os.Stat(in); err != nil {
				return nil, fmt.Errorf("fixture %s: %w (the real-material tier needs testfiles/)", filepath.Base(in), err)
			}
		}

		fullTarget := filepath.Join(fullDir, fx.name+"-target.wav")
		fullDonor := filepath.Join(fullDir, fx.name+"-donor.wav")
		fmt.Fprintf(os.Stderr, "capture: decoding %s (production argv, %s)\n", fx.name, fx.filter)
		if err := runFFmpeg(opts.ffmpeg, decodeArgs(targetIn, fx.target.stream, fx.filter, fullTarget)); err != nil {
			return nil, fmt.Errorf("%s decode target: %w", fx.name, err)
		}
		if err := runFFmpeg(opts.ffmpeg, decodeArgs(donorIn, fx.donor.stream, fx.filter, fullDonor)); err != nil {
			return nil, fmt.Errorf("%s decode donor: %w", fx.name, err)
		}

		tFrames, tRate, err := wavFrames(fullTarget)
		if err != nil {
			return nil, err
		}
		dFrames, dRate, err := wavFrames(fullDonor)
		if err != nil {
			return nil, err
		}
		if tRate != dRate {
			return nil, fmt.Errorf("%s: decoded rates differ: %d vs %d", fx.name, tRate, dRate)
		}
		// analyze.Analyze derives the timeline from min(target, donor).
		duration := float64(min64(tFrames, dFrames)) / float64(tRate)

		g := golden{
			Case:       fx.name,
			Tier:       "real",
			Engine:     engineName,
			Provenance: prov,
		}
		for _, pct := range checkpointPcts {
			start, length := sliceWindow(duration, pct, opts.excerpt)
			id := fmt.Sprintf("%s@%d%%", fx.name, pct)
			target := filepath.Join(opts.wavDir, fmt.Sprintf("%s-cp%02d-target.wav", fx.name, pct))
			donor := filepath.Join(opts.wavDir, fmt.Sprintf("%s-cp%02d-donor.wav", fx.name, pct))

			if err := cutExcerpt(opts.ffmpeg, fullTarget, target, start, length); err != nil {
				return nil, fmt.Errorf("%s: %w", id, err)
			}
			if err := cutExcerpt(opts.ffmpeg, fullDonor, donor, start, length); err != nil {
				return nil, fmt.Errorf("%s: %w", id, err)
			}

			e, err := measure(opts, entryInput{
				id:           id,
				positionPct:  pct,
				sourceStart:  start,
				target:       target,
				donor:        donor,
				recordTarget: relTo(opts.root, filepath.Join(committedWAVDir(opts.root), filepath.Base(target))),
				recordDonor:  relTo(opts.root, filepath.Join(committedWAVDir(opts.root), filepath.Base(donor))),
				sliceStart:   0,
				sliceLen:     length,
				window:       length,
			})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", id, err)
			}
			g.Entries = append(g.Entries, e)
		}
		out = append(out, g)
	}
	return out, nil
}

type entryInput struct {
	id          string
	positionPct int
	sourceStart float64
	// target/donor are the files the engine is run on; recordTarget/recordDonor
	// are the repository-relative paths written into the golden. They differ
	// only under -check, where the engine runs on regenerated scratch copies
	// while the recorded payload keeps describing the committed file.
	target       string
	donor        string
	recordTarget string
	recordDonor  string
	sliceStart   float64
	sliceLen     float64
	window       float64
}

// measure runs one job through the reference engine and records it.
func measure(opts options, in entryInput) (entry, error) {
	payload, err := jobPayload(in.recordTarget, in.recordDonor, in.sliceStart, in.sliceLen, in.window)
	if err != nil {
		return entry{}, err
	}
	run, err := jobPayload(in.target, in.donor, in.sliceStart, in.sliceLen, in.window)
	if err != nil {
		return entry{}, err
	}
	res, err := runSync(opts.syncBin, run)
	if err != nil {
		return entry{}, err
	}
	tSHA, err := fileSHA256(in.target)
	if err != nil {
		return entry{}, err
	}
	dSHA, err := fileSHA256(in.donor)
	if err != nil {
		return entry{}, err
	}
	return entry{
		ID:             in.id,
		PositionPct:    in.positionPct,
		SourceStartSec: round6(in.sourceStart),
		TargetWAV:      in.recordTarget,
		DonorWAV:       in.recordDonor,
		TargetSHA256:   tSHA,
		DonorSHA256:    dSHA,
		Payload:        string(payload),
		Result:         res,
		MarginTo010:    round3(res.CorrelationQuality - lowQualityThreshold),
		Thin:           res.CorrelationQuality < thinQuality,
	}, nil
}

// jobPayload builds the stdin JSON the engine contract expects (job.rs). Paths
// are passed through unchanged, so the recorded payload can carry
// repository-relative paths while the run carries resolved ones.
func jobPayload(target, donor string, sliceStart, sliceLen, window float64) ([]byte, error) {
	body := struct {
		Target struct {
			Path string `json:"path"`
		} `json:"target"`
		Donor struct {
			Path string `json:"path"`
		} `json:"donor"`
		SliceStartSec float64  `json:"slice_start_sec"`
		SliceLenSec   float64  `json:"slice_len_sec"`
		WindowSec     *float64 `json:"window_sec"`
	}{}
	body.Target.Path = target
	body.Donor.Path = donor
	body.SliceStartSec = round6(sliceStart)
	body.SliceLenSec = round6(sliceLen)
	w := round6(window)
	body.WindowSec = &w
	return marshalJSON(body, "")
}

// marshalJSON encodes without HTML escaping, so the recorded argv keeps `<>`
// placeholders readable.
func marshalJSON(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// runSync feeds one job to the reference engine: one JSON object in, one out.
func runSync(bin string, payload []byte) (syncResult, error) {
	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return syncResult{}, fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), payload, err, strings.TrimSpace(stderr.String()))
	}
	var res syncResult
	text := bytes.TrimSpace(stdout.Bytes())
	if err := json.Unmarshal(text, &res); err != nil {
		return syncResult{}, fmt.Errorf("dubarr-sync stdout is not the result object (%w): %q", err, text)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Rust generator

// runEmitter copies sync-engine/ into the scratch directory, adds the emitter
// as a `harness = false` test target and runs it. The copied crate is never
// written back, so `sync-engine/` stays untouched.
func runEmitter(opts options, emitArgv []string) error {
	genDir := filepath.Join(opts.scratch, "engine")
	if err := os.RemoveAll(genDir); err != nil {
		return err
	}
	src := filepath.Join(opts.root, "sync-engine")
	if err := copyTree(filepath.Join(src, "src"), filepath.Join(genDir, "src")); err != nil {
		return err
	}
	if err := copyTree(filepath.Join(src, "tests"), filepath.Join(genDir, "tests")); err != nil {
		return err
	}
	for _, name := range []string{"Cargo.toml", "Cargo.lock"} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(genDir, name)); err != nil {
			return err
		}
	}
	manifest := filepath.Join(genDir, "Cargo.toml")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	stanza := "\n[[test]]\nname = \"emit\"\nharness = false\npath = \"tests/emit.rs\"\n"
	if !bytes.Contains(raw, []byte("name = \"emit\"")) {
		raw = append(raw, []byte(stanza)...)
	}
	if err := os.WriteFile(manifest, raw, 0o644); err != nil {
		return err
	}
	emitter, err := os.ReadFile(filepath.Join(opts.testdata, "capture", "emit.rs"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(genDir, "tests", "emit.rs"), emitter, 0o644); err != nil {
		return err
	}

	synthetic := filepath.Join(opts.scratch, "synthetic")
	if err := os.MkdirAll(synthetic, 0o755); err != nil {
		return err
	}
	args := substituteScratch(emitArgv, opts.scratch)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = genDir
	cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+filepath.Join(opts.scratch, "target"))
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cargo emit failed (%v):\n%s", err, stderr.String())
	}
	return nil
}

// ---------------------------------------------------------------------------
// ffmpeg

// decodeArgs is syncjob.decodeAudio's argv, verbatim: -ac 1 -ar 8000,
// the analysis band filter, and pcm_f32le output.
func decodeArgs(input string, stream int, filter, output string) []string {
	return []string{
		"-v", "error", "-nostdin", "-y",
		"-i", input,
		"-map", "0:" + strconv.Itoa(stream),
		"-ac", "1", "-ar", "8000",
		"-af", filter,
		"-c:a", "pcm_f32le",
		output,
	}
}

// runFFmpeg runs one external command and folds its stderr into the error. The
// name predates the fixture mode, which also uses it for mkvmerge.
func runFFmpeg(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// writeWindowFixture emits the material for U-WIN.3's start_time control: an
// MKV whose video starts at container 0 and whose audio starts at 0.5 s
// (`start_time = 0.5`), plus the full decode of that audio track, which is the
// reference a correctly-corrected window must land on.
//
// The audio is PCM at the analysis rate, so a window decode differs from the
// reference in nothing but where it started — a lossy codec's seek behaviour
// would blur exactly the property the control measures. mkvmerge's
// `--sync 0:500` shifts the *audio input's* only track (track IDs are numbered
// per input file) to a 0.5 s first timestamp; a positive delay is what puts the
// track's first sample later than the container's.
//
// Nothing here is committed: the two files exist only where this tool wrote
// them, and `window_decode_test.go` builds the same pair for itself.
func writeWindowFixture(ffmpegBin, dir string) error {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		return fmt.Errorf("mkvmerge not on PATH: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	audio := filepath.Join(dir, "window-audio.wav")
	video := filepath.Join(dir, "window-video.mkv")
	fixture := filepath.Join(dir, "window-start-time-0.5.mkv")
	reference := filepath.Join(dir, "window-reference.wav")

	// syncjob's same-language band, verbatim: -ac 1 -ar 8000 -af <band>
	// -c:a pcm_f32le is the production analysis decode.
	const filter = "highpass=f=80,lowpass=f=3500"

	steps := []struct {
		bin  string
		args []string
	}{
		{ffmpegBin, []string{
			"-v", "error", "-y",
			"-f", "lavfi", "-i", "anoisesrc=d=30:c=pink:r=8000:a=0.5",
			"-ac", "1", "-c:a", "pcm_s16le", audio,
		}},
		{ffmpegBin, []string{
			"-v", "error", "-y",
			"-f", "lavfi", "-i", "color=c=black:s=160x120:r=8:d=30",
			"-c:v", "mpeg4", video,
		}},
		{"mkvmerge", []string{"-o", fixture, "--quiet", video, "--sync", "0:500", audio}},
		{ffmpegBin, []string{
			"-v", "error", "-y",
			"-i", fixture,
			"-map", "0:1",
			"-ac", "1", "-ar", "8000",
			"-af", filter,
			"-c:a", "pcm_f32le", reference,
		}},
	}
	for _, step := range steps {
		if err := runFFmpeg(step.bin, step.args); err != nil {
			return err
		}
	}
	fmt.Printf("window fixture: %s (audio start_time 0.5 s, stream 1)\nreference:      %s\n", fixture, reference)
	return nil
}

// cutExcerpt trims a decoded 8 kHz mono WAV to one checkpoint window. Output
// seeking (-ss after -i) decodes and discards, so the cut starts on the sample
// ffmpeg resolves from the timestamp; target and donor are cut with the same
// timestamp, so the offset between them is preserved exactly.
func cutExcerpt(bin, src, dst string, start, length float64) error {
	args := []string{
		"-v", "error", "-nostdin", "-y",
		"-i", src,
		"-ss", strconv.FormatFloat(round6(start), 'f', 6, 64),
		"-t", strconv.FormatFloat(round6(length), 'f', 6, 64),
		"-c:a", "pcm_s16le",
		dst,
	}
	if err := runFFmpeg(bin, args); err != nil {
		return err
	}
	// A window that runs past EOF truncates (job.rs), but a checkpoint that
	// produced a slice materially shorter than requested would silently change
	// what the golden means; fail loudly instead.
	frames, rate, err := wavFrames(dst)
	if err != nil {
		return err
	}
	want := int64(round6(length) * float64(rate))
	if frames < 64 || frames*100 < want*99 {
		return fmt.Errorf("excerpt %s: %d frames at %d Hz, want %d", filepath.Base(dst), frames, rate, want)
	}
	return nil
}

// ---------------------------------------------------------------------------
// WAV helpers

type wavInfo struct {
	format     uint16
	subFormat  uint16
	sampleRate uint32
	channels   uint16
	bits       uint16
	blockAlign uint16
	dataOffset int64
	dataLen    int64
	chunksEnd  int64 // offset of the "data" chunk header
}

// isFloat resolves WAVE_FORMAT_EXTENSIBLE (what hound writes for float files)
// through its sub-format tag.
func (w wavInfo) isFloat() bool {
	tag := w.format
	if tag == 0xFFFE {
		tag = w.subFormat
	}
	return tag == 3 && w.bits == 32
}

func parseWAV(f *os.File) (wavInfo, error) {
	var info wavInfo
	var hdr [12]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return info, fmt.Errorf("read header: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return info, errors.New("not a RIFF/WAVE file")
	}
	stat, err := f.Stat()
	if err != nil {
		return info, err
	}
	size := stat.Size()
	off := int64(12)
	for off+8 <= size {
		var ch [8]byte
		if _, err := f.ReadAt(ch[:], off); err != nil {
			return info, fmt.Errorf("read chunk at %d: %w", off, err)
		}
		id := string(ch[0:4])
		clen := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch id {
		case "fmt ":
			n := clen
			if n > 40 {
				n = 40
			}
			if n < 16 {
				return info, fmt.Errorf("short fmt chunk (%d bytes)", clen)
			}
			buf := make([]byte, n)
			if _, err := f.ReadAt(buf, off+8); err != nil {
				return info, fmt.Errorf("read fmt: %w", err)
			}
			info.format = binary.LittleEndian.Uint16(buf[0:2])
			info.channels = binary.LittleEndian.Uint16(buf[2:4])
			info.sampleRate = binary.LittleEndian.Uint32(buf[4:8])
			info.blockAlign = binary.LittleEndian.Uint16(buf[12:14])
			info.bits = binary.LittleEndian.Uint16(buf[14:16])
			if info.format == 0xFFFE && len(buf) >= 26 {
				info.subFormat = binary.LittleEndian.Uint16(buf[24:26])
			}
		case "data":
			info.dataOffset = off + 8
			info.dataLen = clen
			info.chunksEnd = off
			if info.blockAlign == 0 {
				return info, errors.New("data chunk before fmt chunk")
			}
			if info.dataLen > size-info.dataOffset {
				info.dataLen = size - info.dataOffset
			}
			return info, nil
		}
		off += 8 + clen + clen%2
	}
	return info, errors.New("no data chunk")
}

func wavFrames(path string) (frames int64, rate uint32, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	info, err := parseWAV(f)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return info.dataLen / int64(info.blockAlign), info.sampleRate, nil
}

// commitWAV writes the first maxSec seconds of src to dst as a 16-bit PCM WAV.
//
// Truncation is behaviour-neutral here: the engine reads only frames
// [40000,120000) of these files (see syntheticTrimSec), so the samples it
// analyses are byte-identical to the full-length generator output. Integer
// input is copied verbatim; float input (the harness writes the stereo case as
// 32-bit float) is quantised with the harness's own rule from
// tests/common/mod.rs `write_wav_mono_i16`: round(clamp(s,-1,1) * 32767). A
// 32-bit float copy of the analysed region does not fit the per-file cap.
func commitWAV(src, dst string, maxSec float64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := parseWAV(in)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(src), err)
	}
	frames := info.dataLen / int64(info.blockAlign)
	if want := int64(maxSec * float64(info.sampleRate)); frames > want {
		frames = want
	}
	if !info.isFloat() {
		return trimVerbatim(in, info, dst, frames)
	}
	return quantizeToPCM16(in, info, dst, frames)
}

// trimVerbatim copies frames of an integer-encoded WAV, keeping the pre-data
// chunks (and therefore the fmt description) byte-identical.
func trimVerbatim(in *os.File, info wavInfo, dst string, frames int64) error {
	newDataLen := frames * int64(info.blockAlign)
	chunks := make([]byte, info.chunksEnd-12)
	if _, err := in.ReadAt(chunks, 12); err != nil {
		return err
	}
	pad := newDataLen % 2
	riffLen := int64(4) + int64(len(chunks)) + 8 + newDataLen + pad

	var buf bytes.Buffer
	buf.Grow(int(riffLen) + 8)
	buf.WriteString("RIFF")
	writeLE32(&buf, uint32(riffLen))
	buf.WriteString("WAVE")
	buf.Write(chunks)
	buf.WriteString("data")
	writeLE32(&buf, uint32(newDataLen))
	if _, err := io.CopyN(&buf, io.NewSectionReader(in, info.dataOffset, newDataLen), newDataLen); err != nil {
		return err
	}
	if pad != 0 {
		buf.WriteByte(0)
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

// quantizeToPCM16 decodes 32-bit float frames and writes them as 16-bit PCM,
// with the harness's rounding rule so the committed bytes are what the Rust
// writer would have produced for the same samples.
func quantizeToPCM16(in *os.File, info wavInfo, dst string, frames int64) error {
	const (
		bits    = 16
		chBytes = 2
	)
	channels := int64(info.channels)
	dataLen := frames * channels * chBytes
	srcBytes := frames * int64(info.blockAlign)

	raw := make([]byte, srcBytes)
	if _, err := in.ReadAt(raw, info.dataOffset); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	out := make([]byte, dataLen)
	for i := int64(0); i < frames*channels; i++ {
		sample := math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4 : i*4+4]))
		if sample > 1 {
			sample = 1
		} else if sample < -1 {
			sample = -1
		}
		q := int16(math.Round(float64(sample * 32767)))
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(q))
	}

	var buf bytes.Buffer
	buf.Grow(int(44 + dataLen))
	buf.WriteString("RIFF")
	writeLE32(&buf, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	writeLE32(&buf, 16)
	var fmtChunk [16]byte
	binary.LittleEndian.PutUint16(fmtChunk[0:2], 1) // WAVE_FORMAT_PCM
	binary.LittleEndian.PutUint16(fmtChunk[2:4], uint16(channels))
	binary.LittleEndian.PutUint32(fmtChunk[4:8], info.sampleRate)
	binary.LittleEndian.PutUint32(fmtChunk[8:12], uint32(int64(info.sampleRate)*channels*chBytes))
	binary.LittleEndian.PutUint16(fmtChunk[12:14], uint16(channels*chBytes))
	binary.LittleEndian.PutUint16(fmtChunk[14:16], bits)
	buf.Write(fmtChunk[:])
	buf.WriteString("data")
	writeLE32(&buf, uint32(dataLen))
	buf.Write(out)
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

func writeLE32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

// ---------------------------------------------------------------------------
// provenance

func collectProvenance(opts options) (provenance, error) {
	var prov provenance
	commit, err := gitOutput(opts.root, "rev-parse", "HEAD")
	if err != nil {
		return prov, err
	}
	dirty, err := gitOutput(opts.root, "status", "--porcelain", "--", "sync-engine")
	if err != nil {
		return prov, err
	}
	prov.RustCommit = commit
	if strings.TrimSpace(dirty) != "" {
		prov.RustCommit += "+dirty"
	}

	lockPath := filepath.Join(opts.root, "sync-engine", "Cargo.lock")
	lockSHA, err := fileSHA256(lockPath)
	if err != nil {
		return prov, err
	}
	prov.CargoLockSHA256 = lockSHA

	versionOut, err := exec.Command(opts.syncBin, "--version").Output()
	if err != nil {
		return prov, fmt.Errorf("%s --version: %w", opts.syncBin, err)
	}
	prov.DubarrSyncVersion = firstLine(string(versionOut))

	ffOut, err := exec.Command(opts.ffmpeg, "-version").Output()
	if err != nil {
		return prov, fmt.Errorf("%s -version: %w", opts.ffmpeg, err)
	}
	prov.FFmpegVersion = firstLine(string(ffOut))

	host, _ := os.Hostname()
	if uname, err := exec.Command("uname", "-srm").Output(); err == nil {
		host += " " + firstLine(string(uname))
	}
	prov.Host = host
	prov.Date = time.Now().UTC().Format(time.RFC3339)

	prov.Argv = argvSpec{
		Emit: []string{
			"cargo", "test", "--offline", "--release", "--test", "emit", "--",
			"--out", "<scratch>/synthetic",
		},
		Excerpt: []string{
			opts.ffmpeg, "-v", "error", "-nostdin", "-y",
			"-i", "<full-decode>/<source>.wav",
			"-ss", "<checkpoint_start_sec>",
			"-t", "<excerpt_sec>",
			"-c:a", "pcm_s16le",
			"<testdata>/wav/<fixture>-cp<pct>-<side>.wav",
		},
		Sync: []string{relTo(opts.root, opts.syncBin)},
	}
	for _, fx := range fixtures {
		prov.Argv.Decode = append(prov.Argv.Decode,
			decodeArgv{
				Source: "testfiles/" + fx.target.file,
				Stream: fx.target.stream,
				Argv:   decodeArgs("testfiles/"+fx.target.file, fx.target.stream, fx.filter, "<scratch>/full/"+fx.name+"-target.wav"),
			},
			decodeArgv{
				Source: "testfiles/" + fx.donor.file,
				Stream: fx.donor.stream,
				Argv:   decodeArgs("testfiles/"+fx.donor.file, fx.donor.stream, fx.filter, "<scratch>/full/"+fx.name+"-donor.wav"),
			},
		)
	}
	return prov, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------------------
// writing and checking

func writeGoldens(dir string, goldens []golden) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, g := range goldens {
		name := strings.TrimPrefix(g.Case, "synthetic/")
		if g.Tier == "synthetic" {
			name = "synthetic-" + name
		}
		raw, err := marshalJSON(g, "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func goldenFileName(g golden) string {
	name := strings.TrimPrefix(g.Case, "synthetic/")
	if g.Tier == "synthetic" {
		name = "synthetic-" + name
	}
	return name + ".json"
}

func checkCommitted(opts options, testdata, goldenDir string, fresh []golden) error {
	var problems []string

	for _, g := range fresh {
		path := filepath.Join(goldenDir, goldenFileName(g))
		raw, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", goldenFileName(g), err))
			continue
		}
		var committed golden
		if err := json.Unmarshal(raw, &committed); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", goldenFileName(g), err))
			continue
		}
		if committed.Provenance.Date != g.Provenance.Date || committed.Provenance.Host != g.Provenance.Host {
			// Date and host are the only volatile fields; keep them out of the
			// comparison but report the change.
			fmt.Fprintf(os.Stderr, "capture: note: %s provenance date/host differ (recorded %q %q, now %q %q)\n",
				filepath.Base(path), committed.Provenance.Date, committed.Provenance.Host, g.Provenance.Date, g.Provenance.Host)
			g.Provenance.Date = committed.Provenance.Date
			g.Provenance.Host = committed.Provenance.Host
		}
		if !reflect.DeepEqual(committed, g) {
			problems = append(problems, describeGoldenDrift(committed, g))
		}
		for _, e := range g.Entries {
			wavPath := absPath(opts.root, e.TargetWAV)
			if err := checkWAV(opts, wavPath, e.TargetSHA256); err != nil {
				problems = append(problems, err.Error())
			}
			wavPath = absPath(opts.root, e.DonorWAV)
			if err := checkWAV(opts, wavPath, e.DonorSHA256); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}

	mdPath := filepath.Join(testdata, "GOLDEN.md")
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		problems = append(problems, fmt.Sprintf("GOLDEN.md: %v", err))
	} else {
		var committed []golden
		for _, g := range fresh {
			b, err := os.ReadFile(filepath.Join(goldenDir, goldenFileName(g)))
			if err != nil {
				continue
			}
			var cg golden
			if err := json.Unmarshal(b, &cg); err != nil {
				continue
			}
			committed = append(committed, cg)
		}
		if string(raw) != renderGoldenMD(committed) {
			problems = append(problems, "GOLDEN.md does not match the committed goldens (regenerate with a normal capture run)")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("drift:\n  %s", strings.Join(problems, "\n  "))
	}
	fmt.Fprintln(os.Stderr, "capture: no drift")
	return nil
}

func checkWAV(opts options, path, want string) error {
	got, err := fileSHA256(path)
	if err != nil {
		return fmt.Errorf("%s: %v", relTo(opts.root, path), err)
	}
	if got != want {
		return fmt.Errorf("%s: bytes drifted (got %s, golden %s)", relTo(opts.root, path), got[:12], want[:12])
	}
	return nil
}

func describeGoldenDrift(committed, fresh golden) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: differs", goldenFileName(fresh))
	if !reflect.DeepEqual(committed.Provenance, fresh.Provenance) {
		fmt.Fprintf(&b, "\n      provenance: recorded %+v, now %+v", committed.Provenance, fresh.Provenance)
	}
	for i := range fresh.Entries {
		if i >= len(committed.Entries) {
			fmt.Fprintf(&b, "\n      %s: missing from the committed golden", fresh.Entries[i].ID)
			continue
		}
		a, e := committed.Entries[i], fresh.Entries[i]
		if a.Result != e.Result {
			fmt.Fprintf(&b, "\n      %s: offset/quality %v -> %v", e.ID, a.Result, e.Result)
		}
		if a.TargetSHA256 != e.TargetSHA256 || a.DonorSHA256 != e.DonorSHA256 {
			fmt.Fprintf(&b, "\n      %s: excerpt bytes changed", e.ID)
		}
		if a.Payload != e.Payload {
			fmt.Fprintf(&b, "\n      %s: payload changed\n        was %s\n        now %s", e.ID, a.Payload, e.Payload)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// GOLDEN.md

func renderGoldenMD(goldens []golden) string {
	var b strings.Builder
	b.WriteString("# Rust golden capture (U-DSP.1)\n\n")
	b.WriteString("Generated by `backend/internal/dsp/testdata/capture/main.go` — do not edit by hand.\n")
	b.WriteString("Re-capture with `cd backend && go run ./internal/dsp/testdata/capture/main.go`;\n")
	b.WriteString("the same command with `-check` re-runs the capture and exits non-zero on any drift.\n\n")
	b.WriteString("Prerequisites for both modes: a built reference engine\n")
	b.WriteString("(`cd sync-engine && cargo build --release`), `cargo` with the harness's crates in\n")
	b.WriteString("the local registry cache (the emitter builds offline), `ffmpeg`, and `testfiles/`\n")
	b.WriteString("(Tier B needs the four real fixtures). Tier A needs no media. The synthetic tier is\n")
	b.WriteString("produced by the Rust harness's own generator: the tool copies `sync-engine/` into a\n")
	b.WriteString("scratch directory, drops the sibling `capture/emit.rs` in as `tests/emit.rs` and runs\n")
	b.WriteString("`cargo test --test emit`, so `tests/common/mod.rs` is compiled, not re-implemented.\n\n")
	b.WriteString("Every number below is the reference engine's own output (`SyncResult` rounded to\n")
	b.WriteString("three decimals by `job.rs:121-123`), copied verbatim from the golden JSON.\n")
	b.WriteString("**margin to 0.10** is `correlation_quality - 0.10`, the distance to\n")
	b.WriteString("`analyze.LowQualityThreshold`; a checkpoint marked **thin** sits below 0.15\n")
	b.WriteString("(plan conflict (e): the margin is for the maintainer's sign-off, not the coder's).\n\n")

	if len(goldens) > 0 {
		p := goldens[0].Provenance
		b.WriteString("## Provenance\n\n")
		fmt.Fprintf(&b, "| field | value |\n|---|---|\n")
		fmt.Fprintf(&b, "| rust_commit | `%s` |\n", p.RustCommit)
		fmt.Fprintf(&b, "| cargo_lock_sha256 | `%s` |\n", p.CargoLockSHA256)
		fmt.Fprintf(&b, "| dubarr_sync_version | `%s` |\n", p.DubarrSyncVersion)
		fmt.Fprintf(&b, "| ffmpeg_version | `%s` |\n", p.FFmpegVersion)
		fmt.Fprintf(&b, "| host | `%s` |\n", p.Host)
		fmt.Fprintf(&b, "| date | `%s` |\n", p.Date)
		b.WriteString("\nDecode argv (production `syncjob.decodeAudio`, verbatim):\n\n")
		for _, d := range p.Argv.Decode {
			fmt.Fprintf(&b, "- `%s` (stream %d): `%s`\n", d.Source, d.Stream, strings.Join(d.Argv, " "))
		}
		fmt.Fprintf(&b, "\nExcerpt argv: `%s`\n", strings.Join(p.Argv.Excerpt, " "))
		fmt.Fprintf(&b, "\nEngine: `%s`\n", strings.Join(p.Argv.Sync, " "))
	}

	b.WriteString("\n## Tier A — synthetic (generator output, no media)\n\n")
	b.WriteString("| case | offset_ms | quality | margin to 0.10 |\n|---|---|---|---|\n")
	for _, g := range goldens {
		if g.Tier != "synthetic" {
			continue
		}
		for _, e := range g.Entries {
			fmt.Fprintf(&b, "| `%s`%s | %s | %s | %s |\n",
				strings.TrimPrefix(g.Case, "synthetic/"), expectedSuffix(g.Expected),
				fmtOffset(e.Result.OffsetMS), qualityCell(e.Result.CorrelationQuality, e.Thin), fmtMargin(e.MarginTo010))
		}
	}

	b.WriteString("\n## Tier B — real material (committed excerpts)\n")
	b.WriteString("\nEach row is one checkpoint of `analyze.DefaultCheckpointPositionsPct`\n")
	b.WriteString("(10/25/40/55/70/85 %), cut from the production decode of the fixture pair at\n")
	b.WriteString("`analyze.sliceWindow`'s position. The excerpt **is** the analysed window, so the\n")
	b.WriteString("recorded payload asks for `slice_start_sec = 0`; the source position it was cut\n")
	b.WriteString("from is the `source_start_sec` field of each golden row.\n")
	b.WriteString("\nMinions is the cross-language pair: there is no shared reference language, so the\n")
	b.WriteString("band is the 30-200 Hz low-pass and the peaks are weak. Some of its offsets are\n")
	b.WriteString("spurious peaks rather than a real alignment; they are recorded exactly as the\n")
	b.WriteString("reference engine measured them, and the 40 % row is **thin**.\n")
	for _, g := range goldens {
		if g.Tier != "real" {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n", g.Case)
		b.WriteString("| case | offset_ms | quality | margin to 0.10 |\n|---|---|---|---|\n")
		for _, e := range g.Entries {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n",
				e.ID, fmtOffset(e.Result.OffsetMS), qualityCell(e.Result.CorrelationQuality, e.Thin), fmtMargin(e.MarginTo010))
		}
	}

	b.WriteString("\n## Excerpt length and the committed-size budget\n\n")
	b.WriteString("The plan asks for 30 s excerpts and `<= 8 MB` of committed testdata with each WAV\n")
	b.WriteString("`<= 640 KB`. Those do not fit together. A 30 s 8 kHz mono file is 480,044 B at\n")
	b.WriteString("16-bit and 960,044 B at 32-bit float; the 24 Tier B excerpts alone would weigh\n")
	b.WriteString("11.5 MB (16-bit) or 23 MB (float) before a single synthetic file is committed.\n")
	b.WriteString("The committed set therefore uses:\n\n")
	fmt.Fprintf(&b, "- Tier B: %.0f s excerpts, 8 kHz mono 16-bit — %d files, 160,044 B each,\n", defaultExcerptSec, 2*len(checkpointPcts)*len(fixtures))
	b.WriteString("  ~3.67 MiB total.\n")
	b.WriteString("- Tier A: the generator's own samples, truncated to the region the harness\n")
	b.WriteString("  analyses (`[0, 15 s)` = 240,044 B per 16-bit mono file); the stereo case,\n")
	b.WriteString("  which the harness writes as 32-bit float stereo, is committed as 16-bit\n")
	b.WriteString("  stereo (480,044 B for the same 15 s) because a float copy of the analysed\n")
	b.WriteString("  region is 960,068 B and breaks the 640 KB per-file cap. The quantisation is\n")
	b.WriteString("  the harness's own rounding rule (`round(clamp(s,-1,1) * 32767)`).\n")
	b.WriteString("\nEvery committed file is therefore 16-bit PCM (`U-DSP.3`'s `/32768` divisor); the\n")
	b.WriteString("32-bit float decode path is covered by that unit's own table tests, not here.\n")
	b.WriteString("\nThe truncation cannot move a number: the payload still asks for\n")
	b.WriteString("`slice_start_sec = 5` for Tier A, and the engine reads only frames\n")
	b.WriteString("`[40000, 120000)`, which are byte-identical to the full-length file.\n")
	b.WriteString("Tier B is re-encoded to 16-bit by ffmpeg because 32-bit float excerpts do not\n")
	b.WriteString("fit the per-file cap at any useful length; the production decode that feeds\n")
	b.WriteString("them is still `pcm_f32le` and the band filter is verbatim, and the golden\n")
	b.WriteString("describes the committed 16-bit bytes (that is also what exercises U-DSP.3's\n")
	b.WriteString("`/32768` divisor). The consequences are recorded rather than hidden: excerpt\n")
	b.WriteString("numbers are not the production 190 s checkpoint numbers (they never were), and\n")
	b.WriteString("the quality values above are the ones the Go port must reproduce on these files.\n")
	b.WriteString("The capture tool prints the measured committed total on every run.\n")

	b.WriteString("\n## Reading the table\n\n")
	b.WriteString("- `offset_ms = t_target - t_donor` (`main.rs`), so a positive value means the\n")
	b.WriteString("  target's audio runs late relative to the donor.\n")
	b.WriteString("- Tier A cases carry the Rust suite's own bounds in the case label\n")
	b.WriteString("  (`expect … ±… ms, q >= …`, from `tests/golden.rs`).\n")
	b.WriteString("- Tier B has no expected offset: the excerpt value *is* the expectation.\n")
	b.WriteString("- **thin**: `quality < 0.15`. U-DSP.9 asserts `quality_go >= 0.12` and\n")
	b.WriteString("  `|Δquality| <= 0.02` per checkpoint; a thin or sub-0.12 Rust value makes that\n")
	b.WriteString("  assertion unmeetable and is a sign-off matter, not something to round away.\n")
	return b.String()
}

func expectedSuffix(e *expected) string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf(" (expect %s ±%.1f ms, q >= %.2f)", fmtOffset(e.OffsetMS), e.ToleranceMS, e.MinQuality)
}

func qualityCell(q float64, thin bool) string {
	cell := strconv.FormatFloat(q, 'f', 3, 64)
	if thin {
		cell += " **thin**"
	}
	return cell
}

func fmtOffset(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func fmtMargin(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	if !strings.HasPrefix(s, "-") {
		s = "+" + s
	}
	return s
}

func printTable(goldens []golden) {
	fmt.Fprintf(os.Stderr, "\n%-24s %12s %8s %10s\n", "case", "offset_ms", "quality", "margin")
	for _, g := range goldens {
		for _, e := range g.Entries {
			thin := ""
			if e.Thin {
				thin = " thin"
			}
			fmt.Fprintf(os.Stderr, "%-24s %12.3f %8.3f %10.3f%s\n",
				e.ID, e.Result.OffsetMS, e.Result.CorrelationQuality, e.MarginTo010, thin)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers

func repoRoot(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate this source file; pass -repo")
	}
	dir := filepath.Dir(file)
	root := filepath.Clean(filepath.Join(dir, "..", "..", "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "backend", "go.mod")); err != nil {
		return "", fmt.Errorf("derived repository root %s does not hold backend/go.mod; pass -repo", root)
	}
	return root, nil
}

func absPath(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

// committedWAVDir is where the excerpts of both tiers are committed.
func committedWAVDir(root string) string {
	return filepath.Join(root, "backend", "internal", "dsp", "testdata", "wav")
}

func relTo(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

func substituteScratch(args []string, scratch string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "<scratch>", scratch)
	}
	return out
}

// sliceWindow is analyze.sliceWindow: the checkpoint window centred on pct of
// duration, clamped into the file.
func sliceWindow(duration float64, pct int, window float64) (start, length float64) {
	start = duration*float64(pct)/100 - window/2
	if start < 0 {
		start = 0
	}
	if start+window > duration {
		start = duration - window
	}
	if start < 0 {
		start = 0
	}
	length = min64f(window, duration-start)
	return start, length
}

func round3(v float64) float64 { return float64(int64(v*1000+copysign(0.5, v))) / 1000 }
func round6(v float64) float64 { return float64(int64(v*1e6+copysign(0.5, v))) / 1e6 }

func copysign(mag, sign float64) float64 {
	if sign < 0 {
		return -mag
	}
	return mag
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func min64f(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "target" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

// testdataSize is the committed footprint the plan caps at 8 MB.
func testdataSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}
