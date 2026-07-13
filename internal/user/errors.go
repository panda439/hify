package user

import "hify/internal/platform/apperr"

var (
	ErrNotFound          = apperr.NotFound("user.not_found", "用户不存在")
	ErrEmailTaken        = apperr.Conflict("user.email_taken", "该邮箱已被注册")
	ErrInvalidCredential = apperr.Unauthorized("user.invalid_credential", "邮箱或密码错误")
	ErrInactive          = apperr.Forbidden("user.inactive", "该账号已被禁用")
)
