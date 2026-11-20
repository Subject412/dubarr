package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sqliteTimeLayout matches SQLite's CURRENT_TIMESTAMP text format, which the
// schema's DATETIME defaults use.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// defaultSessionTTL is how long a session stays valid without re-login.
const defaultSessionTTL = 7 * 24 * time.Hour

// Dependencies is everything the auth feature needs from the rest of the
// appliance. cmd/dubarr constructs it from the store; auth never opens the
// database itself.
type Dependencies struct {
	// DB is the live SQLite handle from internal/store.
	DB *sql.DB
	// SessionTTL overrides the default 7-day session lifetime when > 0.
	SessionTTL time.Duration
	// KeyringAvailable reports whether an OS keyring backend is usable, so
	// /api/auth/status can tell the SPA whether to offer "remember in keyring"
	// style flows. Optional: nil means "not available".
	KeyringAvailable func() bool
}

// User is the authenticated principal exposed in API responses and in the
// request context. It deliberately carries no password hash.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

// Service holds the auth handlers and middleware. Create it once via New or
// RegisterRoutes and share the pointer.
type Service struct {
	db  *sql.DB
	ttl time.Duration
	// keyringAvailable is nil unless the composition root supplies it.
	keyringAvailable func() bool

	mu  sync.RWMutex
	key []byte
}

// New builds the auth service from its dependencies.
func New(deps Dependencies) *Service {
	ttl := deps.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	return &Service{db: deps.DB, ttl: ttl, keyringAvailable: deps.KeyringAvailable}
}

// RegisterRoutes mounts every auth endpoint on mux and returns the service so
// the caller can also use RequireAuth/RequireAdmin on its own routes.
func RegisterRoutes(mux *http.ServeMux, deps Dependencies) *Service {
	s := New(deps)

	mux.HandleFunc("POST /api/auth/setup", s.handleSetup)
	mux.HandleFunc("GET /api/auth/status", s.handleStatus)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.Handle("GET /api/auth/me", s.RequireAuth(http.HandlerFunc(s.handleMe)))

	mux.Handle("GET /api/auth/tokens", s.RequireAuth(http.HandlerFunc(s.handleListTokens)))
	mux.Handle("POST /api/auth/tokens", s.RequireAdmin(http.HandlerFunc(s.handleCreateToken)))
	mux.Handle("DELETE /api/auth/tokens/{id}", s.RequireAdmin(http.HandlerFunc(s.handleDeleteToken)))

	return s
}

// sessionKey returns the cached signing key, loading or creating it on demand.
func (s *Service) sessionKey(ctx context.Context) ([]byte, error) {
	s.mu.RLock()
	key := s.key
	s.mu.RUnlock()
	if len(key) == sessionKeyLen {
		return key, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.key) == sessionKeyLen {
		return s.key, nil
	}
	loaded, err := ensureSessionKey(ctx, s.db)
	if err != nil {
		return nil, err
	}
	s.key = loaded
	return loaded, nil
}

// createSession mints a signed session cookie value for userID and records its
// digest in the sessions table.
func (s *Service) createSession(ctx context.Context, userID int64) (string, time.Time, error) {
	key, err := s.sessionKey(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	payload, err := randomBytes(sessionPayloadLen)
	if err != nil {
		return "", time.Time{}, err
	}
	value := encodeSessionValue(key, payload)
	expires := time.Now().UTC().Add(s.ttl)

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, token_hash, expires_at, last_seen_at) VALUES (?, ?, ?, ?)`,
		userID, hashToken(value), expires.Format(sqliteTimeLayout), time.Now().UTC().Format(sqliteTimeLayout),
	); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: create session: %w", err)
	}
	return value, expires, nil
}

// sessionUser validates a cookie value and returns its owner.
func (s *Service) sessionUser(ctx context.Context, value string) (User, error) {
	key, err := s.sessionKey(ctx)
	if errors.Is(err, errNoSessionKey) {
		return User{}, errUnauthenticated
	}
	if err != nil {
		return User{}, err
	}
	if _, ok := decodeSessionValue(key, value); !ok {
		return User{}, errUnauthenticated
	}

	digest := hashToken(value)
	var (
		user    User
		expires any
		isAdmin any
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT s.expires_at, u.id, u.username, u.is_admin
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ?`, digest,
	).Scan(&expires, &user.ID, &user.Username, &isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errUnauthenticated
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: load session: %w", err)
	}
	user.IsAdmin = toBool(isAdmin)

	expiresAt, err := asTime(expires)
	if err != nil {
		return User{}, err
	}
	if !time.Now().UTC().Before(expiresAt) {
		if _, delErr := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, digest); delErr != nil {
			return User{}, fmt.Errorf("auth: delete expired session: %w", delErr)
		}
		return User{}, errUnauthenticated
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`,
		time.Now().UTC().Format(sqliteTimeLayout), digest,
	); err != nil {
		return User{}, fmt.Errorf("auth: touch session: %w", err)
	}
	return user, nil
}

// tokenUser resolves an API token's plaintext to its owner and records usage.
func (s *Service) tokenUser(ctx context.Context, plaintext string) (User, error) {
	var (
		user    User
		isAdmin any
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.is_admin
		   FROM api_tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.token_hash = ?`, hashToken(plaintext),
	).Scan(&user.ID, &user.Username, &isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, errUnauthenticated
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: load token: %w", err)
	}
	user.IsAdmin = toBool(isAdmin)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?`,
		time.Now().UTC().Format(sqliteTimeLayout), hashToken(plaintext),
	); err != nil {
		return User{}, fmt.Errorf("auth: touch token: %w", err)
	}
	return user, nil
}

// authenticate resolves the caller from either an Authorization: Bearer token
// or the session cookie. Bearer wins when both are present.
func (s *Service) authenticate(r *http.Request) (User, error) {
	ctx := r.Context()

	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok := bearerToken(h); ok {
			return s.tokenUser(ctx, token)
		}
		return User{}, errUnauthenticated
	}

	ck, err := r.Cookie(sessionCookieName)
	if err != nil || ck.Value == "" {
		return User{}, errUnauthenticated
	}
	return s.sessionUser(ctx, ck.Value)
}

// bearerToken extracts the credentials from a "Bearer <token>" header.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// setSessionCookie writes the hardened session cookie. Secure is set only for
// TLS requests so the appliance also works on a plain-HTTP LAN install;
// SameSite=Lax plus the absence of state-changing GETs covers CSRF.
func (s *Service) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// clearSessionCookie expires the session cookie client-side.
func (s *Service) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// asTime normalises the value SQLite hands back for a DATETIME column, which
// may be text or a time.Time depending on the driver and declared type.
func asTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), nil
	case string:
		return parseTimeText(t)
	case []byte:
		return parseTimeText(string(t))
	case nil:
		return time.Time{}, errors.New("auth: nil timestamp")
	default:
		return time.Time{}, fmt.Errorf("auth: unsupported timestamp type %T", v)
	}
}

func parseTimeText(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{sqliteTimeLayout, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("auth: unparseable timestamp %q", s)
}

// timeText renders a timestamp for storage in a DATETIME column.
func timeText(v any) (string, error) {
	t, err := asTime(v)
	if err != nil {
		return "", err
	}
	return t.Format(sqliteTimeLayout), nil
}

// toBool normalises SQLite's 0/1 integers, text and bools.
func toBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case int64:
		return b != 0
	case int:
		return b != 0
	case string:
		return b == "1" || strings.EqualFold(b, "true")
	case []byte:
		return string(b) == "1" || strings.EqualFold(string(b), "true")
	default:
		return false
	}
}
