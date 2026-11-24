package auth

import (
	"net/http"
	"strings"
	"testing"
)

// changePasswordBody is the request shape shared by the password-change tests.
func changePasswordBody(current, next string) map[string]string {
	return map[string]string{"current_password": current, "new_password": next}
}

func TestChangePasswordRequiresAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}

	code, body, _ := c.do(http.MethodPost, "/api/auth/password",
		changePasswordBody("irrelevant", "brand new password"))
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous change password: status = %d, body = %s, want 401", code, body)
	}
}

func TestChangePasswordRejectsWrongCurrent(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	var before string
	if err := st.DB.QueryRow(`SELECT password_hash FROM users WHERE username = 'admin'`).Scan(&before); err != nil {
		t.Fatalf("read hash: %v", err)
	}

	code, body, _ := c.do(http.MethodPost, "/api/auth/password",
		changePasswordBody("not the current password", "another good password"))
	if code != http.StatusForbidden {
		t.Fatalf("wrong current password: status = %d, body = %s, want 403", code, body)
	}
	if strings.Contains(string(body), "not the current password") {
		t.Fatalf("error body echoes the submitted password: %s", body)
	}

	var after string
	if err := st.DB.QueryRow(`SELECT password_hash FROM users WHERE username = 'admin'`).Scan(&after); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if after != before {
		t.Fatal("rejected change still rewrote the stored hash")
	}

	// The original password must still work.
	code, body, _ = c.do(http.MethodGet, "/api/auth/me", nil)
	if code != http.StatusOK {
		t.Fatalf("me after rejected change: %d %s, want 200", code, body)
	}
}

func TestChangePasswordValidatesNewPassword(t *testing.T) {
	srv, _, st := newTestServer(t)
	c := &testClient{t: t, base: srv.URL, http: srv.Client()}
	c.setupAdmin()

	var before string
	if err := st.DB.QueryRow(`SELECT password_hash FROM users WHERE username = 'admin'`).Scan(&before); err != nil {
		t.Fatalf("read hash: %v", err)
	}

	cases := []struct {
		name string
		body any
	}{
		{"empty new password", changePasswordBody("correct horse battery", "")},
		{"short new password", changePasswordBody("correct horse battery", "short")},
		{"new equals current", changePasswordBody("correct horse battery", "correct horse battery")},
		{"missing current password", changePasswordBody("", "another good password")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, _ := c.do(http.MethodPost, "/api/auth/password", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400", code, body)
			}
			var after string
			if err := st.DB.QueryRow(`SELECT password_hash FROM users WHERE username = 'admin'`).Scan(&after); err != nil {
				t.Fatalf("read hash: %v", err)
			}
			if after != before {
				t.Fatal("invalid change rewrote the stored hash")
			}
		})
	}
}

func TestChangePasswordKeepsCallerAndRevokesOtherSessions(t *testing.T) {
	srv, _, st := newTestServer(t)
	first := &testClient{t: t, base: srv.URL, http: srv.Client()}
	first.setupAdmin()

	// An API token minted before the change must survive it: tokens are not
	// browser sessions.
	code, body, _ := first.do(http.MethodPost, "/api/auth/tokens", map[string]string{"name": "cli"})
	if code != http.StatusCreated {
		t.Fatalf("create token: %d %s", code, body)
	}
	var created struct {
		Token string `json:"token"`
	}
	decodeBody(t, body, &created)

	// A second browser signs in, so the account now owns two sessions.
	second := &testClient{t: t, base: srv.URL, http: srv.Client()}
	code, body, _ = second.do(http.MethodPost, "/api/auth/login", map[string]string{
		"username": "admin",
		"password": "correct horse battery",
	})
	if code != http.StatusOK {
		t.Fatalf("second login: %d %s", code, body)
	}

	var sessions int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 2 {
		t.Fatalf("sessions = %d before change, want 2", sessions)
	}

	code, body, _ = first.do(http.MethodPost, "/api/auth/password",
		changePasswordBody("correct horse battery", "a brand new password"))
	if code != http.StatusNoContent {
		t.Fatalf("change password: %d %s, want 204", code, body)
	}

	// The caller's own session stays valid...
	if code, body, _ := first.do(http.MethodGet, "/api/auth/me", nil); code != http.StatusOK {
		t.Fatalf("caller me after change: %d %s, want 200", code, body)
	}
	// ...while every other session is revoked.
	if code, body, _ := second.do(http.MethodGet, "/api/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("other session after change: %d %s, want 401", code, body)
	}

	var remaining int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&remaining); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("sessions = %d after change, want only the caller's", remaining)
	}
	var kept int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`,
		hashToken(first.cookie.Value)).Scan(&kept); err != nil {
		t.Fatalf("count caller session: %v", err)
	}
	if kept != 1 {
		t.Fatal("the caller's own session row was revoked")
	}

	// The new password is the one that signs in now.
	third := &testClient{t: t, base: srv.URL, http: srv.Client()}
	if code, _, _ := third.do(http.MethodPost, "/api/auth/login", map[string]string{
		"username": "admin",
		"password": "correct horse battery",
	}); code != http.StatusUnauthorized {
		t.Fatal("old password still signs in after a successful change")
	}
	if code, body, _ := third.do(http.MethodPost, "/api/auth/login", map[string]string{
		"username": "admin",
		"password": "a brand new password",
	}); code != http.StatusOK {
		t.Fatalf("new password login: %d %s, want 200", code, body)
	}

	// The API token is unaffected by the password change.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/me", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+created.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("bearer request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer me after change: %d, want 200", resp.StatusCode)
	}

	var stored string
	if err := st.DB.QueryRow(`SELECT password_hash FROM users WHERE username = 'admin'`).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if strings.Contains(stored, "a brand new password") {
		t.Fatal("stored hash leaks the plaintext password")
	}
	if ok, err := VerifyPassword(stored, "a brand new password"); err != nil || !ok {
		t.Fatalf("stored hash does not verify the new password: ok = %v, err = %v", ok, err)
	}
}
