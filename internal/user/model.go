package user

import "time"

// User is user's domain type. It deliberately never carries the password
// hash — anything that needs to verify a credential goes through
// Service.VerifyPassword instead of reading the hash out of this struct.
type User struct {
	ID          string
	Email       string
	DisplayName string
	Role        string
	IsActive    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)
