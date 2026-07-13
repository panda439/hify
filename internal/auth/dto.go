package auth

type loginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type sessionResponse struct {
	AccessToken string      `json:"access_token"`
	User        userSummary `json:"user"`
}

type userSummary struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func toSessionResponse(s Session) sessionResponse {
	return sessionResponse{
		AccessToken: s.AccessToken,
		User: userSummary{
			ID:          s.User.ID,
			Email:       s.User.Email,
			DisplayName: s.User.DisplayName,
			Role:        s.User.Role,
		},
	}
}
