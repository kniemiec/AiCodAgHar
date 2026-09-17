package fakes

import (
	"context"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/approval"
)

// FakeApprovalVerifier is an in-memory ApprovalVerifier. It defaults to
// NoopApprovalVerifier's fail-closed behavior (Approved: false); set Result
// to script an approved outcome.
type FakeApprovalVerifier struct {
	Result approval.ApprovalResult
	Err    error
}

func (f *FakeApprovalVerifier) VerifyApproval(ctx context.Context, taskID string) (approval.ApprovalResult, error) {
	return f.Result, f.Err
}
