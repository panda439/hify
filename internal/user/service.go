package user

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"hify/internal/platform"
)

const bcryptCost = 12

// Service is user's public contract — the only thing other modules (auth,
// at layer 1) are allowed to depend on.
type Service interface {
	Create(ctx context.Context, email, displayName, password, role string) (User, error)
	GetByID(ctx context.Context, id string) (User, error)
	// VerifyPassword checks email+password and returns the matching user.
	// This is where "does this credential match" lives, since Repository
	// owns the password_hash column and User itself never carries it.
	VerifyPassword(ctx context.Context, email, password string) (User, error)
	List(ctx context.Context, limit, offset int) ([]User, int, error)
}

// service is constructed via NewService in wire.go.
type service struct {
	repo *Repository
}

func (s *service) Create(ctx context.Context, email, displayName, password, role string) (User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return User{}, fmt.Errorf("user: hash password: %w", err)
	}

	u := User{
		ID:          platform.NewID(),
		Email:       email,
		DisplayName: displayName,
		Role:        role,
		IsActive:    true,
	}
	if err := s.repo.create(ctx, u, string(hash)); err != nil {
		return User{}, err
	}
	return s.repo.getByID(ctx, u.ID)
}

func (s *service) GetByID(ctx context.Context, id string) (User, error) {
	return s.repo.getByID(ctx, id)
}

func (s *service) VerifyPassword(ctx context.Context, email, password string) (User, error) {
	u, hash, err := s.repo.getByEmailWithHash(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Don't leak whether the email exists at all.
			return User{}, ErrInvalidCredential
		}
		return User{}, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return User{}, ErrInvalidCredential
	}
	if !u.IsActive {
		return User{}, ErrInactive
	}
	return u, nil
}

func (s *service) List(ctx context.Context, limit, offset int) ([]User, int, error) {
	limit = platform.ClampLimit(limit)

	users, err := s.repo.list(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.count(ctx)
	if err != nil {
		return nil, 0, err
	}
	return users, total, nil
}
