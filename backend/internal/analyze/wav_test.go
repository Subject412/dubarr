package analyze

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testChunk builds a RIFF chunk (id + uint32 size + payload) and appends the
// pad byte RIFF requires after an odd-size payload.
func testChunk(id string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	copy(b[0:4], id)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	if len(payload)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// testFmt builds a 16-byte PCM fmt payload with the given byte rate.
func testFmt(byteRate uint32) []byte {
	p := make([]byte, 16)
	binary.LittleEndian.PutUint16(p[0:2], 1) // PCM
	binary.LittleEndian.PutUint16(p[2:4], 1) // mono
	binary.LittleEndian.PutUint32(p[4:8], 8000)
	binary.LittleEndian.PutUint32(p[8:12], byteRate)
	binary.LittleEndian.PutUint16(p[12:14], 2) // block align
	binary.LittleEndian.PutUint16(p[14:16], 16)
	return p
}

// testWAV wraps chunks in a RIFF/WAVE container.
func testWAV(chunks ...[]byte) []byte {
	body := []byte("WAVE")
	for _, c := range chunks {
		body = append(body, c...)
	}
	b := make([]byte, 8, 8+len(body))
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(body)))
	return append(b, body...)
}

func writeTestFile(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.wav")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestWAVDuration(t *testing.T) {
	// 32000 bytes at 16000 B/s == 2 s.
	normal := testWAV(testChunk("fmt ", testFmt(16000)), testChunk("data", make([]byte, 32000)))
	cases := []struct {
		name string
		raw  []byte
		want float64
	}{
		{"normal", normal, 2},
		{"empty data", testWAV(testChunk("fmt ", testFmt(16000)), testChunk("data", nil)), 0},
		{"extensible fmt", testWAV(testChunk("fmt ", append(testFmt(16000), 0, 0)), testChunk("data", make([]byte, 16000))), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wavDuration(writeTestFile(t, tc.raw))
			if err != nil {
				t.Fatalf("wavDuration: %v", err)
			}
			if got != tc.want {
				t.Fatalf("duration = %v, want %v", got, tc.want)
			}
		})
	}
}

// A 17-byte fmt chunk carries a pad byte before the next chunk header. Missing
// the pad desynchronises the parser and loses the data chunk.
func TestWAVDurationOddFmtSizePad(t *testing.T) {
	payload := append(testFmt(16000), 0xAA)
	raw := testWAV(testChunk("fmt ", payload), testChunk("data", make([]byte, 32000)))
	got, err := wavDuration(writeTestFile(t, raw))
	if err != nil {
		t.Fatalf("wavDuration: %v", err)
	}
	if got != 2 {
		t.Fatalf("duration = %v, want 2", got)
	}
}

func TestWAVDurationErrors(t *testing.T) {
	hugeFmt := testWAV(append([]byte("fmt "), 0xFF, 0xFF, 0xFF, 0xFF))
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"not riff", []byte("RIFFxxxxAVI "), "not a RIFF/WAVE"},
		{"missing data", testWAV(testChunk("fmt ", testFmt(16000)), testChunk("LIST", []byte("INFO"))), "no data chunk"},
		{"zero byte rate", testWAV(testChunk("fmt ", testFmt(0)), testChunk("data", make([]byte, 10))), "zero byte rate"},
		{"short fmt", testWAV(testChunk("fmt ", make([]byte, 12)), testChunk("data", make([]byte, 10))), "fmt chunk too short"},
		{"oversized fmt", hugeFmt, "fmt chunk too large"},
		{"data before fmt", testWAV(testChunk("data", make([]byte, 10)), testChunk("fmt ", testFmt(16000))), "data chunk before fmt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := wavDuration(writeTestFile(t, tc.raw)); err == nil {
				t.Fatalf("wavDuration(%s) = nil error, want %q", tc.name, tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := wavDuration(filepath.Join(t.TempDir(), "absent.wav")); err == nil {
		t.Fatal("wavDuration(missing file) = nil error")
	}
}
