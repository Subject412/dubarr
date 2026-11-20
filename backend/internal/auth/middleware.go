package auth

import (
	"context"
	"errors"
	"net/http"
)

// errUnauthenticated is the single failure mode for "no valid credential";
// callers never learn which check failed.
var errUnauthenticated = errors.New("auth: unauthenticated")

type contextKey struct{}

// UserFromContext returns the principal placed there by RequireAuth.
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(contextKey{}).(User)
	return u, ok
}

// RequireAuth rejects unauthenticated requests with a JSON 401 and otherwise
// continues with the resolved user in the request context.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.authenticate(r)
		if err != nil {
			if errors.Is(err, errUnauthenticated) {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			writeError(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, user)))
	})
}

// RequireAdmin enforces RequireAuth and then an admin principal, responding 403
// to authenticated non-admins.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return s.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !user.IsAdmin {
			writeError(w, http.StatusForbidden, "administrator privileges required")
			return
		}
		next.ServeHTTP(w, r)
	}))
}
