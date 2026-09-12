package provider

import (
	"context"
	"errors"
	"net"

	"github.com/sony/gobreaker"
)

// RerankFailureKind is the only failure detail allowed to cross the
// provider/benchmark boundary. It deliberately contains no upstream message,
// response body, query, or document content.
type RerankFailureKind string

const (
	RerankFailureTimeout         RerankFailureKind = "timeout"
	RerankFailureHTTP429         RerankFailureKind = "http_429"
	RerankFailureCircuitOpen     RerankFailureKind = "circuit_open"
	RerankFailureResponseInvalid RerankFailureKind = "response_invalid"
	RerankFailureOther           RerankFailureKind = "other"
)

// ErrRerankResponseInvalid marks malformed or semantically invalid rerank
// responses without retaining the provider's raw response in observer data.
var ErrRerankResponseInvalid = errors.New("rerank response invalid")

// ClassifyRerankFailure maps any provider-side rerank error to the fixed safe
// vocabulary. Callers must persist only the returned kind and aggregate count.
func ClassifyRerankFailure(err error) RerankFailureKind {
	if err == nil {
		return ""
	}
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return RerankFailureCircuitOpen
	}
	var ae *adapterError
	if errors.As(err, &ae) && ae.status == 429 {
		return RerankFailureHTTP429
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return RerankFailureTimeout
	}
	var timeoutErr net.Error
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return RerankFailureTimeout
	}
	if errors.Is(err, ErrRerankResponseInvalid) {
		return RerankFailureResponseInvalid
	}
	return RerankFailureOther
}
