package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"hify/internal/platform/jwt"
	"hify/internal/platform/ratelimit"
	"hify/internal/user"
)

const (
	loginRateLimitAttempts = 10
	loginRateLimitWindow   = 15 * time.Minute

	// refreshTokenRevokedRetention is how long a revoked refresh_tokens
	// row is kept before CleanupExpiredRefreshTokens purges it — long
	// enough to still be useful for a quick audit look-back, short enough
	// not to let the table grow unbounded.
	refreshTokenRevokedRetention = 24 * time.Hour
)

// Service is auth's public contract.
type Service interface {
	Login(ctx context.Context, email, password, clientIP string) (Session, error)
	Refresh(ctx context.Context, rawRefreshToken string) (Session, error)
	Logout(ctx context.Context, rawRefreshToken string) error
	// CleanupExpiredRefreshTokens purges refresh_tokens rows that no
	// longer need to be kept — see tasks.go's periodic task, registered
	// by cmd/hify/main.go, which is the only caller.
	CleanupExpiredRefreshTokens(ctx context.Context) (int64, error)
}

// service is constructed via NewService in wire.go.
type service struct {
	repo       *Repository
	users      user.Service
	redis      *redis.Client
	jwtSecret  string
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func (s *service) Login(ctx context.Context, email, password, clientIP string) (Session, error) {
	// Keyed on ip+email together: an attacker spraying one password across
	// many accounts from one IP is capped by the IP; someone hammering one
	// account from many IPs is capped by the email. Either alone misses one
	// of those two shapes.
	key := "login:" + clientIP + ":" + email
	allowed, err := ratelimit.Allow(ctx, s.redis, key, loginRateLimitAttempts, loginRateLimitWindow)
	if err != nil {
		return Session{}, fmt.Errorf("auth: rate limit check: %w", err)
	}
	if !allowed {
		return Session{}, ErrTooManyAttempts
	}

	u, err := s.users.VerifyPassword(ctx, email, password)
	if err != nil {
		return Session{}, err
	}

	return s.issueSession(ctx, u)
}

func (s *service) Refresh(ctx context.Context, rawRefreshToken string) (Session, error) {
	record, err := s.repo.getValidByToken(ctx, rawRefreshToken)
	if err != nil {
		return Session{}, err
	}

	u, err := s.users.GetByID(ctx, record.UserID)
	if err != nil {
		return Session{}, err
	}
	if !u.IsActive {
		// Mirror user.VerifyPassword's check: a refresh token issued before
		// the account was deactivated must stop minting access tokens, not
		// just be rejected at the next login.
		return Session{}, ErrInactiveUser
	}

	newRaw, err := newOpaqueToken()
	if err != nil {
		return Session{}, fmt.Errorf("auth: generate refresh token: %w", err)
	}

	if err := s.repo.rotate(ctx, record.ID, u.ID, newRaw, s.refreshTTL); err != nil {
		return Session{}, err
	}

	accessToken, err := jwt.Issue(s.jwtSecret, jwt.Claims{UserID: u.ID, Role: u.Role}, s.accessTTL)
	if err != nil {
		return Session{}, fmt.Errorf("auth: issue access token: %w", err)
	}

	return Session{AccessToken: accessToken, RefreshToken: newRaw, RefreshTTL: s.refreshTTL, User: u}, nil
}

func (s *service) Logout(ctx context.Context, rawRefreshToken string) error {
	record, err := s.repo.getValidByToken(ctx, rawRefreshToken)
	if err != nil {
		// Already invalid/expired/revoked — logout is idempotent either way,
		// the caller just wants to be logged out and that's already true.
		return nil
	}
	return s.repo.revoke(ctx, record.ID)
}

func (s *service) CleanupExpiredRefreshTokens(ctx context.Context) (int64, error) {
	return s.repo.deleteExpired(ctx, time.Now().Add(-refreshTokenRevokedRetention))
}

func (s *service) issueSession(ctx context.Context, u user.User) (Session, error) {
	accessToken, err := jwt.Issue(s.jwtSecret, jwt.Claims{UserID: u.ID, Role: u.Role}, s.accessTTL)
	if err != nil {
		return Session{}, fmt.Errorf("auth: issue access token: %w", err)
	}

	rawRefresh, err := newOpaqueToken()
	if err != nil {
		return Session{}, fmt.Errorf("auth: generate refresh token: %w", err)
	}

	if err := s.repo.create(ctx, u.ID, rawRefresh, s.refreshTTL); err != nil {
		return Session{}, fmt.Errorf("auth: store refresh token: %w", err)
	}

	return Session{AccessToken: accessToken, RefreshToken: rawRefresh, RefreshTTL: s.refreshTTL, User: u}, nil
}

func newOpaqueToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
