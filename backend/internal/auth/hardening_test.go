package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// --- strict PHC parsing -----------------------------------------------------

func TestVerifyPasswordRejectsMalformedPHCStrictly(t *testing.T) {
	const (
		salt = "c2FsdHNhbHRzYWx0c2FsdA" // 16 bytes, base64 raw
		hash = "aGFzaGhhc2hoYXNoaGFzaA"
	)
	cases := map[string]string{
		"unknown variant argon2i": "$argon2i$v=19$m=65536,t=1,p=4$" + salt + "$" + hash,
		"unknown variant argon2d": "$argon2d$v=19$m=65536,t=1,p=4$" + salt + "$" + hash,
		"unknown variant scrypt":  "$scrypt$v=19$m=65536,t=1,p=4$" + salt + "$" + hash,
		"version with trailing":   "$argon2id$v=19x$m=65536,t=1,p=4$" + salt + "$" + hash,
		"version with prefix":     "$argon2id$v=019$m=65536,t=1,p=4$" + salt + "$" + hash,
		"params trailing field":   "$argon2id$v=19$m=65536,t=1,p=4,x=1$" + salt + "$" + hash,
		"params trailing comma":   "$argon2id$v=19$m=65536,t=1,p=4,$" + salt + "$" + hash,
		"params wrong separator":  "$argon2id$v=19$m=65536;t=1;p=4$" + salt + "$" + hash,
		"params whitespace":       "$argon2id$v=19$m=65536, t=1, p=4$" + salt + "$" + hash,
		"params missing p":        "$argon2id$v=19$m=65536,t=1$" + salt + "$" + hash,
		"params reordered":        "$argon2id$v=19$t=1,m=65536,p=4$" + salt + "$" + hash,
		"params negative memory":  "$argon2id$v=19$m=-65536,t=1,p=4$" + salt + "$" + hash,
		"params zero memory":      "$argon2id$v=19$m=0,t=1,p=4$" + salt + "$" + hash,
		"params zero threads":     "$argon2id$v=19$m=65536,t=1,p=0$" + salt + "$" + hash,
		"empty salt":              "$argon2id$v=19$m=65536,t=1,p=4$$" + hash,
		"empty hash":              "$argon2id$v=19$m=65536,t=1,p=4$" + salt + "$",
		"invalid salt base64":     "$argon2id$v=19$m=65536,t=1,p=4$!!!!$" + hash,
		"invalid hash base64":     "$argon2id$v=19$m=65536,t=1,p=4$" + salt + "$!!!!",
		"extra field":             "$argon2id$v=19$m=65536,t=1,p=4$" + salt + "$" + hash + "$x",
	}
	for name, enc := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword(enc, "whatever")
			if ok {
				t.Fatalf("VerifyPassword accepted a malformed hash: %q", enc)
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("VerifyPassword error = %v, want ErrInvalidHash", err)
			}
		})
	}
}

// buildPHC renders a PHC string from explicit parameters, used to feed
// VerifyPassword parameters no honest HashPassword call would ever produce.
func buildPHC(memory, iterations uint32, threads uint8, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, iterations, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func TestVerifyPasswordRejectsAbsurdParameters(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key := []byte("0123456789abcdef0123456789abcdef")
	cases := map[string]string{
		"memory over cap":     buildPHC(1024*1024, 1, 4, salt, key),       // 1 GiB
		"iterations over cap": buildPHC(64*1024, 1_000_000, 4, salt, key), // ~1e6 passes
		"threads over cap":    buildPHC(64*1024, 1, 255, salt, key),       // 255 lanes
		"salt over cap":       buildPHC(64*1024, 1, 4, make([]byte, 4096), key),
		"key over cap":        buildPHC(64*1024, 1, 4, salt, make([]byte, 4096)),
	}
	for name, enc := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			ok, err := VerifyPassword(enc, "whatever")
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("VerifyPassword took %v: parameters were not rejected up front", elapsed)
			}
			if ok {
				t.Fatal("VerifyPassword accepted an out-of-range hash")
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("VerifyPassword error = %v, want ErrInvalidHash", err)
			}
		})
	}
}

func TestVerifyPasswordAcceptsRaisedButSaneParameters(t *testing.T) {
	// Higher cost than the defaults, still inside the caps: verification must
	// keep working so cost parameters can be raised without breaking logins.
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("raised cost password"), salt, 3, 128*1024, 8, 32)
	enc := buildPHC(128*1024, 3, 8, salt, key)

	ok, err := VerifyPassword(enc, "raised cost password")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("a hash with raised-but-sane parameters was rejected")
	}
}

// --- cookie handling --------------------------------------------------------

func TestMalformedSessionCookieRejected(t *testing.T) {
	srv, _, st := newTestServer(t)
	admin := &testClient{t: t, base: srv.URL, http: srv.Client()}
	admin.setupAdmin()

	junk := []string{
		"",
		"plaintext",
		".",
		"a.b",
		"!!!.###",
		strings.Repeat("A", 500),
		"YWFh.YWFh",      // valid base64, wrong payload length + bad MAC
		"YWFh.YWFh.YWFh", // too many segments
		admin.cookie.Value + ".extra",
	}

	for _, value := range junk {
		t.Run(fmt.Sprintf("%.12q", value), func(t *testing.T) {
			c := &testClient{t: t, base: srv.URL, http: srv.Client()}
			c.cookie = &http.Cookie{Name: sessionCookieName, Value: value}
			code, body, _ := c.do(http.MethodGet, "/api/auth/me", nil)
			if code != http.StatusUnauthorized {
				t.Fatalf("cookie %q: status = %d, body = %s, want 401", value, code, body)
			}
		})
	}

	var sessions int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("sessions = %d after malformed cookies, want 1 (the admin's)", sessions)
	}
}

// --- session lifecycle ------------------------------------------------------

func TestExpiredSessionRowIsDeleted(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	past := time.Now().UTC().Add(-time.Minute).Format(sqliteTimeLayout)
	if _, err := st.DB.Exec(`UPDATE sessions SET expires_at = ?`, past); err != nil {
		t.Fatalf("expire session: %v", err)
	}

	code, body, _ := c.do(http.MethodGet, "/api/auth/me", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("expired session: %d %s, want 401", code, body)
	}

	var sessions int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("sessions = %d after an expired-session hit, want the row deleted", sessions)
	}
}

func TestLogoutRevokesOnlyOwnSession(t *testing.T) {
	srv, _, st := newTestServer(t)
	admin := &testClient{t: t, base: srv.URL, http: srv.Client()}
	admin.setupAdmin()

	// A second principal with its own live session.
	hash, err := HashPassword("second admin password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := st.DB.Exec(
		`INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, 1)`, "second", hash); err != nil {
		t.Fatalf("insert second admin: %v", err)
	}
	other := &testClient{t: t, base: srv.URL, http: srv.Client()}
	code, body, _ := other.do(http.MethodPost, "/api/auth/login", map[string]string{
		"username": "second", "password": "second admin password",
	})
	if code != http.StatusOK {
		t.Fatalf("second login: %d %s", code, body)
	}

	if code, body, _ := admin.do(http.MethodPost, "/api/auth/logout", nil); code != http.StatusNoContent {
		t.Fatalf("admin logout: %d %s", code, body)
	}

	// The other user's session must survive another user's logout.
	if code, body, _ := other.do(http.MethodGet, "/api/auth/me", nil); code != http.StatusOK {
		t.Fatalf("second user after admin logout: %d %s, want 200", code, body)
	}
	var sessions int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("sessions = %d after admin logout, want the other user's session left intact", sessions)
	}
}

// --- cross-user token revocation -------------------------------------------

func TestCrossUserTokenRevokeRejected(t *testing.T) {
	srv, _, st := newTestServer(t)
	owner := &testClient{t: t, base: srv.URL, http: srv.Client()}
	owner.setupAdmin()

	code, body, _ := owner.do(http.MethodPost, "/api/auth/tokens", map[string]string{"name": "owner-token"})
	if code != http.StatusCreated {
		t.Fatalf("create owner token: %d %s", code, body)
	}
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	decodeBody(t, body, &created)

	// A second administrator must not be able to revoke another user's token.
	hash, err := HashPassword("intruder admin password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := st.DB.Exec(
		`INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, 1)`, "intruder", hash); err != nil {
		t.Fatalf("insert intruder: %v", err)
	}
	intruder := &testClient{t: t, base: srv.URL, http: srv.Client()}
	code, body, _ = intruder.do(http.MethodPost, "/api/auth/login", map[string]string{
		"username": "intruder", "password": "intruder admin password",
	})
	if code != http.StatusOK {
		t.Fatalf("intruder login: %d %s", code, body)
	}

	code, body, _ = intruder.do(http.MethodDelete, fmt.Sprintf("/api/auth/tokens/%d", created.ID), nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-user delete: %d %s, want 404", code, body)
	}

	// The owner's token must still authenticate.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/me", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("bearer request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner token after cross-user delete: %d, want 200", resp.StatusCode)
	}
}

// --- setup race / fail-fast -------------------------------------------------

func TestConcurrentSetupCreatesExactlyOneAdmin(t *testing.T) {
	srv, _, st := newTestServer(t)

	const attempts = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		codes   = make(map[int]int)
	)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := &testClient{t: t, base: srv.URL, http: srv.Client()}
			<-start
			code, body, _ := c.do(http.MethodPost, "/api/auth/setup", map[string]string{
				"username": fmt.Sprintf("admin%d", i),
				"password": "concurrent setup password",
			})
			mu.Lock()
			codes[code]++
			if code == http.StatusCreated {
				created++
			}
			mu.Unlock()
			if code != http.StatusCreated && code != http.StatusConflict {
				t.Errorf("setup %d: status = %d, body = %s, want 201 or 409", i, code, body)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if created != 1 {
		t.Fatalf("successful setups = %d (codes %v), want exactly 1", created, codes)
	}
	var users int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("users = %d after the setup race, want 1", users)
	}
}

func TestSetupFailsFastBeforeHashingWhenAdminExists(t *testing.T) {
	srv, _, st := newTestServer(t)
	admin := &testClient{t: t, base: srv.URL, http: srv.Client()}
	admin.setupAdmin()

	original := hashPassword
	var hashed int
	hashPassword = func(string) (string, error) {
		hashed++
		return "", errors.New("setup must not hash once an admin exists")
	}
	t.Cleanup(func() { hashPassword = original })

	replay := &testClient{t: t, base: srv.URL, http: srv.Client()}
	code, body, _ := replay.do(http.MethodPost, "/api/auth/setup", map[string]string{
		"username": "intruder",
		"password": "another password",
	})
	if code != http.StatusConflict {
		t.Fatalf("replayed setup: %d %s, want 409", code, body)
	}
	if hashed != 0 {
		t.Fatalf("setup hashed the password %d time(s) before rejecting a completed setup", hashed)
	}

	var users int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("users = %d, want 1", users)
	}
}
