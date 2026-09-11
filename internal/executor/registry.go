package executor

// Step type constants — the closed set ADR-11 fixes for the StepExecutor
// registry key.
const (
	StepTypeAgentStage        = "agent-stage"
	StepTypeDeterministicStep = "deterministic-step"
)

// Registry is the closed-set StepExecutor registry keyed by step type.
// It is intentionally not open for arbitrary registration — ADR-11 fixes
// the set of step types, so the registry's constructor takes exactly the
// two known executors.
type Registry struct {
	executors map[string]StepExecutor
}

// NewRegistry builds the closed-set registry from the two known executors.
func NewRegistry(agentStage, deterministicStep StepExecutor) *Registry {
	return &Registry{
		executors: map[string]StepExecutor{
			StepTypeAgentStage:        agentStage,
			StepTypeDeterministicStep: deterministicStep,
		},
	}
}

// Lookup returns the StepExecutor registered for stepType, or ok=false if
// stepType is outside the closed set.
func (r *Registry) Lookup(stepType string) (StepExecutor, bool) {
	e, ok := r.executors[stepType]
	return e, ok
}
