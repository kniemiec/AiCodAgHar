package main

import (
	"encoding/json"

	"github.com/kniemiec/AiCodAgHar/internal/launcher/vsock"
)

// failClosedStructuredResult is what the launcher substitutes whenever an
// agent-stage invocation doesn't end in a real, parseable verdict from the
// guest — design §6: "an inconclusive run must not look clean." It is
// never RunResult.Status = "completed" alongside this value.
const failClosedStructuredResult = `{"has_blocking_finding":true}`

// validStructuredResult reports whether data is a JSON object carrying a
// boolean has_blocking_finding field — the only shape AgentStageExecutor's
// own parsing (agentStageResult) treats as a real verdict rather than an
// absent or malformed one.
func validStructuredResult(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var parsed struct {
		HasBlockingFinding *bool `json:"has_blocking_finding"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false
	}
	return parsed.HasBlockingFinding != nil
}

// deriveVsockOutcome turns one vsock wait's raw result into this launch's
// status, exit code, and structured result — design §6's status table and
// fail-closed default, formalized. waitErr is whatever vsock.WaitForResult
// returned (a guest-reported error envelope, a connection that closed
// without ever sending a structured_result record — which is exactly what
// the guest wrapper's instant-exit-0 detection now produces instead of a
// fabricated success — or a stream-read failure).
func deriveVsockOutcome(result vsock.Result, waitErr error) (status string, exitCode int32, structuredResultJSON []byte) {
	if waitErr == nil && validStructuredResult(result.StructuredResultJSON) {
		return "completed", 0, result.StructuredResultJSON
	}
	return "error", 1, []byte(failClosedStructuredResult)
}

// deriveCommandOutcome is deriveVsockOutcome's deterministic-step
// counterpart (design §6): ExitCode is the guest command's own real exit
// code, propagated directly — not translated through has_blocking_finding,
// which is an agent-stage-only concept (DeterministicStepExecutor derives
// HasFinding straight from ExitCode != 0). A run that never reported a real
// exit code — guest crash, timeout, or a stream that closed without a
// command_result record — still needs to read as a finding, so it fails
// closed to a synthetic non-zero exit code, the same fail-closed posture
// deriveVsockOutcome takes for agent-stage.
func deriveCommandOutcome(result vsock.Result, waitErr error) (status string, exitCode int32) {
	if waitErr == nil && result.ExitCode != nil {
		return "completed", *result.ExitCode
	}
	return "error", 1
}
