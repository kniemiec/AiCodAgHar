// Package audit holds the AuditWriter interface; its Postgres implementation
// and hash-chain logic for the append-only invocation_audit trail land in
// Phase 5.
package audit

import "context"

// InvocationAuditRecord is one row of the audit.invocation_audit table
// (Phase 5's Postgres DDL): a merge of ADR-2's delegation record, ADR-3's PDP
// decision (including ADR-11's resolved_steps extension), ADR-1's execution
// outcome, and ADR-9's approval fields, plus the SHA-256 hash-chain fields.
//
// StepType also covers two Phase 5 additions: "pdp-decision" (one row per
// round's PDP Evaluate call, so a deny/require-approval round that launches
// nothing is still audited) and "task-outcome" (one row per task's terminal
// stop-success/stop-escalate outcome). Round is a Phase 5 addition too — it
// lets the policy-gate reconciliation check correlate a round's PDP
// decision with that round's launches without parsing invocation_id
// strings.
type InvocationAuditRecord struct {
	InvocationID string
	TaskID       string
	Round        int
	StepID       string
	StepType     string
	BotAccount   string
	TargetRepo   string

	// PDP decision (ADR-3), including ADR-11's resolved_steps extension.
	PolicyDecision      string
	GrantedScopes       []string
	PolicyBundleVersion string
	ResolvedStepsJSON   []byte

	// Execution outcome (ADR-1).
	ExitCode int32
	Status   string

	// Approval (ADR-9).
	Approved   bool
	ApprovedBy string
	Evidence   string

	// Hash chain (ADR-5).
	PrevHash string
	RowHash  string
}

// AuditWriter is ADR-5's append-only, hash-chained audit-trail test-seam
// interface. Its production implementation writes to the audit schema via
// the orchestrator_audit_writer Postgres role, which is granted SELECT and
// INSERT only — never UPDATE or DELETE — making ADR-5's "append-only"
// property a database grant, not just a convention. SELECT remains
// necessary: the hash chain must read the previous row to extend it, and
// GetTaskChain is itself a read.
type AuditWriter interface {
	WriteInvocationRecord(ctx context.Context, rec InvocationAuditRecord) error
	GetTaskChain(ctx context.Context, taskID string) ([]InvocationAuditRecord, error)
}
