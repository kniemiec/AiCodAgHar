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
func InvocationID(sc StepContext) string {
	return fmt.Sprintf("%s:%d:%s", sc.Task.ID, sc.Round, sc.Step.StepID)
}
