package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"

	"hify/internal/db/gen"
)

const mysqlDuplicateEntry = 1062

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	queries *gen.Queries
}

func (r *Repository) create(ctx context.Context, u User, passwordHash string) error {
	err := r.queries.CreateUser(ctx, gen.CreateUserParams{
		ID:           u.ID,
		Email:        u.Email,
		PasswordHash: passwordHash,
		DisplayName:  u.DisplayName,
		Role:         u.Role,
		IsActive:     u.IsActive,
	})
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateEntry {
			return ErrEmailTaken
		}
		return fmt.Errorf("user: create: %w", err)
	}
	return nil
}

func (r *Repository) getByID(ctx context.Context, id string) (User, error) {
	row, err := r.queries.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("user: get by id: %w", err)
	}
	return toDomain(row), nil
}

// getByEmailWithHash is the one place the password hash leaves the storage
// layer, used only by Service.VerifyPassword — it never becomes part of the
// public User type.
func (r *Repository) getByEmailWithHash(ctx context.Context, email string) (User, string, error) {
	row, err := r.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, "", ErrNotFound
		}
		return User{}, "", fmt.Errorf("user: get by email: %w", err)
	}
	return toDomain(row), row.PasswordHash, nil
}

func (r *Repository) list(ctx context.Context, limit, offset int) ([]User, error) {
	rows, err := r.queries.ListUsers(ctx, gen.ListUsersParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("user: list: %w", err)
	}
	out := make([]User, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomain(row))
	}
	return out, nil
}

func (r *Repository) count(ctx context.Context) (int, error) {
	n, err := r.queries.CountUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("user: count: %w", err)
	}
	return int(n), nil
}

func toDomain(row gen.User) User {
	return User{
		ID:          row.ID,
		Email:       row.Email,
		DisplayName: row.DisplayName,
		Role:        row.Role,
		IsActive:    row.IsActive,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}
