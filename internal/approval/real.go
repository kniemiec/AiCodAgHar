package approval

import (
	"context"
	"fmt"
)

// TaskApprovalReader is RealApprovalVerifier's minimal read into
// orchestrator.tasks — narrow enough that this package doesn't need to
// import internal/taskrunner (which imports internal/coreloop, which
// imports this package): taskrunner wires an adapter over its own
// TaskStore, the same pattern coreloop.StepRunRecorder uses.
type TaskApprovalReader interface {
	// ApprovedBy returns the taskID's persisted approved_by column ("" if
	// not yet approved).
	ApprovedBy(ctx context.Context, taskID string) (approvedBy string, err error)
}

// RealApprovalVerifier is design §4.4's real ApprovalVerifier: all the
// actual verification (HMAC signature, APPROVERS-file membership, live
// collaborator-role check) already happened in cmd/control-plane's webhook
// handler before it persisted approved_by — this just reads that back.
type RealApprovalVerifier struct {
	Reader TaskApprovalReader
}

func (v *RealApprovalVerifier) VerifyApproval(ctx context.Context, taskID string) (ApprovalResult, error) {
	approvedBy, err := v.Reader.ApprovedBy(ctx, taskID)
	if err != nil {
		return ApprovalResult{}, fmt.Errorf("approval: verify %s: %w", taskID, err)
	}
	return ApprovalResult{Approved: approvedBy != "", ApprovedBy: approvedBy}, nil
}
