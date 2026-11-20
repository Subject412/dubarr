// Package auth implements Dubarr's single-operator authentication: Argon2id
// password hashing, HMAC-signed HttpOnly session cookies, SHA-256-hashed API
// tokens and the RequireAuth/RequireAdmin middleware.
//
// The package owns no router of its own: cmd/dubarr mounts it with
// RegisterRoutes(mux, Dependencies{...}) per the AGENTS.md route contract.
//
// Security notes:
//   - Passwords are stored as Argon2id PHC strings (m=65536, t=1, p=4) and
//     verified with a constant-time comparison.
//   - Session cookies carry an opaque 32-byte payload signed with
//     HMAC-SHA256. Only SHA-256(cookie value) is persisted, so a database leak
//     cannot be replayed as a login. The signing key is generated once and kept
//     in the settings table under "auth.session_key" (the DB is 0600); the
//     sanitized settings export must exclude that key.
//   - API tokens are shown exactly once at creation; only their SHA-256 digest
//     and a display prefix are stored.
//   - Login failures are indistinguishable (no user enumeration), and no
//     secret is ever logged or echoed in an error body.
package auth
