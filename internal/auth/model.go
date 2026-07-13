package auth

import (
	"time"

	"hify/internal/user"
)

// Session is what a successful login/refresh produces. RefreshToken is the
// raw (unhashed) value — only the handler ever sees it, to set it as an
// httpOnly cookie; it's never persisted or returned in a JSON body.
type Session struct {
	AccessToken  string
	RefreshToken string
	RefreshTTL   time.Duration
	User         user.User
}

// refreshTokenRecord is the repository-internal shape returned by lookups —
// service.go only ever needs the token's id (to rotate/revoke it) and the
// owning user's id, never the hash or timestamps.
type refreshTokenRecord struct {
	ID     string
	UserID string
}
