// Package apperr defines the small, fixed taxonomy of business error kinds
// every module builds its sentinel errors from. Kind drives the HTTP status
// code (see internal/platform/httperr); Code is a module-namespaced string
// (e.g. "user.email_taken") for frontend-side branching that's more specific
// than the coarse Kind. Modules declare their own errors against this type
// in their own errors.go — there is no central per-error registry to keep in
// sync, which would violate the modular-monolith rule that modules only
// touch their own files.
package apperr

// Kind classifies an error for HTTP-status mapping. Keep this list short:
// it is the one piece of error taxonomy every module shares.
type Kind string

const (
	KindNotFound     Kind = "not_found"
	KindConflict     Kind = "conflict"
	KindInvalidInput Kind = "invalid_input"
	KindUnauthorized Kind = "unauthorized"
	KindForbidden    Kind = "forbidden"
	KindRateLimited  Kind = "rate_limited"
)

// AppError is the business-exception type every module's sentinel errors
// should be built from via the constructors below.
type AppError struct {
	Kind    Kind
	Code    string // module-namespaced, e.g. "user.email_taken"
	Message string // human-readable, safe to return to the client
	err     error  // optional wrapped cause, kept out of the client-facing message
}

func (e *AppError) Error() string { return e.Message }

func (e *AppError) Unwrap() error { return e.err }

// Is lets errors.Is match on Kind+Code so callers can check
// errors.Is(err, user.ErrEmailTaken) the same way they would any sentinel.
func (e *AppError) Is(target error) bool {
	t, ok := target.(*AppError)
	if !ok {
		return false
	}
	return e.Kind == t.Kind && e.Code == t.Code
}

// WithCause attaches an underlying error for logging/errors.Is chains
// without leaking its message to the client (AppError.Message is what gets
// serialized in the HTTP response, not the wrapped cause).
func (e *AppError) WithCause(cause error) *AppError {
	return &AppError{Kind: e.Kind, Code: e.Code, Message: e.Message, err: cause}
}

func New(kind Kind, code, message string) *AppError {
	return &AppError{Kind: kind, Code: code, Message: message}
}

func NotFound(code, message string) *AppError        { return New(KindNotFound, code, message) }
func Conflict(code, message string) *AppError        { return New(KindConflict, code, message) }
func InvalidInput(code, message string) *AppError    { return New(KindInvalidInput, code, message) }
func Unauthorized(code, message string) *AppError    { return New(KindUnauthorized, code, message) }
func Forbidden(code, message string) *AppError       { return New(KindForbidden, code, message) }
func TooManyRequests(code, message string) *AppError { return New(KindRateLimited, code, message) }
