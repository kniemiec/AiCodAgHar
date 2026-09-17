// Package approval holds the ApprovalVerifier interface: v1's fail-closed
// stub, NoopApprovalVerifier, and the real implementation, RealApprovalVerifier
// (docs/designs/completed/full-pipeline-async-approval-design.md §4.4) — the
// actual issue/label/webhook-HMAC verification work happens in
// cmd/control-plane's webhook handler; by the time VerifyApproval is called,
// all that's left is reading back what the handler already persisted.
package approval

import "context"

// ApprovalResult is the outcome of an approval check.
type ApprovalResult struct {
	Approved   bool
	ApprovedBy string
}

// ApprovalVerifier is ADR-9's human-approval test-seam interface.
type ApprovalVerifier interface {
	// VerifyApproval reports whether taskID's pending approve-phase gate
	// has a recorded, verified approval.
	VerifyApproval(ctx context.Context, taskID string) (ApprovalResult, error)
}
