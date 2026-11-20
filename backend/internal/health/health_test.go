package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// fakeProber wires deterministic lookPath/version seams into a service so tool
// detection can be tested without depending on the host's PATH.
func fakeProber(found map[string]string, versions map[string]string) *Service {
	lookPath := func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", exec.ErrNotFound
	}
	version := func(_ context.Context, _ string, path string) (string, error) {
		return versions[path], nil
	}
	return New(Dependencies{LookPath: lookPath, VersionCommand: version})
}

func getHealth(t *testing.T, s *Service) (int, map[string]any, string) {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, body, rec.Body.String()
}

func TestHealthReportsToolAvailabilityAndVersions(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := fakeProber(
		map[string]string{
			"ffmpeg":   "/usr/bin/ffmpeg",
			"ffprobe":  "/usr/bin/ffprobe",
			"mkvmerge": "/usr/bin/mkvmerge",
		},
		map[string]string{
			"/usr/bin/ffmpeg":   "ffmpeg version n9.0.2 Copyright (c) 2000-2026 the FFmpeg developers",
			"/usr/bin/ffprobe":  "ffprobe version n9.0.2 Copyright (c) 2007-2026 the FFmpeg developers",
			"/usr/bin/mkvmerge": "mkvmerge v103.0 ('The Darkness') 64-bit",
		},
	)
	svc.db = st.DB
	svc.version = "1.2.3"

	code, body, raw := getHealth(t, svc)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, raw)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
	if body["version"] != "1.2.3" {
		t.Errorf("version = %v, want 1.2.3", body["version"])
	}
	if body["db_ok"] != true {
		t.Errorf("db_ok = %v, want true", body["db_ok"])
	}

	tools, ok := body["tools"].(map[string]any)
	if !ok {
		t.Fatalf("tools missing or wrong type: %v", body["tools"])
	}
	wantVersions := map[string]string{
		"ffmpeg":   "n9.0.2",
		"ffprobe":  "n9.0.2",
		"mkvmerge": "103.0",
	}
	for name, want := range wantVersions {
		tool, ok := tools[name].(map[string]any)
		if !ok {
			t.Fatalf("tools[%s] missing: %v", name, tools[name])
		}
		if tool["available"] != true {
			t.Errorf("%s available = %v, want true", name, tool["available"])
		}
		if tool["version"] != want {
			t.Errorf("%s version = %v, want %q", name, tool["version"], want)
		}
		if tool["path"] == "" {
			t.Errorf("%s path is empty", name)
		}
	}
}

func TestHealthReportsMissingToolGracefully(t *testing.T) {
	svc := fakeProber(map[string]string{"/usr/bin/ffmpeg": "/usr/bin/ffmpeg"}, nil)
	svc.db = nil // also exercises the db-unavailable path

	code, body, raw := getHealth(t, svc)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, raw)
	}
	if body["db_ok"] != false {
		t.Errorf("db_ok = %v, want false", body["db_ok"])
	}
	if body["status"] != "degraded" {
		t.Errorf("status = %v, want degraded when db is unavailable", body["status"])
	}
	tools := body["tools"].(map[string]any)
	probe := tools["ffprobe"].(map[string]any)
	if probe["available"] != false {
		t.Errorf("ffprobe available = %v, want false", probe["available"])
	}
	if probe["version"] != "" || probe["path"] != "" {
		t.Errorf("missing tool leaked metadata: %v", probe)
	}
}

func TestHealthDetectsRealHostTools(t *testing.T) {
	svc := New(Dependencies{})
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	var body struct {
		Tools map[string]struct {
			Available bool   `json:"available"`
			Version   string `json:"version"`
			Path      string `json:"path"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, name := range []string{"ffmpeg", "ffprobe", "mkvmerge"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Logf("skipping %s: not on PATH", name)
			continue
		}
		tool := body.Tools[name]
		if !tool.Available {
			t.Errorf("%s available = false, want true", name)
		}
		if tool.Version == "" {
			t.Errorf("%s version is empty", name)
		}
		if !strings.Contains(tool.Path, name) {
			t.Errorf("%s path = %q, want a path containing %q", name, tool.Path, name)
		}
	}
}

// TestHealthDoesNotHangOnHungTool is the regression guard for IMP-1: a tool
// binary that never exits must not stall the public /api/health endpoint. The
// fake version seam blocks until its context is cancelled, so the only way this
// test can pass is if the service itself bounds the probe with
// versionProbeTimeout and passes that context down the seam.
func TestHealthDoesNotHangOnHungTool(t *testing.T) {
	lookPath := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	version := func(ctx context.Context, name, _ string) (string, error) {
		if name == "ffmpeg" {
			<-ctx.Done() // simulate a binary that never terminates
			return "", ctx.Err()
		}
		return name + " version 1.2.3", nil
	}
	svc := New(Dependencies{LookPath: lookPath, VersionCommand: version})
	svc.db = nil
	// Keep the suite fast: the production budget is versionProbeTimeout, but the
	// wiring under test is the same at any duration.
	svc.probeTimeout = 50 * time.Millisecond
	budget := svc.probeTimeout

	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)

	type result struct {
		code int
		body map[string]any
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			done <- result{code: -1}
			return
		}
		done <- result{code: rec.Code, body: body}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(10 * budget):
		t.Fatalf("GET /api/health did not return within %v with a hung tool", 10*budget)
	}
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("health took %v with a hung tool, want a prompt bounded return", elapsed)
	}
	if got.code != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.code)
	}
	tools, ok := got.body["tools"].(map[string]any)
	if !ok {
		t.Fatalf("tools missing: %v", got.body["tools"])
	}
	hung := tools["ffmpeg"].(map[string]any)
	if hung["version"] != "" {
		t.Errorf("hung tool version = %v, want empty", hung["version"])
	}
	// A hung probe must not suppress the healthy tools' results.
	for _, name := range []string{"ffprobe", "mkvmerge", "dubarr-sync"} {
		tool, ok := tools[name].(map[string]any)
		if !ok {
			t.Fatalf("tools[%s] missing", name)
		}
		if tool["available"] != true || tool["version"] != "1.2.3" {
			t.Errorf("%s = %v, want available with version 1.2.3", name, tool)
		}
	}
}

// TestProbeVersionHonorsCanceledContext pins the exec.CommandContext behaviour:
// an already-dead context must return promptly instead of spawning the process.
func TestProbeVersionHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if _, err := probeVersion(ctx, "sleep", "/bin/sleep"); err == nil {
		t.Fatal("probeVersion with canceled context = nil error, want non-nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("probeVersion took %v with a canceled context, want prompt return", elapsed)
	}
}

func TestHealthNeverLeaksSecretsOrKeys(t *testing.T) {
	svc := New(Dependencies{
		KeyringAvailable:   func() bool { return true },
		SecretsInitialized: func() bool { return true },
		AuthInitialized:    func() bool { return true },
		VersionCommand:     func(context.Context, string, string) (string, error) { return "", exec.ErrNotFound },
	})
	_, body, raw := getHealth(t, svc)
	for _, forbidden := range []string{"secret", "password", "master_key", "session_key", "token"} {
		if strings.Contains(strings.ToLower(raw), forbidden+"\"") {
			t.Errorf("health response contains %q key: %s", forbidden, raw)
		}
	}
	if body["keyring_available"] != true || body["secrets_initialized"] != true || body["auth_initialized"] != true {
		t.Errorf("status flags = %v", body)
	}
}
