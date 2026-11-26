package wav

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wavFmt is the fmt-chunk description a fixture is built from.
type wavFmt struct {
	tag      uint16 // 1 PCM, 3 IEEE float, 0xFFFE extensible
	channels int
	rate     uint32
	bits     int
	sub      uint16 // extensible sub-format tag (1 PCM, 3 float)
}

// payload renders the fmt chunk body. Extensible gets the canonical 40-byte
// layout: cbSize, valid bits, channel mask and the KSDATAFORMAT_SUBTYPE GUID.
func (f wavFmt) payload() []byte {
	blockAlign := uint16(f.channels * f.bits / 8)
	p := make([]byte, 16)
	binary.LittleEndian.PutUint16(p[0:2], f.tag)
	binary.LittleEndian.PutUint16(p[2:4], uint16(f.channels))
	binary.LittleEndian.PutUint32(p[4:8], f.rate)
	binary.LittleEndian.PutUint32(p[8:12], f.rate*uint32(blockAlign))
	binary.LittleEndian.PutUint16(p[12:14], blockAlign)
	binary.LittleEndian.PutUint16(p[14:16], uint16(f.bits))
	if f.tag != formatExtensible {
		return p
	}
	// cbSize, valid bits, channel mask, and then the 16-byte sub-format GUID.
	p = binary.LittleEndian.AppendUint16(p, 22)
	p = binary.LittleEndian.AppendUint16(p, uint16(f.bits))
	p = binary.LittleEndian.AppendUint32(p, 0)
	sub := make([]byte, 16)
	binary.LittleEndian.PutUint16(sub[0:2], f.sub)
	// 0000-0010-8000-00AA00389B71, the constant tail of every KSDATAFORMAT
	// subtype GUID.
	copy(sub[2:], []byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
	return append(p, sub...)
}

// riffChunk builds id+size+payload and applies RIFF's pad byte for odd payloads.
func riffChunk(id string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	copy(out[0:4], id)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[8:], payload)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// wavBytes assembles a RIFF/WAVE container from a fmt spec and a data payload.
func wavBytes(f wavFmt, data []byte) []byte {
	body := append([]byte("WAVE"), riffChunk("fmt ", f.payload())...)
	body = append(body, riffChunk("data", data)...)
	out := make([]byte, 8, 8+len(body))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(body)))
	return append(out, body...)
}

// encodeInt packs interleaved integer samples little-endian in the container's
// own width; the low bits/8 bytes of the int32 are the two's complement sample.
func encodeInt(samples []int32, bits int) []byte {
	width := bits / 8
	out := make([]byte, len(samples)*width)
	for i, s := range samples {
		u := uint32(s)
		for b := 0; b < width; b++ {
			out[i*width+b] = byte(u >> (8 * b))
		}
	}
	return out
}

// encodeFloat packs interleaved float32 samples bit for bit.
func encodeFloat(samples []float32) []byte {
	out := make([]byte, len(samples)*4)
	for i, s := range samples {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(s))
	}
	return out
}

func writeTemp(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.wav")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func mustOpen(t *testing.T, path string) *RangeReader {
	t.Helper()
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// TestReadMonoEncodings pins the divisor of every accepted encoding, mono and
// stereo: the value at a chosen frame must equal the expected constant exactly,
// with no tolerance, which is where a wrong divisor or a wrong int-to-f32
// conversion shows up. 2147483647 is in the table on purpose: hound converts
// the i32 to f32 first, so the value rounds to 2147483648 and normalises to 1.
func TestReadMonoEncodings(t *testing.T) {
	const rate = 8000

	cases := []struct {
		name string
		fmt  wavFmt
		data []byte
		want []float32
	}{
		{
			name: "16-bit mono",
			fmt:  wavFmt{tag: formatPCM, channels: 1, rate: rate, bits: 16},
			data: encodeInt([]int32{0, 16384, -16384, 32767, -32768}, 16),
			want: []float32{0, 0.5, -0.5, 32767.0 / 32768.0, -1},
		},
		{
			name: "16-bit stereo",
			fmt:  wavFmt{tag: formatPCM, channels: 2, rate: rate, bits: 16},
			data: encodeInt([]int32{0, 0, 8192, 24576, -8192, -24576, 32767, 32767, -32768, -32768}, 16),
			want: []float32{0, 0.5, -0.5, 32767.0 / 32768.0, -1},
		},
		{
			name: "24-bit mono",
			fmt:  wavFmt{tag: formatPCM, channels: 1, rate: rate, bits: 24},
			data: encodeInt([]int32{0, 4194304, -4194304, 8388607, -8388608}, 24),
			want: []float32{0, 0.5, -0.5, 8388607.0 / 8388608.0, -1},
		},
		{
			name: "24-bit stereo",
			fmt:  wavFmt{tag: formatPCM, channels: 2, rate: rate, bits: 24},
			data: encodeInt([]int32{0, 0, 2097152, 6291456, -2097152, -6291456, 8388607, 8388607, -8388608, -8388608}, 24),
			want: []float32{0, 0.5, -0.5, 8388607.0 / 8388608.0, -1},
		},
		{
			name: "32-bit int mono",
			fmt:  wavFmt{tag: formatPCM, channels: 1, rate: rate, bits: 32},
			data: encodeInt([]int32{0, 1073741824, -1073741824, 2147483647, -2147483648}, 32),
			want: []float32{0, 0.5, -0.5, 1, -1},
		},
		{
			name: "32-bit int stereo",
			fmt:  wavFmt{tag: formatPCM, channels: 2, rate: rate, bits: 32},
			data: encodeInt([]int32{0, 0, 536870912, 1610612736, -536870912, -1610612736, 2147483647, 2147483647, -2147483648, -2147483648}, 32),
			want: []float32{0, 0.5, -0.5, 1, -1},
		},
		{
			name: "32-bit float mono",
			fmt:  wavFmt{tag: formatFloat, channels: 1, rate: rate, bits: 32},
			data: encodeFloat([]float32{0, 0.5, -0.5, 0.25, -0.25}),
			want: []float32{0, 0.5, -0.5, 0.25, -0.25},
		},
		{
			name: "32-bit float stereo",
			fmt:  wavFmt{tag: formatFloat, channels: 2, rate: rate, bits: 32},
			data: encodeFloat([]float32{0, 0, 0.25, 0.75, -0.25, -0.75, 0.1, 0.2, 1, -1}),
			// 0.1f + 0.2f halved: the f32 sum is 0.30000001192092896.
			want: []float32{0, 0.5, -0.5, 0.15000000596046448, 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustOpen(t, writeTemp(t, wavBytes(tc.fmt, tc.data)))
			if got, want := r.SampleRate(), tc.fmt.rate; got != want {
				t.Fatalf("SampleRate = %d, want %d", got, want)
			}
			if got, want := r.Frames(), int64(len(tc.want)); got != want {
				t.Fatalf("Frames = %d, want %d", got, want)
			}
			got, err := r.ReadMono(0, r.Frames())
			if err != nil {
				t.Fatalf("ReadMono: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ReadMono returned %d frames, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("frame %d = %v (bits %#08x), want %v exactly (bits %#08x)",
						i, got[i], math.Float32bits(got[i]), tc.want[i], math.Float32bits(tc.want[i]))
				}
			}
			// The plan's acceptance point, spelled out for one frame that a
			// zero-initialised or mis-sought buffer cannot fake.
			const chosen = 1
			if got[chosen] != tc.want[chosen] {
				t.Fatalf("frame %d = %v, want %v exactly", chosen, got[chosen], tc.want[chosen])
			}
		})
	}
}

// TestStereoMixdownBits pins the mixdown expression to wav.rs:132 —
// `(frame[0] + frame[1]) * 0.5` in float32 — by comparing the resulting bits.
// The pairs are chosen so that the f32 sum rounds: a mixdown that halved each
// channel first, or that folded frames through an f64 accumulator, lands on a
// neighbouring value.
func TestStereoMixdownBits(t *testing.T) {
	cases := []struct {
		left, right float32
		want        uint32
	}{
		{0.1, 0.2, 0x3E19999A},
		{1.0 / 3, 0.7, 0x3F044444},
		{-0.5, 0.25, 0xBE000000},
		{1.0, -1.0, 0x00000000},
		{0.001, 1.0, 0x3F0020C5},
	}

	samples := make([]float32, 0, 2*len(cases))
	for _, tc := range cases {
		samples = append(samples, tc.left, tc.right)
	}
	spec := wavFmt{tag: formatFloat, channels: 2, rate: 8000, bits: 32}
	r := mustOpen(t, writeTemp(t, wavBytes(spec, encodeFloat(samples))))

	got, err := r.ReadMono(0, int64(len(cases)))
	if err != nil {
		t.Fatalf("ReadMono: %v", err)
	}
	if len(got) != len(cases) {
		t.Fatalf("ReadMono returned %d frames, want %d", len(got), len(cases))
	}
	for i, tc := range cases {
		if bits := math.Float32bits(got[i]); bits != tc.want {
			t.Fatalf("mean(%v, %v) = %v (bits %#08x), want bits %#08x",
				tc.left, tc.right, got[i], bits, tc.want)
		}
	}
}

// TestReadMonoRangeMatchesWholeFile proves the range path is sample-exact: a
// mid-file window must be byte-identical to the same window of a whole-file
// read, including windows that straddle the internal chunk-buffer seams, and a
// window running past the end must truncate to the frames that exist.
func TestReadMonoRangeMatchesWholeFile(t *testing.T) {
	const (
		rate   = 16000
		frames = 20000
	)
	samples := make([]int32, frames*2)
	for i := 0; i < frames; i++ {
		// Asymmetric channels with different periods, so the mixdown is
		// exercised frame by frame and a mis-sought buffer cannot pass.
		samples[2*i] = int32(i%2000) - 1000
		samples[2*i+1] = int32(i%1500) - 750
	}
	spec := wavFmt{tag: formatPCM, channels: 2, rate: rate, bits: 16}
	r := mustOpen(t, writeTemp(t, wavBytes(spec, encodeInt(samples, 16))))
	if got := r.Frames(); got != frames {
		t.Fatalf("Frames = %d, want %d", got, frames)
	}

	whole, err := r.ReadMono(0, r.Frames())
	if err != nil {
		t.Fatalf("whole-file ReadMono: %v", err)
	}
	if len(whole) != frames {
		t.Fatalf("whole-file read returned %d frames, want %d", len(whole), frames)
	}

	cases := []struct {
		name  string
		start int64
		n     int64
		want  int64 // frames the range must report
	}{
		{"start of file", 0, 1, 1},
		{"unaligned mid-file", 7, 1000, 1000},
		{"crosses chunk seams", 5000, 12345, 12345},
		{"exact tail", frames - 1000, 1000, 1000},
		{"tail truncated", frames - 10, 100, 10},
		{"start at end", frames, 100, 0},
		{"start past end", frames + 10, 100, 0},
		{"zero frames", 123, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ReadMono(tc.start, tc.n)
			if err != nil {
				t.Fatalf("ReadMono(%d, %d): %v", tc.start, tc.n, err)
			}
			if int64(len(got)) != tc.want {
				t.Fatalf("ReadMono(%d, %d) returned %d frames, want %d",
					tc.start, tc.n, len(got), tc.want)
			}
			if tc.want == 0 {
				if got == nil {
					t.Fatal("empty read returned a nil slice")
				}
				return
			}
			// Byte-identical to the whole-file read, sliced in memory.
			want := whole[tc.start : tc.start+tc.want]
			for i := range want {
				if gb, wb := math.Float32bits(got[i]), math.Float32bits(want[i]); gb != wb {
					t.Fatalf("frame %d (file frame %d) = %#08x, whole-file slice = %#08x",
						i, tc.start+int64(i), gb, wb)
				}
			}
		})
	}
}

// TestTruncatedDataChunkClamps covers the issue-#22 tail: a data chunk that
// claims more bytes than the file holds reads as a shorter file (no error), so
// a cut-short donor cannot fail a job.
func TestTruncatedDataChunkClamps(t *testing.T) {
	spec := wavFmt{tag: formatPCM, channels: 1, rate: 8000, bits: 16}
	raw := wavBytes(spec, encodeInt([]int32{0, 16384, 32767, -16384, -32768, 100, 200, 300, 400, 500}, 16))
	// Cut three frames off the end without touching the header, so the data
	// chunk still claims ten frames.
	raw = raw[:len(raw)-3*2]

	r := mustOpen(t, writeTemp(t, raw))
	if got, want := r.Frames(), int64(7); got != want {
		t.Fatalf("Frames = %d, want %d (the frames the file really holds)", got, want)
	}
	got, err := r.ReadMono(0, 10)
	if err != nil {
		t.Fatalf("ReadMono: %v", err)
	}
	if got, want := int64(len(got)), int64(7); got != want {
		t.Fatalf("ReadMono returned %d frames, want the truncated %d", got, want)
	}
	if got[1] != 0.5 {
		t.Fatalf("frame 1 = %v, want 0.5", got[1])
	}
}

// TestExtensible covers WAVE_FORMAT_EXTENSIBLE: the sub-format GUID decides the
// encoding, and a non-PCM/float sub-format is refused with the typed error.
func TestExtensible(t *testing.T) {
	t.Run("pcm accepted", func(t *testing.T) {
		spec := wavFmt{tag: formatExtensible, channels: 1, rate: 8000, bits: 16, sub: formatPCM}
		r := mustOpen(t, writeTemp(t, wavBytes(spec, encodeInt([]int32{0, 16384, -32768}, 16))))
		got, err := r.ReadMono(0, 3)
		if err != nil {
			t.Fatalf("ReadMono: %v", err)
		}
		want := []float32{0, 0.5, -1}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("frame %d = %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("float accepted", func(t *testing.T) {
		spec := wavFmt{tag: formatExtensible, channels: 2, rate: 8000, bits: 32, sub: formatFloat}
		r := mustOpen(t, writeTemp(t, wavBytes(spec, encodeFloat([]float32{0.25, 0.75, -1, 1}))))
		got, err := r.ReadMono(0, 2)
		if err != nil {
			t.Fatalf("ReadMono: %v", err)
		}
		if got[0] != 0.5 || got[1] != 0 {
			t.Fatalf("ReadMono = %v, want [0.5 0]", got)
		}
	})
}

// TestOpenErrors covers the typed-failure contract: structural damage is *Io
// (the mapping wav.rs applies to every hound error, FormatError included) and a
// layout/encoding the engine does not read is *UnsupportedWav.
func TestOpenErrors(t *testing.T) {
	valid := wavBytes(wavFmt{tag: formatPCM, channels: 1, rate: 8000, bits: 16},
		encodeInt([]int32{0, 16384}, 16))

	badMagic := append([]byte(nil), valid...)
	copy(badMagic[0:4], "RIFX")

	notWave := append([]byte(nil), valid...)
	copy(notWave[8:12], "AVI ")

	// "RIFF" + size + "WAVE" and then nothing: the chunk walk hits EOF with no
	// fmt or data chunk.
	headerOnly := append([]byte(nil), valid[:12]...)

	// A fmt header claiming 16 bytes with only 8 of them present.
	fmtTruncated := append([]byte(nil), valid[:12]...)
	fmtTruncated = append(fmtTruncated, riffChunk("fmt ", make([]byte, 8))...)
	fmtTruncated[12+4] = 16 // rewrite the chunk size from 8 to 16

	threeChannels := wavBytes(wavFmt{tag: formatPCM, channels: 3, rate: 8000, bits: 16},
		encodeInt([]int32{0, 0, 0, 16384, 16384, 16384}, 16))
	eightBit := wavBytes(wavFmt{tag: formatPCM, channels: 1, rate: 8000, bits: 8},
		[]byte{0x00, 0x80, 0xFF})
	adpcm := wavBytes(wavFmt{tag: 0x0011, channels: 1, rate: 8000, bits: 4}, []byte{0, 0, 0, 0})
	extAdpcm := wavBytes(wavFmt{tag: formatExtensible, channels: 1, rate: 8000, bits: 16, sub: 0x0002},
		encodeInt([]int32{0, 16384}, 16))

	write := func(name string, raw []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}

	cases := []struct {
		name        string
		path        string
		unsupported bool
		contains    string
	}{
		{
			name:        "bad magic",
			path:        write("bad-magic.wav", badMagic),
			unsupported: false,
			contains:    "RIFF",
		},
		{
			name:        "not wav",
			path:        write("not-wave.wav", notWave),
			unsupported: false,
			contains:    "WAVE",
		},
		{
			name:        "truncated riff",
			path:        write("truncated-riff.wav", headerOnly),
			unsupported: false,
			contains:    "no fmt chunk",
		},
		{
			name:        "truncated fmt chunk",
			path:        write("truncated-fmt.wav", fmtTruncated),
			unsupported: false,
			contains:    "read fmt chunk",
		},
		{
			name:        "three channels",
			path:        write("three-channels.wav", threeChannels),
			unsupported: true,
			contains:    "3 channel(s)",
		},
		{
			name:        "eight bit",
			path:        write("eight-bit.wav", eightBit),
			unsupported: true,
			contains:    "8-bit Int samples",
		},
		{
			name:        "unsupported format tag",
			path:        write("adpcm.wav", adpcm),
			unsupported: true,
			contains:    "format tag",
		},
		{
			name:        "unsupported extensible sub-format",
			path:        write("extensible-adpcm.wav", extAdpcm),
			unsupported: true,
			contains:    "sub-format",
		},
		{
			name:        "missing file",
			path:        filepath.Join(t.TempDir(), "absent.wav"),
			unsupported: false,
			contains:    "cannot read",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Open(tc.path)
			if err == nil {
				r.Close()
				t.Fatal("Open succeeded, want a typed error")
			}
			if tc.unsupported {
				var unsupported *UnsupportedWav
				if !errors.As(err, &unsupported) {
					t.Fatalf("Open error %v (%T), want *UnsupportedWav", err, err)
				}
			} else {
				var ioErr *Io
				if !errors.As(err, &ioErr) {
					t.Fatalf("Open error %v (%T), want *Io", err, err)
				}
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error %q does not mention %q", err, tc.contains)
			}
		})
	}
}

// TestReadMonoRejectsNegativeRange covers the caller-bug guard: a negative
// index cannot be a file condition (the file starts at frame 0), so it is a
// plain error rather than a typed one.
func TestReadMonoRejectsNegativeRange(t *testing.T) {
	spec := wavFmt{tag: formatPCM, channels: 1, rate: 8000, bits: 16}
	r := mustOpen(t, writeTemp(t, wavBytes(spec, encodeInt([]int32{0, 16384}, 16))))
	for _, tc := range []struct{ start, n int64 }{{-1, 1}, {0, -1}} {
		if _, err := r.ReadMono(tc.start, tc.n); err == nil {
			t.Fatalf("ReadMono(%d, %d) accepted a negative range", tc.start, tc.n)
		}
	}
}

// TestLargeReadAllocationsBounded is the issue-#22 guard. The fixture is a
// 190 s production-length slice (8 kHz, stereo, 32-bit float — 8 bytes per
// frame on disk, the widest layout the engine accepts), so a whole-file reader
// would have to hold 12 bytes per frame in memory.
//
// Open must touch the header only, and the read must cost its output plus a
// fixed 32 KiB buffer. The plan's bound is frames*4*3; the tighter frames*4*2
// check is the one that actually separates streaming from a whole-file read,
// which would sit at exactly frames*4*3 for this encoding and could pass the
// looser bound by rounding.
func TestLargeReadAllocationsBounded(t *testing.T) {
	const (
		rate   = 8000
		frames = 190 * rate // 1_520_000
	)
	const spec = "190 s @ 8 kHz stereo float32"

	data := make([]byte, frames*2*4)
	for i := 0; i < frames*2; i++ {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(float32(i%2000-1000)/1000))
	}
	path := writeTemp(t, wavBytes(wavFmt{tag: formatFloat, channels: 2, rate: rate, bits: 32}, data))
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat fixture: %v", err)
	} else if info.Size() < int64(frames)*8 {
		t.Fatalf("fixture is %d bytes, want at least %d", info.Size(), int64(frames)*8)
	}

	t.Run("open reads the header only", func(t *testing.T) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r, err := Open(path)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		runtime.KeepAlive(r)
		defer r.Close()

		used := after.TotalAlloc - before.TotalAlloc
		if used > 64<<10 {
			t.Fatalf("Open allocated %d bytes on a %s file; it must read the header only", used, spec)
		}
	})

	t.Run("read stays streamed", func(t *testing.T) {
		r := mustOpen(t, path)
		if got := r.Frames(); got != frames {
			t.Fatalf("Frames = %d, want %d", got, frames)
		}

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		samples, err := r.ReadMono(0, frames)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("ReadMono: %v", err)
		}
		runtime.KeepAlive(samples)
		if len(samples) != frames {
			t.Fatalf("ReadMono returned %d frames, want %d", len(samples), frames)
		}

		used := after.TotalAlloc - before.TotalAlloc
		output := uint64(frames) * 4
		if used < output {
			t.Fatalf("read allocated %d bytes, less than the %d-byte output; the read did not happen", used, output)
		}
		if bound := uint64(frames) * 4 * 3; used > bound {
			t.Fatalf("read allocated %d bytes for %s, want at most %d (frames*4*3)", used, spec, bound)
		}
		if bound := uint64(frames) * 4 * 2; used > bound {
			t.Fatalf("read allocated %d bytes for %s, want at most %d (frames*4*2): "+
				"a whole-file read of this encoding costs frames*4*3", used, spec, bound)
		}
		t.Logf("%s: %d frames, %d bytes allocated (output is %d)", spec, len(samples), used, output)
	})
}
