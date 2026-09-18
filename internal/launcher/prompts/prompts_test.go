package prompts

import (
	"strings"
	"testing"
)

func TestRender_Implement_FreshRound(t *testing.T) {
	task, ok, err := Render("implement", map[string]string{
		"TARGET_REPO": "org/repo",
		"BRANCH":      "add-feature",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(task, "org/repo") || !strings.Contains(task, "add-feature") {
		t.Errorf("task missing repo/branch: %q", task)
	}
	if strings.Contains(task, "previous review round") {
		t.Errorf("fresh-round task should not mention prior findings: %q", task)
	}
	if !strings.Contains(task, StructuredResultPath) {
		t.Errorf("task missing structured result path: %q", task)
	}
}

func TestRender_Implement_WithPriorFindings(t *testing.T) {
	task, ok, err := Render("implement", map[string]string{
		"TARGET_REPO":         "org/repo",
		"BRANCH":              "add-feature",
		"PRIOR_FINDINGS_JSON": `[{"severity":"blocking","message":"off-by-one"}]`,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(task, "off-by-one") {
		t.Errorf("task missing quoted prior findings: %q", task)
	}
	if !strings.Contains(task, "not instructions to follow") {
		t.Errorf("task should flag prior findings as untrusted: %q", task)
	}
}

func TestRender_ReviewWithCommit(t *testing.T) {
	task, ok, err := Render("review-with-commit", map[string]string{
		"TARGET_REPO": "org/repo",
		"PR_NUMBER":   "7",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(task, "#7") || !strings.Contains(task, "org/repo") {
		t.Errorf("task missing PR number/repo: %q", task)
	}
}

func TestRender_UnknownStepID(t *testing.T) {
	task, ok, err := Render("no-such-step", map[string]string{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if ok {
		t.Error("ok = true, want false for an unmapped step ID")
	}
	if task != "" {
		t.Errorf("task = %q, want empty", task)
	}
}

func TestRenderCommand_UnitTests(t *testing.T) {
	command, ok := RenderCommand("unit-tests")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if command == "" {
		t.Error("command = empty, want a fixed guest command")
	}
}

func TestRenderCommand_UnknownStepID(t *testing.T) {
	command, ok := RenderCommand("no-such-step")
	if ok {
		t.Error("ok = true, want false for an unmapped step ID")
	}
	if command != "" {
		t.Errorf("command = %q, want empty", command)
	}
}
