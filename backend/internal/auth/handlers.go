package auth

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Subject412/dubarr/backend/internal/secrets"
)

const (
	// minPasswordLen is the shortest password accepted at setup.
	minPasswordLen = 8
	// tokenPrefixLen is how many leading characters of a token we store for
	// display, so users can tell tokens apart without revealing them.
	tokenPrefixLen = 12
	// maxBodyBytes bounds JSON request bodies.
	maxBodyBytes = 64 << 10
	// tokenNamespace prefixes generated API tokens.
	tokenNamespace = "dub_"
)

// tokenView is the masked representation of an API token. It never contains the
// plaintext token or its hash.
type tokenView struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

// tokenCreated is tokenView plus the one-time plaintext token.
type tokenCreated struct {
	tokenView
	Token string `json:"token"`
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a {"error": message} body. Messages are generic by design:
// they never include credentials, hashes or keys.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// decodeJSON reads a bounded JSON body into dst, replying 400 on malformed
// input. It reports whether decoding succeeded.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// handleStatus is the public first-run probe used by the setup wizard.
func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	var users int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		writeError(w, http.StatusInternalServerError, "could not read auth state")
		return
	}
	// Probe the request's own credentials: valid session cookie or bearer token.
	_, authErr := s.authenticate(r)
	keyring := false
	if s.keyringAvailable != nil {
		keyring = s.keyringAvailable()
	}
	// The pinned source comes from the settings marker; a failure to read it
	// leaves the field empty rather than breaking the public status probe.
	var keySource string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT value FROM settings WHERE key = ?`, secrets.KeySourceSetting,
	).Scan(&keySource); err != nil && !errors.Is(err, sql.ErrNoRows) {
		keySource = ""
	}
	changed := false
	if s.keySourceChanged != nil {
		changed = s.keySourceChanged()
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Initialized:      users > 0,
		Authenticated:    authErr == nil,
		KeyringAvailable: keyring,
		KeySource:        keySource,
		KeySourceChanged: changed,
	})
}

// statusResponse is the public /api/auth/status contract (IDEA.md): whether the
// first-run admin exists, whether this caller is signed in, whether an OS
// keyring backend is usable, and which master-key source is pinned. It carries
// no user identity or secret material.
type statusResponse struct {
	Initialized      bool   `json:"initialized"`
	Authenticated    bool   `json:"authenticated"`
	KeyringAvailable bool   `json:"keyring_available"`
	KeySource        string `json:"keySource"`
	KeySourceChanged bool   `json:"keySourceChanged"`
}

// handleSetup creates the first administrator exactly once. Any existing user
// blocks it, so it cannot be replayed to add accounts.
func (s *Service) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || len(req.Password) < minPasswordLen {
		writeError(w, http.StatusBadRequest,
			"username is required and password must be at least 8 characters")
		return
	}

	ctx := r.Context()

	// Fail fast: a completed setup must be rejected before paying the Argon2id
	// cost, so replaying setup cannot be used to burn CPU. The transaction
	// below re-checks under the write lock to close the concurrent-setup race.
	var existing int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&existing); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}
	if existing > 0 {
		writeError(w, http.StatusConflict, "setup has already been completed")
		return
	}

	hash, err := hashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Re-check inside the transaction so two concurrent setup calls cannot both
	// create an admin.
	var users int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}
	if users > 0 {
		writeError(w, http.StatusConflict, "setup has already been completed")
		return
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, 1)`, username, hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}

	// createSession also provisions the session signing key on first run.
	value, expires, err := s.createSession(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}
	s.setSessionCookie(w, r, value, expires)

	writeJSON(w, http.StatusCreated, User{ID: id, Username: username, IsAdmin: true})
}

// handleLogin verifies a password and starts a session. Failures are identical
// for unknown users and bad passwords.
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	ctx := r.Context()
	var (
		user     User
		hash     string
		isAdmin  any
		username = strings.TrimSpace(req.Username)
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, is_admin FROM users WHERE username = ?`, username,
	).Scan(&user.ID, &user.Username, &hash, &isAdmin)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Burn comparable CPU time so timing does not reveal account existence.
		if dummy := dummyPasswordHash(); dummy != "" {
			_, _ = VerifyPassword(dummy, req.Password)
		}
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return
	}

	ok, err := VerifyPassword(hash, req.Password)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	user.IsAdmin = toBool(isAdmin)

	value, expires, err := s.createSession(ctx, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}
	s.setSessionCookie(w, r, value, expires)

	writeJSON(w, http.StatusOK, user)
}

// handleLogout revokes the caller's session row and clears the cookie.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookieName); err == nil && ck.Value != "" {
		if _, err := s.db.ExecContext(r.Context(),
			`DELETE FROM sessions WHERE token_hash = ?`, hashToken(ck.Value),
		); err != nil {
			writeError(w, http.StatusInternalServerError, "could not end session")
			return
		}
	}
	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the authenticated principal.
func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// handleListTokens returns the caller's tokens, masked.
func (s *Service) handleListTokens(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, token_prefix, created_at, last_used_at
		   FROM api_tokens WHERE user_id = ? ORDER BY id`, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list tokens")
		return
	}
	defer rows.Close()

	tokens := make([]tokenView, 0, 4)
	for rows.Next() {
		var (
			view    tokenView
			prefix  sql.NullString
			created any
			used    any
		)
		if err := rows.Scan(&view.ID, &view.Name, &prefix, &created, &used); err != nil {
			writeError(w, http.StatusInternalServerError, "could not list tokens")
			return
		}
		view.Prefix = prefix.String
		if view.CreatedAt, err = timeText(created); err != nil {
			writeError(w, http.StatusInternalServerError, "could not list tokens")
			return
		}
		if used != nil {
			text, err := timeText(used)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "could not list tokens")
				return
			}
			view.LastUsedAt = &text
		}
		tokens = append(tokens, view)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not list tokens")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleCreateToken mints a new API token for the caller and returns the
// plaintext exactly once.
func (s *Service) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "token name is required")
		return
	}

	raw, err := randomBytes(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create token")
		return
	}
	plaintext := tokenNamespace + base64.RawURLEncoding.EncodeToString(raw)
	prefix := plaintext
	if len(prefix) > tokenPrefixLen {
		prefix = prefix[:tokenPrefixLen]
	}

	createdAt := time.Now().UTC()
	res, err := s.db.ExecContext(r.Context(),
		`INSERT INTO api_tokens (user_id, name, token_hash, token_prefix, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		user.ID, name, hashToken(plaintext), prefix, createdAt.Format(sqliteTimeLayout),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create token")
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create token")
		return
	}

	writeJSON(w, http.StatusCreated, tokenCreated{
		tokenView: tokenView{
			ID:        id,
			Name:      name,
			Prefix:    prefix,
			CreatedAt: createdAt.Format(sqliteTimeLayout),
		},
		Token: plaintext,
	})
}

// handleDeleteToken revokes one of the caller's tokens.
func (s *Service) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid token id")
		return
	}

	res, err := s.db.ExecContext(r.Context(),
		`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke token")
		return
	}
	affected, err := res.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke token")
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
