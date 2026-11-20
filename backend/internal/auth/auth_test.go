package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/store"
)

// testClient is a minimal HTTP client that owns a single session cookie so
// tests can tamper with, expire and drop it deliberately.
type testClient struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
	http   *http.Client
}

func newTestServer(t *testing.T) (*httptest.Server, *Service, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	mux := http.NewServeMux()
	svc := RegisterRoutes(mux, Dependencies{DB: st.DB})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, svc, st
}

func (c *testClient) do(method, path string, body any) (int, []byte, *http.Response) {
	c.t.Helper()

	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		c.t.Fatalf("read body: %v", err)
	}

	for _, ck := range resp.Cookies() {
		if ck.Name != sessionCookieName {
			continue
		}
		if ck.MaxAge < 0 || ck.Value == "" {
			c.cookie = nil
			continue
		}
		c.cookie = ck
	}
	return resp.StatusCode, buf.Bytes(), resp
}

func (c *testClient) setupAdmin() {
	c.t.Helper()
	code, body, _ := c.do(http.MethodPost, "/api/auth/setup", map[string]string{
		"username": "admin",
		"password": "correct horse battery",
	})
	if code != http.StatusCreated {
		c.t.Fatalf("setup: status = %d, body = %s", code, body)
	}
	if c.cookie == nil {
		c.t.Fatal("setup did not set a session cookie")
	}
}

func decodeBody(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
}

// --- password hashing -------------------------------------------------------

func TestPasswordHashVerifyRoundtrip(t *testing.T) {
	const pw = "s3cret-pässword"

	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=1,p=4$") {
		t.Fatalf("hash has unexpected PHC encoding: %q", hash)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("hash leaks the plaintext password")
	}

	ok, err := VerifyPassword(hash, pw)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("correct password rejected")
	}

	ok, err = VerifyPassword(hash, "wrong-password")
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): %v", err)
	}
	if ok {
		t.Fatal("wrong password accepted")
	}

	// Salts are random: hashing the same input twice must differ.
	other, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if other == hash {
		t.Fatal("two hashes of the same password are identical (salt not random)")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, enc := range []string{"", "plaintext", "$argon2id$v=19$m=65536,t=1,p=4$notbase64$x"} {
		if ok, err := VerifyPassword(enc, "whatever"); ok || err == nil {
			t.Errorf("VerifyPassword(%q) = (%v, %v), want (false, error)", enc, ok, err)
		}
	}
}

// --- setup ------------------------------------------------------------------

func TestStatusReportsSetupRequired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}

	code, body, _ := c.do(http.MethodGet, "/api/auth/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status endpoint: %d %s", code, body)
	}
	var st struct {
		Initialized bool `json:"initialized"`
	}
	decodeBody(t, body, &st)
	if st.Initialized {
		t.Fatal("fresh database should not be initialized")
	}
}

func TestSetupCreatesAdminAndIsSetupOnce(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}

	c.setupAdmin()

	code, body, _ := c.do(http.MethodGet, "/api/auth/me", nil)
	if code != http.StatusOK {
		t.Fatalf("me after setup: %d %s", code, body)
	}
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		IsAdmin  bool   `json:"is_admin"`
	}
	decodeBody(t, body, &me)
	if me.Username != "admin" || !me.IsAdmin {
		t.Fatalf("me = %+v, want admin with is_admin", me)
	}

	var isAdmin int
	if err := st.DB.QueryRow(`SELECT is_admin FROM users WHERE id = ?`, me.ID).Scan(&isAdmin); err != nil {
		t.Fatalf("query is_admin: %v", err)
	}
	if isAdmin != 1 {
		t.Fatalf("users.is_admin = %d, want 1", isAdmin)
	}

	// Second setup attempt must be rejected and must not create another user.
	other := &testClient{t: t, base: srv.URL, http: srv.Client()}
	code, body, _ = other.do(http.MethodPost, "/api/auth/setup", map[string]string{
		"username": "intruder",
		"password": "another password",
	})
	if code < 400 || code >= 500 {
		t.Fatalf("second setup: status = %d, body = %s, want 4xx", code, body)
	}

	var users int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("users = %d after rejected setup, want 1", users)
	}

	code, body, _ = c.do(http.MethodGet, "/api/auth/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, body)
	}
	var stt struct {
		Initialized bool `json:"initialized"`
	}
	decodeBody(t, body, &stt)
	if !stt.Initialized {
		t.Fatal("initialized should be true after setup")
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}

	code, body, _ := c.do(http.MethodPost, "/api/auth/setup", map[string]string{
		"username": "admin",
		"password": "short",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("short password: status = %d, body = %s, want 400", code, body)
	}
	var users int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatalf("users = %d after rejected setup, want 0", users)
	}
}

// --- login / logout ---------------------------------------------------------

func TestLoginSetsHardenedCookie(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	// Log out first so the login response is the one setting the cookie.
	if code, body, _ := c.do(http.MethodPost, "/api/auth/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", code, body)
	}

	code, body, resp := c.do(http.MethodPost, "/api/auth/login", map[string]string{
		"username": "admin",
		"password": "correct horse battery",
	})
	if code != http.StatusOK {
		t.Fatalf("login: %d %s", code, body)
	}

	var ck *http.Cookie
	for _, cand := range resp.Cookies() {
		if cand.Name == sessionCookieName {
			ck = cand
		}
	}
	if ck == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !ck.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", ck.SameSite)
	}
	if ck.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", ck.Path)
	}
	if ck.Secure {
		t.Error("session cookie must not set Secure over plain HTTP")
	}
	if len(ck.Value) < 40 {
		t.Errorf("session cookie value %q looks too short", ck.Value)
	}
}

func TestLoginRejectsWrongPasswordAndUnknownUser(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	cases := []struct{ name, user, pass string }{
		{"wrong password", "admin", "not the password"},
		{"unknown user", "nobody", "correct horse battery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, resp := c.do(http.MethodPost, "/api/auth/login", map[string]string{
				"username": tc.user,
				"password": tc.pass,
			})
			if code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s, want 401", code, body)
			}
			if strings.Contains(string(body), "password") || strings.Contains(string(body), "user") {
				t.Errorf("error body leaks credential detail: %s", body)
			}
			for _, ck := range resp.Cookies() {
				if ck.Name == sessionCookieName && ck.Value != "" {
					t.Error("failed login set a session cookie")
				}
			}
		})
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	code, body, _ := c.do(http.MethodPost, "/api/auth/logout", nil)
	if code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", code, body)
	}

	var sessions int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("sessions = %d after logout, want 0", sessions)
	}
	if c.cookie != nil {
		t.Fatal("logout did not clear the session cookie client-side")
	}

	// Replaying the old cookie must fail.
	code, _, _ = c.do(http.MethodGet, "/api/auth/me", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d, want 401", code)
	}
}

// --- session validation -----------------------------------------------------

func TestMeRequiresAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}

	code, body, _ := c.do(http.MethodGet, "/api/auth/me", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("me without cookie: %d %s, want 401", code, body)
	}
}

func TestSessionRejectsTamperedSignature(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	tampered := *c.cookie
	v := []byte(tampered.Value)
	last := v[len(v)-1]
	if last == 'A' {
		v[len(v)-1] = 'B'
	} else {
		v[len(v)-1] = 'A'
	}
	tampered.Value = string(v)
	c.cookie = &tampered

	code, body, _ := c.do(http.MethodGet, "/api/auth/me", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie: %d %s, want 401", code, body)
	}

	var sessions int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("tampered request altered sessions: %d rows, want 1", sessions)
	}
}

func TestSessionRejectsExpired(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	past := time.Now().UTC().Add(-time.Minute).Format("2006-01-02 15:04:05")
	if _, err := st.DB.Exec(`UPDATE sessions SET expires_at = ?`, past); err != nil {
		t.Fatalf("expire session: %v", err)
	}

	code, body, _ := c.do(http.MethodGet, "/api/auth/me", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("expired cookie: %d %s, want 401", code, body)
	}
}

func TestSessionKeyPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	key1, err := ensureSessionKey(t.Context(), st.DB)
	if err != nil {
		t.Fatalf("ensureSessionKey: %v", err)
	}
	if len(key1) != 32 {
		t.Fatalf("session key length = %d, want 32", len(key1))
	}
	key2, err := ensureSessionKey(t.Context(), st.DB)
	if err != nil {
		t.Fatalf("ensureSessionKey (second): %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if !bytes.Equal(key1, key2) {
		t.Fatal("session key changed between calls")
	}

	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	key3, err := ensureSessionKey(t.Context(), st2.DB)
	if err != nil {
		t.Fatalf("ensureSessionKey after reopen: %v", err)
	}
	if !bytes.Equal(key1, key3) {
		t.Fatal("session key did not survive a store reopen")
	}
}

// --- middleware -------------------------------------------------------------

func TestRequireAuthAndRequireAdmin(t *testing.T) {
	srv, svc, st := newTestServer(t)
	mux := srv.Config.Handler.(*http.ServeMux)

	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFromContext(r.Context())
		if !ok {
			t.Error("RequireAuth did not put a user in the context")
		}
		_ = u
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /api/private", svc.RequireAuth(protected))
	mux.Handle("GET /api/admin-only", svc.RequireAdmin(protected))

	anon := &testClient{t: t, base: srv.URL, http: srv.Client()}
	if code, body, _ := anon.do(http.MethodGet, "/api/private", nil); code != http.StatusUnauthorized {
		t.Fatalf("anon private: %d %s, want 401", code, body)
	}

	admin := &testClient{t: t, base: srv.URL, http: srv.Client()}
	admin.setupAdmin()
	if code, body, _ := admin.do(http.MethodGet, "/api/private", nil); code != http.StatusNoContent {
		t.Fatalf("admin private: %d %s, want 204", code, body)
	}
	if code, body, _ := admin.do(http.MethodGet, "/api/admin-only", nil); code != http.StatusNoContent {
		t.Fatalf("admin admin-only: %d %s, want 204", code, body)
	}

	// A non-admin user must pass RequireAuth but fail RequireAdmin.
	hash, err := HashPassword("viewer password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	res, err := st.DB.Exec(`INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, 0)`,
		"viewer", hash)
	if err != nil {
		t.Fatalf("insert viewer: %v", err)
	}
	if _, err := res.LastInsertId(); err != nil {
		t.Fatalf("viewer id: %v", err)
	}

	viewer := &testClient{t: t, base: srv.URL, http: srv.Client()}
	code, body, _ := viewer.do(http.MethodPost, "/api/auth/login", map[string]string{
		"username": "viewer",
		"password": "viewer password",
	})
	if code != http.StatusOK {
		t.Fatalf("viewer login: %d %s", code, body)
	}
	if code, body, _ := viewer.do(http.MethodGet, "/api/private", nil); code != http.StatusNoContent {
		t.Fatalf("viewer private: %d %s, want 204", code, body)
	}
	if code, body, _ := viewer.do(http.MethodGet, "/api/admin-only", nil); code != http.StatusForbidden {
		t.Fatalf("viewer admin-only: %d %s, want 403", code, body)
	}
	if code, body, _ := viewer.do(http.MethodPost, "/api/auth/tokens", map[string]string{"name": "x"}); code != http.StatusForbidden {
		t.Fatalf("viewer token create: %d %s, want 403", code, body)
	}
}

// --- API tokens -------------------------------------------------------------

func TestAPITokenLifecycle(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	code, body, _ := c.do(http.MethodPost, "/api/auth/tokens", map[string]string{"name": "cli"})
	if code != http.StatusCreated {
		t.Fatalf("create token: %d %s", code, body)
	}
	var created struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
	}
	decodeBody(t, body, &created)
	if created.Token == "" {
		t.Fatal("create response did not include the plaintext token")
	}
	if created.Name != "cli" {
		t.Fatalf("token name = %q, want cli", created.Name)
	}

	// Only the SHA-256 digest is persisted.
	var storedHash, prefix string
	if err := st.DB.QueryRow(`SELECT token_hash, token_prefix FROM api_tokens WHERE id = ?`, created.ID).
		Scan(&storedHash, &prefix); err != nil {
		t.Fatalf("query token row: %v", err)
	}
	if storedHash == created.Token || strings.Contains(storedHash, created.Token) {
		t.Fatal("plaintext token stored in the database")
	}
	if len(storedHash) != 64 {
		t.Fatalf("token_hash = %q, want a 64-char hex SHA-256 digest", storedHash)
	}
	if want := hashToken(created.Token); storedHash != want {
		t.Fatalf("token_hash = %q, want %q", storedHash, want)
	}
	if prefix == "" || !strings.HasPrefix(created.Token, prefix) {
		t.Fatalf("token_prefix = %q, want a prefix of the token", prefix)
	}

	// Listing is masked: no plaintext, no hash.
	code, body, _ = c.do(http.MethodGet, "/api/auth/tokens", nil)
	if code != http.StatusOK {
		t.Fatalf("list tokens: %d %s", code, body)
	}
	var list struct {
		Tokens []struct {
			ID        int64   `json:"id"`
			Name      string  `json:"name"`
			Prefix    string  `json:"prefix"`
			CreatedAt string  `json:"created_at"`
			LastUsed  *string `json:"last_used_at"`
		} `json:"tokens"`
	}
	decodeBody(t, body, &list)
	if len(list.Tokens) != 1 || list.Tokens[0].ID != created.ID {
		t.Fatalf("token list = %+v, want the created token", list.Tokens)
	}
	if strings.Contains(string(body), created.Token) {
		t.Fatal("token list leaks the plaintext token")
	}
	if strings.Contains(string(body), storedHash) {
		t.Fatal("token list leaks the token hash")
	}

	// Bearer auth works without any session cookie.
	bearer := &testClient{t: t, base: srv.URL, http: srv.Client()}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/me", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)
	resp, err := bearer.http.Do(req)
	if err != nil {
		t.Fatalf("bearer request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer me: status = %d, want 200", resp.StatusCode)
	}

	// A bogus bearer token is rejected.
	badReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/me", nil)
	badReq.Header.Set("Authorization", "Bearer dub_not_a_real_token")
	badResp, err := bearer.http.Do(badReq)
	if err != nil {
		t.Fatalf("bogus bearer request: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus bearer: status = %d, want 401", badResp.StatusCode)
	}

	// Revocation: delete then reuse must fail.
	code, body, _ = c.do(http.MethodDelete, "/api/auth/tokens/"+strconv.FormatInt(created.ID, 10), nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete token: %d %s", code, body)
	}

	replay, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/me", nil)
	replay.Header.Set("Authorization", "Bearer "+created.Token)
	replayResp, err := bearer.http.Do(replay)
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}
	replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked bearer: status = %d, want 401", replayResp.StatusCode)
	}

	var remaining int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_tokens`).Scan(&remaining); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("api_tokens rows = %d after revoke, want 0", remaining)
	}
}

func TestTokenCreateRequiresName(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	code, body, _ := c.do(http.MethodPost, "/api/auth/tokens", map[string]string{"name": "  "})
	if code != http.StatusBadRequest {
		t.Fatalf("empty name: %d %s, want 400", code, body)
	}
}

func TestTokenListRequiresAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}

	code, body, _ := c.do(http.MethodGet, "/api/auth/tokens", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("anon token list: %d %s, want 401", code, body)
	}
}
