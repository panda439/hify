package provider

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sony/gobreaker"

	"hify/internal/platform/apperr"
)

var (
	ErrNotFound        = apperr.NotFound("provider.not_found", "供应商不存在")
	ErrNameTaken       = apperr.Conflict("provider.name_taken", "该供应商名称已被使用")
	ErrInactive        = apperr.InvalidInput("provider.inactive", "该供应商已被禁用")
	ErrUnsupportedType = apperr.InvalidInput("provider.unsupported_adapter_type", "不支持的适配器类型")
	ErrModelNotFound   = apperr.NotFound("provider.model_not_found", "模型不存在")
	ErrModelNameTaken  = apperr.Conflict("provider.model_name_taken", "该供应商下已存在同名模型")
	ErrWrongCapability = apperr.InvalidInput("provider.wrong_capability", "模型能力类型不匹配")
	ErrAPIKeyRequired  = apperr.InvalidInput("provider.api_key_required", "该鉴权方式需要填写 API Key")
	ErrInvalidRequest  = apperr.InvalidInput("provider.invalid_request", "请求参数不正确")
)

// WrapClientError translates a raw Client-call error (from the adapter, or
// from the resilience decorator's circuit breaker) into an *apperr.AppError
// so httperr.Write returns a clean 4xx instead of falling through to a
// generic 500 — a wrong API key or an unreachable base_url is an expected,
// user-actionable outcome, not a Hify bug. Deliberately uses InvalidInput
// (400) even for what was an upstream 401/403: HTTP 401 from Hify's own API
// is reserved for "your Hify session is invalid" (the frontend's
// lib/api.ts auto-refreshes on any 401), so reusing it here for "the
// provider rejected our stored key" would incorrectly trigger that flow.
func WrapClientError(err error) error {
	if err == nil {
		return nil
	}

	var ae *adapterError
	if errors.As(err, &ae) {
		return apperr.InvalidInput("provider.request_failed", describeAdapterError(ae)).WithCause(err)
	}
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return apperr.InvalidInput("provider.circuit_open", "该供应商当前不可用（连续失败次数过多，已暂时熔断），请稍后重试").WithCause(err)
	}
	return err
}

func describeAdapterError(ae *adapterError) string {
	switch {
	case ae.status == http.StatusUnauthorized || ae.status == http.StatusForbidden:
		return "鉴权失败，请检查 API Key 是否正确"
	case ae.status == http.StatusNotFound:
		return "供应商地址不正确或接口不存在"
	case ae.status == http.StatusTooManyRequests:
		return "供应商限流，请稍后重试"
	case ae.status >= 500:
		return "供应商服务异常，请稍后重试"
	case ae.status == 0:
		return "无法连接到供应商，请检查地址是否正确"
	default:
		return fmt.Sprintf("请求供应商失败（状态码 %d）", ae.status)
	}
}
