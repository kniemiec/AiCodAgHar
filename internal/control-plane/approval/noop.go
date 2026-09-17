package approval

import "context"

// NoopApprovalVerifier is the fail-closed default when no webhook is
// configured (e.g. local dev without the real Forgejo webhook wired up): it
// never verifies a real approval and always reports Approved: false,
// matching the ADR-3/ADR-9 fail-closed posture.
type NoopApprovalVerifier struct{}

func (NoopApprovalVerifier) VerifyApproval(ctx context.Context, taskID string) (ApprovalResult, error) {
	return ApprovalResult{Approved: false}, nil
}
