package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/secrets"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	// Keyring:nil keeps tests on the 0600 keyfile path; tests must never write
	// to the developer's real keyring.
	srv, err := newServerWithSecrets(
		Config{DataDir: dir, Listen: "127.0.0.1:0"},
		secrets.Options{DataDir: dir},
	)
	if err != nil {
		t.Fatalf("newServerWithSecrets: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return srv
}

// useKeyfileSecrets swaps the production server factory for one whose secrets
// backend never touches an OS keyring.
func useKeyfileSecrets(t *testing.T) {
	t.Helper()
	prev := newServerFn
	newServerFn = func(cfg Config) (*server, error) {
		return newServerWithSecrets(cfg, secrets.Options{DataDir: cfg.DataDir})
	}
	t.Cleanup(func() { newServerFn = prev })
}

func TestEmbeddedSPAServesIndexAndFallsBack(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	for _, p := range []string{"/", "/library/movies", "/settings"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Dubarr") {
			t.Fatalf("GET %s did not serve the embedded index.html: %s", p, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown /api path = %d, want 404 (no SPA fallback for the API)", rec.Code)
	}
}

func TestFullRouteFlow(t *testing.T) {
	srv := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, func(addr string) { addrCh <- addr }) }()

	var base string
	select {
	case addr := <-addrCh:
		base = "http://" + addr
	case err := <-done:
		t.Fatalf("Serve exited before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	post := func(path, body string) (int, string) {
		resp, err := client.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	getInto := func(path string, dst any) int {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if dst != nil {
			if err := json.Unmarshal(raw, dst); err != nil {
				t.Fatalf("GET %s decode: %v (%s)", path, err, raw)
			}
		}
		return resp.StatusCode
	}

	// 1. Fresh install: public status reports uninitialized.
	var status struct {
		Initialized      bool `json:"initialized"`
		Authenticated    bool `json:"authenticated"`
		KeyringAvailable bool `json:"keyring_available"`
	}
	if code := getInto("/api/auth/status", &status); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if status.Initialized || status.Authenticated {
		t.Fatalf("fresh status = %+v, want initialized=false authenticated=false", status)
	}

	// 2. Setup creates the admin and logs in.
	if code, body := post("/api/auth/setup", `{"username":"admin","password":"correct horse"}`); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("setup = %d %s", code, body)
	}

	// 3. Authenticated session: status reflects it.
	if code := getInto("/api/auth/status", &status); code != http.StatusOK {
		t.Fatalf("status after setup = %d", code)
	}
	if !status.Initialized || !status.Authenticated {
		t.Fatalf("status after setup = %+v, want initialized+authenticated", status)
	}

	// 4. Settings are seeded and never expose privileged rows.
	var settingsBody struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if code := getInto("/api/settings", &settingsBody); code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d", code)
	}
	if len(settingsBody.Settings) == 0 {
		t.Fatal("settings are empty; Seed did not run")
	}
	if _, ok := settingsBody.Settings["auth.session_key"]; ok {
		t.Fatal("settings leaked auth.session_key")
	}

	// 5. Health reports the toolchain without secrets.
	var health struct {
		Status string `json:"status"`
		DBOK   bool   `json:"db_ok"`
		Tools  map[string]struct {
			Available bool   `json:"available"`
			Version   string `json:"version"`
		} `json:"tools"`
		AuthInitialized bool `json:"auth_initialized"`
	}
	if code := getInto("/api/health", &health); code != http.StatusOK {
		t.Fatalf("GET /api/health = %d", code)
	}
	if !health.DBOK || health.Status != "ok" {
		t.Fatalf("health = %+v, want db_ok and status ok", health)
	}
	if !health.AuthInitialized {
		t.Fatal("health.auth_initialized = false after setup")
	}
	for _, name := range []string{"ffmpeg", "ffprobe", "mkvmerge"} {
		tool, ok := health.Tools[name]
		if !ok {
			t.Fatalf("health.tools missing %s: %+v", name, health.Tools)
		}
		if tool.Available && tool.Version == "" {
			t.Fatalf("%s reported available without a version", name)
		}
	}

	// 6. Admin-only browse works while the wizard is still unconfigured.
	if code := getInto("/api/system/browse", nil); code != http.StatusOK {
		t.Fatalf("GET /api/system/browse = %d", code)
	}

	// 7. Unauthenticated clients stay locked out of privileged routes.
	anon := &http.Client{Timeout: 5 * time.Second}
	resp, err := anon.Get(base + "/api/settings")
	if err != nil {
		t.Fatalf("anon GET settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon /api/settings = %d, want 401", resp.StatusCode)
	}

	// 8. Graceful shutdown: cancel stops Serve cleanly.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after shutdown, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down")
	}

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepting connections after shutdown")
	}
}

func TestServeReportsListenErrors(t *testing.T) {
	srv := newTestServer(t)
	// Occupy a port, then ask the server to bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv.cfg.Listen = ln.Addr().String()

	err = srv.Serve(context.Background(), func(string) {})
	if err == nil {
		t.Fatal("Serve succeeded on an occupied port")
	}
}

// serveForTest starts srv.Serve and returns its base URL plus the channel
// carrying Serve's result.
func serveForTest(t *testing.T, srv *server, ctx context.Context) (string, <-chan error) {
	t.Helper()
	addrCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, func(addr string) { addrCh <- addr }) }()
	select {
	case addr := <-addrCh:
		return "http://" + addr, done
	case err := <-done:
		t.Fatalf("Serve exited before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	return "", nil
}

// healthResult is the client-side outcome of one GET /api/health.
type healthResult struct {
	code   int
	status string
	dbOK   bool
	auth   bool
	body   []byte
	err    error
}

func getHealth(base string) healthResult {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(base + "/api/health")
	if err != nil {
		return healthResult{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return healthResult{err: err}
	}
	out := healthResult{code: resp.StatusCode, body: body}
	var payload struct {
		Status          string `json:"status"`
		DBOK            bool   `json:"db_ok"`
		AuthInitialized bool   `json:"auth_initialized"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		out.err = err
		return out
	}
	out.status, out.dbOK, out.auth = payload.Status, payload.DBOK, payload.AuthInitialized
	return out
}

// gateHealthHandler wraps the wired mux so exactly one request parks inside the
// HTTP server until release is closed. It is the synchronisation seam for the
// issue-#62 tests: the overlap is forced deterministically, never by sleeping.
func gateHealthHandler(mux *http.ServeMux, entered chan struct{}, release chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		mux.ServeHTTP(w, r)
	})
}

// waitForSignal fails the test instead of hanging when a synchronisation point
// never arrives.
func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestHealthReportsDegradedWhenStoreClosedMidRequest reproduces issue #62 at the
// HTTP layer: /api/health is executing when the store handle is released.
//
// What it would have caught: before the fix Store.Close set s.DB = nil, so the
// AuthInitialized closure dereferenced a nil *sql.DB here and panicked the
// process. It now gets a closed-database error and reports db_ok=false,
// status=degraded and auth_initialized=false instead of pretending to be healthy.
func TestHealthReportsDegradedWhenStoreClosedMidRequest(t *testing.T) {
	srv := newTestServer(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	srv.http.Handler = gateHealthHandler(srv.mux, entered, release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, serveDone := serveForTest(t, srv, ctx)

	resCh := make(chan healthResult, 1)
	go func() { resCh <- getHealth(base) }()

	waitForSignal(t, entered, "the health request to reach the server")
	// Release the pool while the handler sits between its gate and its queries:
	// exactly the interleaving that used to nil out st.DB under the handler.
	if err := srv.store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	close(release)

	res := <-resCh
	if res.err != nil {
		t.Fatalf("GET /api/health: %v", res.err)
	}
	if res.code != http.StatusOK {
		t.Fatalf("GET /api/health = %d, want 200 (%s)", res.code, res.body)
	}
	if res.dbOK {
		t.Fatalf("db_ok = true after the store was closed (%s)", res.body)
	}
	if res.status != "degraded" {
		t.Fatalf("status = %q, want degraded (%s)", res.status, res.body)
	}
	if res.auth {
		t.Fatalf("auth_initialized = true with a closed database (%s)", res.body)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

// TestCloseStopsHTTPBeforeReleasingStore pins the shutdown ordering from issue
// #62: Close must stop the HTTP server and let Serve drain in-flight handlers
// before it releases the store. The request is parked on a gate, and
// RegisterOnShutdown reports when Close reached the graceful-shutdown step, so
// no sleeps are involved.
//
// What it would have caught: the old Close released the store (and nilled DB)
// while the handler was still running, which both violated the ordering and
// triggered the nil dereference in the AuthInitialized closure.
func TestCloseStopsHTTPBeforeReleasingStore(t *testing.T) {
	srv := newTestServer(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	srv.http.Handler = gateHealthHandler(srv.mux, entered, release)

	shutdownStarted := make(chan struct{})
	var once sync.Once
	srv.http.RegisterOnShutdown(func() {
		once.Do(func() { close(shutdownStarted) })
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, serveDone := serveForTest(t, srv, ctx)

	resCh := make(chan healthResult, 1)
	go func() { resCh <- getHealth(base) }()
	waitForSignal(t, entered, "the health request to reach the server")

	closeDone := make(chan error, 1)
	go func() { closeDone <- srv.Close() }()

	select {
	case <-shutdownStarted:
		// Close stopped the HTTP server, as required.
	case err := <-closeDone:
		t.Fatalf("Close returned (%v) without stopping the HTTP server first", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not start the HTTP shutdown")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned (%v) while a /api/health handler was still in flight", err)
	default:
	}

	close(release)
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve: %v", err)
	}

	res := <-resCh
	if res.err != nil {
		t.Fatalf("GET /api/health: %v", res.err)
	}
	if res.code != http.StatusOK {
		t.Fatalf("GET /api/health = %d, want 200 (%s)", res.code, res.body)
	}
	// The response must stay coherent (a JSON payload the SPA can render); the
	// store state itself is asserted separately below, because a shutdown that
	// hits the bounded window may legitimately hand the handler a closed db.
	if res.status != "ok" && res.status != "degraded" {
		t.Fatalf("health status = %q, want ok or degraded (%s)", res.status, res.body)
	}

	// After Close the store is genuinely released: a further query fails, but
	// the handle is still non-nil so no reader can dereference nil.
	if err := srv.store.DB.Ping(); err == nil {
		t.Fatal("store still reachable after Close")
	}
}
