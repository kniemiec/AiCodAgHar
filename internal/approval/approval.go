// Package approval holds the ApprovalVerifier interface; its v1 fail-closed
// stub, NoopApprovalVerifier, lands in Phase 2.
package approval

import "context"

// ApprovalCheckRequest identifies the Forgejo issue/webhook evidence a human
// approval would be verified against (ADR-9's issue+label+webhook-HMAC flow).
type ApprovalCheckRequest struct {
	TargetRepo       string
	IssueNumber      int
	WebhookSignature []byte
	WebhookPayload   []byte
}

// ApprovalResult is the outcome of an approval check.
type ApprovalResult struct {
	Approved   bool
	ApprovedBy string
	Evidence   string
}

// ApprovalVerifier is ADR-9's human-approval test-seam interface. This plan
// defers the real Forgejo issue+label+webhook-HMAC mechanism; its only
// implementation here is NoopApprovalVerifier, a fail-closed stub that
// always returns Approved: false, matching the ADR-3/ADR-9 fail-closed
// posture.
type ApprovalVerifier interface {
	VerifyApproval(ctx context.Context, req ApprovalCheckRequest) (ApprovalResult, error)
}
