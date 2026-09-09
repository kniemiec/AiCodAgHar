// Package fakes holds one in-memory fake per ADR-9 test-seam interface,
// used by internal/coreloop and internal/executor's unit tests so that
// `go test ./...` never needs a live service.
package fakes

import (
	"context"
	"sync"

	"github.com/kniemiec/AiCodAgHar/internal/clients/policyclient"
)

// FakePolicyClient is an in-memory PolicyClient. By default it allows every
// request with no resolved steps; set EvaluateFunc to script round-by-round
// or deny/require-approval behavior.
type FakePolicyClient struct {
	EvaluateFunc func(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error)

	mu    sync.Mutex
	calls []policyclient.PolicyRequest
}

func (f *FakePolicyClient) Evaluate(ctx context.Context, req policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	if f.EvaluateFunc != nil {
		return f.EvaluateFunc(ctx, req)
	}
	return policyclient.PolicyDecision{Decision: "allow"}, nil
}

// Calls returns every PolicyRequest Evaluate has been called with, in order.
func (f *FakePolicyClient) Calls() []policyclient.PolicyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]policyclient.PolicyRequest, len(f.calls))
	copy(out, f.calls)
	return out
}
