package executor_test

import (
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/executor"
	"github.com/kniemiec/AiCodAgHar/internal/fakes"
)

func TestRegistry_DispatchByType(t *testing.T) {
	agentStage := &executor.AgentStageExecutor{Launcher: &fakes.FakeContainerLauncher{}}
	deterministicStep := &executor.DeterministicStepExecutor{Launcher: &fakes.FakeContainerLauncher{}}
	reg := executor.NewRegistry(agentStage, deterministicStep)

	got, ok := reg.Lookup(executor.StepTypeAgentStage)
	if !ok || got != executor.StepExecutor(agentStage) {
		t.Fatalf("Lookup(%q) = %v, %v; want agentStage, true", executor.StepTypeAgentStage, got, ok)
	}

	got, ok = reg.Lookup(executor.StepTypeDeterministicStep)
	if !ok || got != executor.StepExecutor(deterministicStep) {
		t.Fatalf("Lookup(%q) = %v, %v; want deterministicStep, true", executor.StepTypeDeterministicStep, got, ok)
	}

	if _, ok := reg.Lookup("unknown-type"); ok {
		t.Fatal("Lookup(\"unknown-type\") = ok=true; want ok=false for a type outside the closed set")
	}
}
