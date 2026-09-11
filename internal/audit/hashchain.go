package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// GenesisHash is the prev_hash value for the first row of a given task_id's
// chain. Exported so internal/fakes.FakeAuditWriter can maintain a real
// hash chain too, not just store records verbatim.
const GenesisHash = "genesis"

// hashableFields is the canonical, field-order-stable representation of an
// InvocationAuditRecord's content that gets hashed into RowHash. It
// deliberately excludes PrevHash/RowHash (computed from this, not part of
// it) and the server-assigned CreatedAt.
type hashableFields struct {
	InvocationID        string   `json:"invocation_id"`
	TaskID              string   `json:"task_id"`
	Round               int      `json:"round"`
	StepID              string   `json:"step_id"`
	StepType            string   `json:"step_type"`
	BotAccount          string   `json:"bot_account"`
	TargetRepo          string   `json:"target_repo"`
	PolicyDecision      string   `json:"policy_decision"`
	GrantedScopes       []string `json:"granted_scopes"`
	PolicyBundleVersion string   `json:"policy_bundle_version"`
	ResolvedStepsJSON   string   `json:"resolved_steps_json"`
	ExitCode            int32    `json:"exit_code"`
	Status              string   `json:"status"`
	Approved            bool     `json:"approved"`
	ApprovedBy          string   `json:"approved_by"`
	Evidence            string   `json:"evidence"`
}

func toHashableFields(rec InvocationAuditRecord) hashableFields {
	return hashableFields{
		InvocationID:        rec.InvocationID,
		TaskID:              rec.TaskID,
		Round:               rec.Round,
		StepID:              rec.StepID,
		StepType:            rec.StepType,
		BotAccount:          rec.BotAccount,
		TargetRepo:          rec.TargetRepo,
		PolicyDecision:      rec.PolicyDecision,
		GrantedScopes:       rec.GrantedScopes,
		PolicyBundleVersion: rec.PolicyBundleVersion,
		ResolvedStepsJSON:   string(rec.ResolvedStepsJSON),
		ExitCode:            rec.ExitCode,
		Status:              rec.Status,
		Approved:            rec.Approved,
		ApprovedBy:          rec.ApprovedBy,
		Evidence:            rec.Evidence,
	}
}

// ComputeRowHash is SHA-256(canonical JSON of rec's content || prevHash),
// hex-encoded. Shared by PostgresWriter (to extend the chain),
// fakes.FakeAuditWriter (so unit tests exercise real chain semantics), and
// VerifyChain (to detect tampering).
func ComputeRowHash(rec InvocationAuditRecord, prevHash string) (string, error) {
	b, err := json.Marshal(toHashableFields(rec))
	if err != nil {
		return "", fmt.Errorf("marshal hashable fields for %s: %w", rec.InvocationID, err)
	}
	h := sha256.New()
	h.Write(b)
	h.Write([]byte(prevHash))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyChain checks that records — one task_id's chain, in insertion order
// (as GetTaskChain returns it) — is internally consistent: each row's
// RowHash matches its own content, and each row's PrevHash matches the
// previous row's RowHash. It returns a descriptive error identifying the
// first broken link, or nil if the chain is intact.
func VerifyChain(records []InvocationAuditRecord) error {
	prevHash := GenesisHash
	for i, rec := range records {
		if rec.PrevHash != prevHash {
			return fmt.Errorf("record %d (invocation_id=%s): prev_hash %q does not match preceding row's row_hash %q", i, rec.InvocationID, rec.PrevHash, prevHash)
		}
		want, err := ComputeRowHash(rec, rec.PrevHash)
		if err != nil {
			return err
		}
		if rec.RowHash != want {
			return fmt.Errorf("record %d (invocation_id=%s): row_hash %q does not match recomputed hash %q — row content was tampered with", i, rec.InvocationID, rec.RowHash, want)
		}
		prevHash = rec.RowHash
	}
	return nil
}
