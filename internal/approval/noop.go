package approval

import "context"

// NoopApprovalVerifier is the v1 fail-closed stub: it never verifies a real
// Forgejo issue/label/webhook-HMAC approval and always reports Approved:
// false, matching the ADR-3/ADR-9 fail-closed posture. It exists so
// coreloop's require_human_approval branch has a real, wired dependency
// before the actual approval flow is built.
type NoopApprovalVerifier struct{}

func (NoopApprovalVerifier) VerifyApproval(ctx context.Context, req ApprovalCheckRequest) (ApprovalResult, error) {
	return ApprovalResult{Approved: false}, nil
}
