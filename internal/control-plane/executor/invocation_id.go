package executor

import "fmt"

// InvocationID deterministically derives an invocation ID from the
// (task, round, step) triple. Deterministic IDs keep Phase 2's unit tests
// simple; revisit if a task ever needs to run the same step twice in one
// round.
//
// Exported so internal/control-plane/coreloop can precompute a step's invocation ID
// before dispatch — internal/control-plane/taskrunner's crash-recovery step_runs row
// needs to exist from the moment a step launches, not just once it
// resolves (design docs/designs/completed/full-pipeline-async-approval-design.md
// §4.2), and the ID is this package's single source of truth for the
// format.
//
// Only [A-Za-z0-9_-] is allowed: the real container-launcher embeds the ID
// in jail paths and jailer's --id, and rejects anything else
// (cmd/container-launcher/server.go's validInvocationID). launcher-mock
// doesn't check, so a colon-separated format passed every mocked test but
// failed every real Launch with InvalidArgument.
func InvocationID(sc StepContext) string {
	return fmt.Sprintf("%s-r%d-%s", sc.Task.ID, sc.Round, sc.Step.StepID)
}
