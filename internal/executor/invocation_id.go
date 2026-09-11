package executor

import "fmt"

// invocationID deterministically derives an invocation ID from the
// (task, round, step) triple. Deterministic IDs keep Phase 2's unit tests
// simple; revisit if a task ever needs to run the same step twice in one
// round.
func invocationID(sc StepContext) string {
	return fmt.Sprintf("%s:%d:%s", sc.Task.ID, sc.Round, sc.Step.StepID)
}
