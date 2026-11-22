package analyze

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// maxFmtChunkSize bounds the fmt payload we are willing to read. The canonical
// PCM fmt chunk is 16 bytes and WAVE_FORMAT_EXTENSIBLE adds 24; anything past
// this is malformed and trusting the file-controlled uint32 would allow a
// multi-gigabyte allocation.
const maxFmtChunkSize = 64 << 10

// wavDuration reads the playable duration from a RIFF/WAVE header without
// decoding samples. The engine accepts any PCM/float WAV; data size over byte
// rate is exact for constant-bitrate PCM, which is all the sync stage stages.
func wavDuration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, fmt.Errorf("read RIFF header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, fmt.Errorf("%s is not a RIFF/WAVE file", path)
	}

	var byteRate uint32
	var dataSize uint32
	haveFmt := false
	var chunk [8]byte
	for {
		if _, err := io.ReadFull(f, chunk[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return 0, fmt.Errorf("read chunk header: %w", err)
		}
		id := string(chunk[0:4])
		size := binary.LittleEndian.Uint32(chunk[4:8])
		switch id {
		case "fmt ":
			if size < 16 {
				return 0, fmt.Errorf("fmt chunk too short (%d bytes)", size)
			}
			if size > maxFmtChunkSize {
				return 0, fmt.Errorf("fmt chunk too large (%d bytes, limit %d)", size, maxFmtChunkSize)
			}
			// Only the 16 fixed bytes are needed; skip the rest of the payload
			// (plus RIFF's pad byte when the payload is odd) so the next chunk
			// header is read at the right offset.
			var body [16]byte
			if _, err := io.ReadFull(f, body[:]); err != nil {
				return 0, fmt.Errorf("read fmt chunk: %w", err)
			}
			if rest := int64(size) - 16 + int64(size%2); rest > 0 {
				if _, err := f.Seek(rest, io.SeekCurrent); err != nil {
					return 0, fmt.Errorf("skip fmt chunk payload: %w", err)
				}
			}
			byteRate = binary.LittleEndian.Uint32(body[8:12])
			haveFmt = true
		case "data":
			dataSize = size
			if !haveFmt {
				return 0, fmt.Errorf("data chunk before fmt chunk")
			}
			if byteRate == 0 {
				return 0, fmt.Errorf("fmt chunk has zero byte rate")
			}
			return float64(dataSize) / float64(byteRate), nil
		default:
			// Skip the payload, honouring RIFF's odd-size padding byte.
			skip := int64(size)
			if size%2 == 1 {
				skip++
			}
			if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
				return 0, fmt.Errorf("skip %q chunk: %w", id, err)
			}
		}
	}
	return 0, fmt.Errorf("no data chunk in %s", path)
}
