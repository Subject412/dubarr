-- 0002: per-user admin flag and API-token ownership (T3 auth).
--
-- 0001 mirrors IDEA.md §4 verbatim and therefore has neither users.is_admin
-- (single-operator appliance, admin flag added with auth) nor
-- api_tokens.user_id (tokens are now owned by the user that created them).
-- schema.sql intentionally keeps mirroring 0001; this forward migration is the
-- authoritative change.

ALTER TABLE users ADD COLUMN is_admin BOOLEAN NOT NULL DEFAULT 0;

ALTER TABLE api_tokens ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_id);
