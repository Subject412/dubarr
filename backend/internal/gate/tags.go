package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds each mkvtoolnix invocation. Like probe's timeout it
	// is a hang guard for wedged children, not a performance budget.
	DefaultTimeout = 30 * time.Second

	mkvmergeBin   = "mkvmerge"
	mkvextractBin = "mkvextract"
)

// Dependencies configures a Tagger. All fields are optional.
type Dependencies struct {
	// MKVMergePath / MKVExtractPath pin the binaries; empty resolves them on PATH.
	MKVMergePath   string
	MKVExtractPath string
	// LookPath overrides binary discovery (defaults to exec.LookPath).
	LookPath func(string) (string, error)
	// Run executes a tool and returns its stdout. Tests and future sandboxing
	// substitute it; nil uses exec.CommandContext.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Timeout bounds one invocation; 0 selects DefaultTimeout.
	Timeout time.Duration
}

// Tags is the tag information observed on one target file.
type Tags struct {
	Path string
	// Container holds the file's global (non-track, non-chapter) Matroska tags.
	Container []Tag
}

// Tagger reads Matroska container tags through mkvtoolnix.
type Tagger struct {
	lookPath       func(string) (string, error)
	run            func(ctx context.Context, name string, args ...string) ([]byte, error)
	timeout        time.Duration
	mkvmergePath   string
	mkvExtractPath string

	mu         sync.Mutex
	mkvmerge   string
	mkvextract string
	resolved   bool
	resolveErr error
}

// New builds a Tagger. A missing mkvtoolnix is only reported from Read, so the
// backend still starts (with a degraded /api/health) without it.
func New(deps Dependencies) *Tagger {
	t := &Tagger{
		lookPath:       deps.LookPath,
		run:            deps.Run,
		timeout:        deps.Timeout,
		mkvmergePath:   deps.MKVMergePath,
		mkvExtractPath: deps.MKVExtractPath,
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

// Read returns the container-level tags of the Matroska file at path.
//
// `mkvmerge -J` first identifies the file (this rejects non-Matroska and
// corrupt inputs and reports whether any global tag exists at all), then
// `mkvextract tags` yields the tag names/values, which mkvmerge's JSON
// identification does not include.
func (t *Tagger) Read(ctx context.Context, path string) (*Tags, error) {
	mkvmerge, mkvextract, err := t.tools()
	if err != nil {
		return nil, err
	}

	raw, err := t.invoke(ctx, mkvmerge, "-J", path)
	if err != nil {
		return nil, fmt.Errorf("gate: identify %s: %w", path, err)
	}
	doc, err := parseIdentify(path, raw)
	if err != nil {
		return nil, err
	}
	tags := &Tags{Path: path, Container: []Tag{}}
	if len(doc.GlobalTags) == 0 {
		// No global tags: mkvextract would exit non-zero with "no tags found",
		// and track tags never carry the gate marker.
		return tags, nil
	}

	raw, err = t.invoke(ctx, mkvextract, path, "tags")
	if err != nil {
		return nil, fmt.Errorf("gate: extract tags from %s: %w", path, err)
	}
	container, err := parseTagsXML(path, raw)
	if err != nil {
		return nil, err
	}
	tags.Container = container
	return tags, nil
}

// tools resolves both binaries once. mkvextract ships with mkvmerge, so a
// missing extractor is a broken installation worth surfacing.
func (t *Tagger) tools() (mkvmerge, mkvextract string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved {
		return t.mkvmerge, t.mkvextract, t.resolveErr
	}
	t.resolved = true
	// A miss is not cached: installing mkvtoolnix into a running container is
	// picked up on the next call, mirroring internal/probe.
	t.mkvmerge, t.resolveErr = t.resolve(t.mkvmergePath, mkvmergeBin)
	if t.resolveErr != nil {
		t.resolved = false
		return "", "", t.resolveErr
	}
	t.mkvextract, t.resolveErr = t.resolve(t.mkvExtractPath, mkvextractBin)
	if t.resolveErr != nil {
		t.mkvmerge = ""
		t.resolved = false
		return "", "", t.resolveErr
	}
	t.resolveErr = nil
	return t.mkvmerge, t.mkvextract, nil
}

// resolve returns a pinned path verbatim, otherwise looks the default name up
// on PATH.
func (t *Tagger) resolve(pinned, name string) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	path, err := t.lookPath(name)
	if err != nil {
		return "", fmt.Errorf("gate: %s not found on PATH: %w", name, err)
	}
	return path, nil
}

// invoke runs one tool under the hang-guard timeout and prefixes failures with
// the tool name; stderr is folded into the error by execRun.
func (t *Tagger) invoke(ctx context.Context, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, err := t.run(ctx, bin, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// execRun executes a tool, returning stdout. mkvmerge exits 1 for non-fatal
// warnings while still printing usable JSON, so a code-1 exit with stdout is
// accepted; anything else fails with the captured stderr.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(bytes.TrimSpace(out)) > 0 {
		return out, nil
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return out, fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return out, fmt.Errorf("%s: %w", name, err)
}

// identifyDoc is the subset of `mkvmerge -J` output the gate needs.
type identifyDoc struct {
	Container struct {
		Type string `json:"type"`
	} `json:"container"`
	GlobalTags []struct {
		NumEntries int `json:"num_entries"`
	} `json:"global_tags"`
	Errors []string `json:"errors"`
}

func parseIdentify(path string, raw []byte) (*identifyDoc, error) {
	var doc identifyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("gate: decode mkvmerge identification of %s: %w", path, err)
	}
	if len(doc.Errors) > 0 {
		return nil, fmt.Errorf("gate: mkvmerge cannot read %s: %s", path, strings.Join(doc.Errors, "; "))
	}
	if doc.Container.Type == "" {
		return nil, fmt.Errorf("gate: %s is not a recognised Matroska container", path)
	}
	return &doc, nil
}

// tagsDoc mirrors `mkvextract tags` XML (Matroska tag XML).
type tagsDoc struct {
	Tags []struct {
		// Targets is non-nil and non-empty for track/edition/chapter/attachment
		// scoped tags; a global tag has an empty Targets element.
		Targets *struct {
			TrackUIDs      []string `xml:"TrackUID"`
			EditionUIDs    []string `xml:"EditionUID"`
			ChapterUIDs    []string `xml:"ChapterUID"`
			AttachmentUIDs []string `xml:"AttachmentUID"`
		} `xml:"Targets"`
		Simple []struct {
			Name   string `xml:"Name"`
			String string `xml:"String"`
		} `xml:"Simple"`
	} `xml:"Tag"`
}

// parseTagsXML returns the global tags from mkvextract's XML document.
//
// mkvextract prefixes its output with a UTF-8 BOM, which encoding/xml rejects,
// so it is trimmed first.
func parseTagsXML(path string, raw []byte) ([]Tag, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var doc tagsDoc
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("gate: decode tags of %s: %w", path, err)
	}
	out := make([]Tag, 0, len(doc.Tags))
	for _, tag := range doc.Tags {
		if tag.Targets != nil && (len(tag.Targets.TrackUIDs) > 0 ||
			len(tag.Targets.EditionUIDs) > 0 ||
			len(tag.Targets.ChapterUIDs) > 0 ||
			len(tag.Targets.AttachmentUIDs) > 0) {
			continue // track-scoped tag, not a container marker
		}
		for _, simple := range tag.Simple {
			out = append(out, Tag{Name: simple.Name, Value: simple.String})
		}
	}
	return out, nil
}
