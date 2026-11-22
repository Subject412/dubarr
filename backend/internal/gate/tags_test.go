package gate

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mkvTool resolves an mkvtoolnix binary, skipping the test when the host does
// not have it. The tools live on PATH in production and in CI containers.
func mkvTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH: %v", name, err)
	}
	return path
}

// writeTestWAV writes a 0.1 s mono 16-bit 8 kHz PCM WAV: a valid, tiny
// Matroska payload source that needs no ffmpeg.
func writeTestWAV(t *testing.T, path string) {
	t.Helper()
	const (
		sampleRate = 8000
		samples    = 800 // 0.1 s
		dataLen    = samples * 2
	)
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(make([]byte, dataLen))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
}

// buildTestMKV muxes a tiny WAV into name inside dir. When tagValue is
// non-empty it writes a global Matroska tag (name "DUBARR_GATE") carrying that
// value, exactly the way a muxer records a container-level marker.
func buildTestMKV(t *testing.T, dir, name, tagValue string) string {
	t.Helper()
	mkvmerge := mkvTool(t, "mkvmerge")

	wav := filepath.Join(dir, "source.wav")
	if _, err := os.Stat(wav); err != nil {
		writeTestWAV(t, wav)
	}
	out := filepath.Join(dir, name)
	args := []string{"-o", out, "--quiet"}
	if tagValue != "" {
		xmlPath := filepath.Join(dir, "tags.xml")
		doc := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Tags><Tag><Simple><Name>DUBARR_GATE</Name><String>%s</String></Simple></Tag></Tags>`, tagValue)
		if err := os.WriteFile(xmlPath, []byte(doc), 0o600); err != nil {
			t.Fatalf("write tags xml: %v", err)
		}
		args = append(args, "--global-tags", xmlPath)
	}
	args = append(args, wav)
	if outp, err := exec.Command(mkvmerge, args...).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge: %v: %s", err, outp)
	}
	return out
}

func TestTaggerReadsContainerTags(t *testing.T) {
	dir := t.TempDir()
	path := buildTestMKV(t, dir, "tagged.mkv", "[ArcA310]")

	tagger := New(Dependencies{MKVMergePath: mkvTool(t, "mkvmerge"), MKVExtractPath: mkvTool(t, "mkvextract")})
	tags, err := tagger.Read(context.Background(), path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	found := false
	for _, tag := range tags.Container {
		if tag.Name == "DUBARR_GATE" && tag.Value == "[ArcA310]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("container tags = %+v, want DUBARR_GATE=[ArcA310]", tags.Container)
	}

	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}
	if got := r.Classify(filepath.Base(path), tags.Container); got.Status != StatusEligible {
		t.Errorf("classification = %+v, want eligible", got)
	}
}

func TestTaggerUntaggedFileHasNoGateTag(t *testing.T) {
	dir := t.TempDir()
	path := buildTestMKV(t, dir, "plain.mkv", "")

	tagger := New(Dependencies{MKVMergePath: mkvTool(t, "mkvmerge"), MKVExtractPath: mkvTool(t, "mkvextract")})
	tags, err := tagger.Read(context.Background(), path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	r := Rule{Required: []string{"[ArcA310]"}, Mode: MatchAny, Enforce: true}
	if got := r.Classify(filepath.Base(path), tags.Container); got.Status != StatusWaitingForTags {
		t.Errorf("classification = %+v, want waiting_for_tags", got)
	}
}

func TestTaggerRejectsNonMatroska(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-video.mkv")
	if err := os.WriteFile(path, []byte("definitely not matroska"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	tagger := New(Dependencies{MKVMergePath: mkvTool(t, "mkvmerge"), MKVExtractPath: mkvTool(t, "mkvextract")})
	if _, err := tagger.Read(context.Background(), path); err == nil {
		t.Fatal("Read of a corrupt file returned no error")
	}
}

func TestTaggerMissingTool(t *testing.T) {
	tagger := New(Dependencies{LookPath: func(string) (string, error) { return "", exec.ErrNotFound }})
	if _, err := tagger.Read(context.Background(), filepath.Join(t.TempDir(), "x.mkv")); err == nil {
		t.Fatal("Read without mkvtoolnix on PATH returned no error")
	}
}
