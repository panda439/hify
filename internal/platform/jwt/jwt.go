// Package jwt is a thin, stateless wrapper around access-token signing and
// verification. It deliberately knows nothing about users, sessions, or
// refresh tokens — that business logic lives in internal/auth. This package
// exists so internal/server/middleware can verify incoming bearer tokens
// without importing internal/auth: auth (layer 1) depends on internal/user
// (layer 0) for credential lookups, and middleware is imported by every
// module's handler.go (including user's, at layer 0) — if middleware
// imported internal/auth directly, that would close an import cycle
// (user -> middleware -> auth -> user). Keeping token crypto here, at
// layer 0 alongside platform, breaks the cycle.
package jwt

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("jwt: invalid token")
	ErrExpiredToken = errors.New("jwt: expired token")
)

// Claims is the canonical shape of a Hify access token's payload.
type Claims struct {
	UserID string
	Role   string
}

type registeredClaims struct {
	UserID string `json:"uid"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

// Issue signs a new access token for the given user/role, expiring after ttl.
func Issue(secret string, claims Claims, ttl time.Duration) (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, registeredClaims{
		UserID: claims.UserID,
		Role:   claims.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Subject:   claims.UserID,
		},
	})

	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("jwt: sign token: %w", err)
	}
	return signed, nil
}

// Verify parses and validates tokenString, returning its claims.
func Verify(secret, tokenString string) (Claims, error) {
	parsed, err := jwt.ParseWithClaims(tokenString, &registeredClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Claims{}, ErrExpiredToken
		}
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := parsed.Claims.(*registeredClaims)
	if !ok || !parsed.Valid {
		return Claims{}, ErrInvalidToken
	}

	return Claims{UserID: claims.UserID, Role: claims.Role}, nil
}
