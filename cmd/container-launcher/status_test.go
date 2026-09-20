package main

import (
	"errors"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/launcher/vsock"
)

func TestValidStructuredResult(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"well-formed false", []byte(`{"has_blocking_finding":false}`), true},
		{"well-formed true", []byte(`{"has_blocking_finding":true}`), true},
		{"nil", nil, false},
		{"empty", []byte(``), false},
		{"empty object", []byte(`{}`), false},
		{"wrong type", []byte(`{"has_blocking_finding":"yes"}`), false},
		{"not json", []byte(`not json at all`), false},
		{"extra fields still valid", []byte(`{"has_blocking_finding":true,"notes":"ok"}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validStructuredResult(tc.data); got != tc.want {
				t.Errorf("validStructuredResult(%s) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestDeriveVsockOutcome_ValidResult(t *testing.T) {
	result := vsock.Result{StructuredResultJSON: []byte(`{"has_blocking_finding":false}`)}
	status, exitCode, structuredResult := deriveVsockOutcome(result, nil)
	if status != "completed" || exitCode != 0 {
		t.Errorf("status=%q exitCode=%d, want completed/0", status, exitCode)
	}
	if string(structuredResult) != `{"has_blocking_finding":false}` {
		t.Errorf("structuredResult = %q, want the guest's own verdict passed through", structuredResult)
	}
}

func TestDeriveVsockOutcome_GuestReportedError(t *testing.T) {
	result := vsock.Result{}
	status, exitCode, structuredResult := deriveVsockOutcome(result, errors.New("guest wrapper reported an error: MMDS fetch failed"))
	if status != "error" || exitCode != 1 {
		t.Errorf("status=%q exitCode=%d, want error/1", status, exitCode)
	}
	if string(structuredResult) != failClosedStructuredResult {
		t.Errorf("structuredResult = %q, want fail-closed default", structuredResult)
	}
}

func TestDeriveVsockOutcome_MalformedResultDespiteNoWaitError(t *testing.T) {
	// The guest did send a structured_result envelope, but its content
	// doesn't carry a real verdict — design §6: absent OR malformed must
	// both fail closed, not just the "absent" case.
	result := vsock.Result{StructuredResultJSON: []byte(`{}`)}
	status, exitCode, structuredResult := deriveVsockOutcome(result, nil)
	if status != "error" || exitCode != 1 {
		t.Errorf("status=%q exitCode=%d, want error/1", status, exitCode)
	}
	if string(structuredResult) != failClosedStructuredResult {
		t.Errorf("structuredResult = %q, want fail-closed default", structuredResult)
	}
}

func TestDeriveVsockOutcome_NeverCompletedWithFailClosedDefault(t *testing.T) {
	// Guard against the two fields drifting apart: "completed" must never
	// be paired with the fail-closed placeholder, and the placeholder must
	// never be paired with "completed" — design §6's stated invariant.
	status, _, structuredResult := deriveVsockOutcome(vsock.Result{}, errors.New("boom"))
	if status == "completed" {
		t.Fatal("status = completed for a failed wait, want a failure status")
	}
	if string(structuredResult) != failClosedStructuredResult {
		t.Errorf("structuredResult = %q, want fail-closed default", structuredResult)
	}
}

func TestDeriveCommandOutcome_ValidResult(t *testing.T) {
	exitCode := int32(0)
	status, gotExitCode := deriveCommandOutcome(vsock.Result{ExitCode: &exitCode}, nil)
	if status != "completed" || gotExitCode != 0 {
		t.Errorf("status=%q exitCode=%d, want completed/0", status, gotExitCode)
	}
}

func TestDeriveCommandOutcome_NonZeroExitPropagatedDirectly(t *testing.T) {
	// deterministic-step's ExitCode is the guest command's own real exit
	// code, not translated through any has_blocking_finding concept.
	exitCode := int32(17)
	status, gotExitCode := deriveCommandOutcome(vsock.Result{ExitCode: &exitCode}, nil)
	if status != "completed" || gotExitCode != 17 {
		t.Errorf("status=%q exitCode=%d, want completed/17", status, gotExitCode)
	}
}

func TestDeriveCommandOutcome_GuestReportedError(t *testing.T) {
	status, exitCode := deriveCommandOutcome(vsock.Result{}, errors.New("guest wrapper reported an error: git clone failed"))
	if status != "error" || exitCode != 1 {
		t.Errorf("status=%q exitCode=%d, want error/1", status, exitCode)
	}
}

func TestDeriveCommandOutcome_ClosedWithoutRecordFailsClosed(t *testing.T) {
	status, exitCode := deriveCommandOutcome(vsock.Result{}, nil)
	if status != "error" || exitCode != 1 {
		t.Errorf("status=%q exitCode=%d, want error/1", status, exitCode)
	}
}
