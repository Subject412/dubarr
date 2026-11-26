package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// WAVE format tags. The extensible tag defers the real encoding to the first
// two bytes of its sub-format GUID.
const (
	formatPCM        = 0x0001
	formatFloat      = 0x0003
	formatExtensible = 0xFFFE
)

const (
	// maxFmtChunkSize bounds the fmt payload we are willing to read. The
	// canonical PCM fmt chunk is 16 bytes and WAVE_FORMAT_EXTENSIBLE adds 24;
	// anything past this is malformed, and trusting the file-controlled uint32
	// would allow a multi-gigabyte allocation from a header alone.
	maxFmtChunkSize = 64 << 10

	// readChunkBytes is the fixed read window ReadMono streams through: one
	// chunk buffer is allocated per call, no matter how many frames are asked
	// for, so a long range costs its output and 32 KiB, never the file.
	readChunkBytes = 32 << 10
)

// Io reports a WAV that could not be opened or whose RIFF/WAVE structure is
// broken. Msg is the engine's own wording, so log lines keep their shape.
type Io struct {
	Path string
	Msg  string
}

// Error implements error, rendering the engine's `cannot read <path>: <msg>`.
func (e *Io) Error() string { return "cannot read " + e.Path + ": " + e.Msg }

// UnsupportedWav reports a structurally valid WAV whose channel layout or
// sample encoding the engine does not read.
type UnsupportedWav struct {
	Msg string
}

// Error implements error, rendering the engine's `unsupported WAV: <msg>`.
func (e *UnsupportedWav) Error() string { return "unsupported WAV: " + e.Msg }

// RangeReader is a header-validated WAV reader that decodes only a requested
// frame range. It is safe for concurrent use: every read is an [os.File.ReadAt]
// at an explicit offset and carries its own buffer.
type RangeReader struct {
	file *os.File
	path string

	dataStart int64 // byte offset of the first sample byte in the file
	frames    int64 // frames available in the data chunk, floored and clamped
	channels  int
	rate      uint32

	sampleBytes int // bytes per channel sample: 2, 3 or 4
	stride      int // bytes per frame: channels * sampleBytes

	decode sampleDecoder
}

// Open reads and validates the header of the WAV at path.
//
// Only the header is read: the `data` chunk is located, not loaded. The channel
// layout (mono or stereo) and the sample encoding (16/24/32-bit int or 32-bit
// float) are checked here, so an unsupported file fails before any sample is
// decoded and before any sample-sized buffer is allocated.
func Open(path string) (*RangeReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, &Io{Path: path, Msg: err.Error()}
	}
	reader, err := readHeader(file, path)
	if err != nil {
		file.Close()
		return nil, err
	}
	return reader, nil
}

// SampleRate reports the rate from the `fmt` chunk, in Hz.
func (r *RangeReader) SampleRate() uint32 { return r.rate }

// Frames reports how many frames (samples per channel) the reader can decode.
//
// A `data` chunk that claims more bytes than the file holds is clamped to the
// file's real end, so Frames never over-reports: a truncated download reads as
// a shorter file instead of failing mid-job.
func (r *RangeReader) Frames() int64 { return r.frames }

// ReadMono decodes up to frames mono frames starting at frame start.
//
// A range that runs past the end of the file is truncated to what is left and
// returned with a nil error, which mirrors the former whole-file slice: a donor
// whose tail is shorter than the analysed window must not fail the job. A start
// at or past the end returns an empty slice. frames == 0 returns an empty slice
// without touching the file.
//
// A negative start or frames is a caller bug, not a file condition, and returns
// a plain error: seeking before the data chunk would decode header bytes as
// audio.
func (r *RangeReader) ReadMono(start, frames int64) ([]float32, error) {
	if start < 0 || frames < 0 {
		return nil, fmt.Errorf("wav: negative frame range start=%d frames=%d", start, frames)
	}
	if frames == 0 || start >= r.frames {
		return []float32{}, nil
	}
	want := frames
	if available := r.frames - start; want > available {
		want = available
	}

	out := make([]float32, want)
	buffer := make([]byte, r.chunkBytes())
	perChunk := int64(len(buffer) / r.stride)
	offset := r.dataStart + start*int64(r.stride)

	var done int64
	for done < want {
		batch := want - done
		if batch > perChunk {
			batch = perChunk
		}
		n, err := r.file.ReadAt(buffer[:int(batch)*r.stride], offset)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &Io{Path: r.path, Msg: err.Error()}
		}
		got := int64(n) / int64(r.stride)
		if got == 0 {
			// The file shrank since Open; truncate rather than fail.
			break
		}
		r.decodeChunk(out[done:done+got], buffer[:int(got)*r.stride])
		done += got
		offset += got * int64(r.stride)
	}
	// A short read means the tail is gone; report the truncated length.
	return out[:done], nil
}

// Close releases the underlying file.
func (r *RangeReader) Close() error { return r.file.Close() }

// chunkBytes rounds the fixed read window down to a whole number of frames.
func (r *RangeReader) chunkBytes() int {
	bytes := readChunkBytes / r.stride * r.stride
	if bytes < r.stride {
		bytes = r.stride
	}
	return bytes
}

// decodeChunk converts len(dst) complete interleaved frames from src.
func (r *RangeReader) decodeChunk(dst []float32, src []byte) {
	if r.channels == 1 {
		for i := range dst {
			dst[i] = r.decode(src[i*r.stride:])
		}
		return
	}
	// Stereo mixdown: the arithmetic mean of the two channels, in float32, as
	// `(frame[0] + frame[1]) * 0.5` (wav.rs:132). Keep it one add and one
	// multiply by a power of two; accumulating frames in f64 (a "batched"
	// mixdown) rounds differently and moves the last bit the goldens compare.
	for i := range dst {
		left := r.decode(src[i*r.stride:])
		right := r.decode(src[i*r.stride+r.sampleBytes:])
		dst[i] = (left + right) * 0.5
	}
}

// sampleDecoder converts one encoded channel sample to float32.
type sampleDecoder func([]byte) float32

// decoderFor returns the decoder for an encoding, and false when the engine
// does not read it. The divisors are hound's, quoted from wav.rs:88-117.
func decoderFor(bits int, isFloat bool) (sampleDecoder, bool) {
	if isFloat {
		if bits != 32 {
			return nil, false
		}
		return func(b []byte) float32 {
			return math.Float32frombits(binary.LittleEndian.Uint32(b))
		}, true
	}
	switch bits {
	case 16:
		return func(b []byte) float32 {
			return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
		}, true
	case 24:
		return func(b []byte) float32 {
			v := int32(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16)
			// Sign-extend 24 bits to 32 before the conversion, as hound does
			// when it hands back an i32.
			v = v << 8 >> 8
			return float32(v) / 8388608
		}, true
	case 32:
		return func(b []byte) float32 {
			return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
		}, true
	}
	return nil, false
}

// formatName is hound's SampleFormat spelling, used in the error message.
func formatName(isFloat bool) string {
	if isFloat {
		return "Float"
	}
	return "Int"
}

// readHeader walks the RIFF chunks, locates `fmt ` and `data`, and returns a
// reader for the frames the data chunk really holds.
func readHeader(file *os.File, path string) (*RangeReader, error) {
	ioErr := func(format string, args ...any) error {
		return &Io{Path: path, Msg: fmt.Sprintf(format, args...)}
	}

	var riff [12]byte
	if _, err := io.ReadFull(file, riff[:]); err != nil {
		return nil, ioErr("read RIFF header: %v", err)
	}
	if magic := string(riff[0:4]); magic != "RIFF" {
		return nil, ioErr("not a RIFF file (magic %q)", magic)
	}
	if form := string(riff[8:12]); form != "WAVE" {
		return nil, ioErr("RIFF form %q is not WAVE", form)
	}

	info, err := file.Stat()
	if err != nil {
		return nil, ioErr("%v", err)
	}
	fileSize := info.Size()

	var (
		haveFmt, haveData  bool
		channels, bits     int
		isFloat            bool
		rate               uint32
		dataStart, dataLen int64
	)

	header := make([]byte, 8)
	offset := int64(len(riff))
	for {
		if _, err := io.ReadFull(file, header); err != nil {
			// A chunk header cut short is where a truncated file ends; the
			// missing-chunk checks below decide whether that is fatal.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, ioErr("read chunk header: %v", err)
		}
		id := string(header[0:4])
		size := int64(binary.LittleEndian.Uint32(header[4:8]))
		body := offset + 8
		next := body + size + size%2 // RIFF pads odd payloads to even

		switch id {
		case "fmt ":
			if haveFmt {
				break
			}
			if size < 16 {
				return nil, ioErr("fmt chunk too short (%d bytes)", size)
			}
			if size > maxFmtChunkSize {
				return nil, ioErr("fmt chunk too large (%d bytes, limit %d)", size, maxFmtChunkSize)
			}
			payload := make([]byte, int(size))
			if _, err := io.ReadFull(file, payload); err != nil {
				return nil, ioErr("read fmt chunk: %v", err)
			}
			tag := binary.LittleEndian.Uint16(payload[0:2])
			channels = int(binary.LittleEndian.Uint16(payload[2:4]))
			rate = binary.LittleEndian.Uint32(payload[4:8])
			bits = int(binary.LittleEndian.Uint16(payload[14:16]))
			switch tag {
			case formatPCM:
				isFloat = false
			case formatFloat:
				isFloat = true
			case formatExtensible:
				// cbSize, valid bits, channel mask, then the 16-byte
				// sub-format GUID: its first two bytes are the real tag.
				if size < 40 {
					return nil, ioErr("WAVE_FORMAT_EXTENSIBLE fmt chunk too short (%d bytes)", size)
				}
				sub := binary.LittleEndian.Uint16(payload[24:26])
				switch sub {
				case formatPCM:
					isFloat = false
				case formatFloat:
					isFloat = true
				default:
					return nil, &UnsupportedWav{Msg: fmt.Sprintf(
						"extensible sub-format 0x%04x; expected PCM (0x0001) or IEEE float (0x0003)", sub)}
				}
			default:
				return nil, &UnsupportedWav{Msg: fmt.Sprintf(
					"format tag 0x%04x; expected PCM (0x0001) or IEEE float (0x0003)", tag)}
			}
			haveFmt = true
		case "data":
			if !haveData {
				dataStart, dataLen, haveData = body, size, true
			}
		}

		offset = next
		if _, err := file.Seek(next, io.SeekStart); err != nil {
			return nil, ioErr("seek to chunk offset %d: %v", next, err)
		}
	}

	if !haveFmt {
		return nil, ioErr("no fmt chunk before end of file")
	}
	if !haveData {
		return nil, ioErr("no data chunk before end of file")
	}
	// Channels first: wav.rs:45-51 rejects the layout at open, before any
	// encoding question.
	if channels < 1 || channels > 2 {
		return nil, &UnsupportedWav{Msg: fmt.Sprintf(
			"%d channel(s); only mono or stereo is supported", channels)}
	}
	decode, ok := decoderFor(bits, isFloat)
	if !ok {
		return nil, &UnsupportedWav{Msg: fmt.Sprintf(
			"%d-bit %s samples; expected 16/24/32-bit int or 32-bit float", bits, formatName(isFloat))}
	}

	// block align is derived from the layout rather than trusted from the
	// header, so a wrong field cannot misalign the decode.
	sampleBytes := bits / 8
	stride := channels * sampleBytes

	// A data chunk that claims more bytes than the file holds — a cut-short
	// file, or a streamed header with 0xFFFFFFFF — is clamped to the real end.
	if available := fileSize - dataStart; dataLen > available {
		dataLen = available
	}
	if dataLen < 0 {
		dataLen = 0
	}

	return &RangeReader{
		file:        file,
		path:        path,
		dataStart:   dataStart,
		frames:      dataLen / int64(stride),
		channels:    channels,
		rate:        rate,
		sampleBytes: sampleBytes,
		stride:      stride,
		decode:      decode,
	}, nil
}
