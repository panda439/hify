package auth

import "hify/internal/platform/apperr"

var (
	ErrInvalidRefreshToken = apperr.Unauthorized("auth.invalid_refresh_token", "登录已过期，请重新登录")
	ErrTooManyAttempts     = apperr.TooManyRequests("auth.too_many_attempts", "登录尝试次数过多，请稍后再试")
	ErrInvalidRequest      = apperr.InvalidInput("auth.invalid_request", "请输入邮箱和密码")
	ErrInactiveUser        = apperr.Forbidden("auth.inactive_user", "该账号已被禁用")
)
