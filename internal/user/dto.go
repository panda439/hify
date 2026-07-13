package user

type userResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func toResponse(u User) userResponse {
	return userResponse{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        u.Role,
	}
}
