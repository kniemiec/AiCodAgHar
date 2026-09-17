// Package coreloop implements ADR-7's fixed per-round decision logic:
// PDP-evaluate (resolve steps) -> fan out generate/review (phase-grouped,
// see groupStepsByPhase) -> aggregate -> if no outstanding blocking
// findings, gate on approve (design §4.4: at least one verified approval,
// no verified rejection) -> dispatch publish -> evaluate (continue /
// stop-success / stop-escalate / waiting-approval).
//
// RunRound runs exactly one round — internal/control-plane/taskrunner (design §4.2) owns
// looping across rounds up to the round cap, persistence, and pause/resume;
// coreloop stays a pure, single-round function apart from what Dependencies
// provides.
package coreloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/approval"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/audit"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/executor"
)

// MaxRounds is ADR-7's hardcoded round cap. internal/control-plane/taskrunner is
// responsible for stopping at it — RunRound itself has no notion of a
// cap, since it only ever runs one round.
const MaxRounds = 5

// Outcome is the result RunRound reaches for one round.
type Outcome string

const (
	// OutcomeContinue means this round found an outstanding blocking
	// finding: not a task-terminal outcome — the caller should dispatch
	// another round (up to MaxRounds) using RoundResult.PriorFindings.
	OutcomeContinue     Outcome = "continue"
	OutcomeStopSuccess  Outcome = "stop-success"
	OutcomeStopEscalate Outcome = "stop-escalate"

	// OutcomeWaitingApproval means this round's approve-phase gate (design
	// §4.4) is unmet: no outstanding blocking findings, but no verified
	// approval recorded yet either. Not task-terminal — the caller persists
	// RoundResult.ApprovalIssueNumber and status=waiting_approval, and stops
	// until an external event (the webhook handler, after verifying an
	// approval) calls back in for this same round.
	OutcomeWaitingApproval Outcome = "waiting-approval"
)

// Dependencies are the test-seam interfaces RunRound needs, plus the
// orchestrator process identity PolicyRequest carries. SecretsClient is
// wired into internal/control-plane/executor directly (credential issuance happens per
// step, not per round).
type Dependencies struct {
	Policy                 policyclient.PolicyClient
	Approval               approval.ApprovalVerifier
	Audit                  audit.AuditWriter
	Executors              *executor.Registry
	OrchestratorInstanceID string

	// Recorder is internal/control-plane/taskrunner's crash-recovery hook into fan-out
	// (design §4.2): when set, fanOut calls it immediately before and after
	// dispatching each step, so a step_runs row exists from the instant a
	// step launches, not only once it resolves. Nil is a valid, common
	// value — e.g. every coreloop-level unit test that isn't exercising
	// persistence — and fanOut treats a nil Recorder as a no-op.
	Recorder StepRunRecorder

	// IssueOpener opens (or reuses) the approval issue an unmet approve-phase
	// gate needs (design §4.4). Any GitHostClient implementation satisfies
	// this structurally; kept narrow so coreloop doesn't need the rest of
	// that interface. Nil is only valid for a task whose config never
	// resolves an approve-phase step.
	IssueOpener IssueOpener
}

// IssueOpener is Dependencies.IssueOpener's interface.
type IssueOpener interface {
	CreateOrReuseIssue(ctx context.Context, repo gitclient.RepoRef, title, body string) (gitclient.Issue, error)
}

// StepRunRecorder is Dependencies.Recorder's interface — kept narrow and
// coreloop-local (rather than taskrunner's whole TaskStore) so coreloop
// stays decision-logic-only and doesn't import taskrunner.
type StepRunRecorder interface {
	// RecordLaunched inserts a step_runs row the instant a step is
	// dispatched, before its outcome is known.
	RecordLaunched(ctx context.Context, taskID string, round int, step policyclient.ResolvedStep, invocationID string) error
	// RecordCompleted updates that row in place once the step resolves.
	// execErr is exec.Execute's own error (nil on success) — it decides
	// step_runs.status (failed vs completed); run's fields are only
	// meaningful when execErr is nil.
	RecordCompleted(ctx context.Context, taskID string, invocationID string, run domain.StepRun, execErr error) error
}

// RoundResult is RunRound's outcome for one round of one task.
type RoundResult struct {
	Outcome Outcome
	Reason  string
	// StepRuns holds only this round's step runs (steps skipped via
	// alreadyCompleted are included too, so callers see a complete picture
	// of the round regardless of what was actually (re)dispatched).
	StepRuns []domain.StepRun
	// PriorFindings is this round's marshaled findings, for the next
	// round's generate-phase dispatch. Set iff Outcome == OutcomeContinue.
	PriorFindings []byte
	// ApprovalIssueNumber is the Forgejo issue a human approves/rejects
	// against. Set iff Outcome == OutcomeWaitingApproval.
	ApprovalIssueNumber int
}

// RunRound executes ADR-7's decision logic once for one round: PDP-evaluate
// -> phase-grouped fan-out -> aggregate -> evaluate. priorFindings is the
// previous round's marshaled findings (nil on round 1), passed through to
// the generate phase's step only.
//
// alreadyCompleted lets a caller skip re-dispatching steps a prior,
// interrupted attempt at this same round already completed — internal/
// taskrunner's startup reconciliation (design §4.2) is the only caller that
// passes a non-empty map; every other caller passes nil. Skipped steps are
// still included in RoundResult.StepRuns and still participate in
// aggregate/evaluate, exactly as if they'd been dispatched just now.
func RunRound(ctx context.Context, deps Dependencies, task domain.Task, round int, priorFindings []byte, alreadyCompleted map[string]domain.StepRun) (RoundResult, error) {
	// The nanosecond suffix is what makes this unique per actual call to
	// RunRound, not just per (task, round): design §4.4's approve-phase
	// resume, and startup reconciliation's crash recovery (design §4.2),
	// can both re-evaluate an already-audited round — a plain "task:round-N:
	// pdp-eval" invocation_id would collide with invocation_audit's unique
	// constraint on the second evaluation. Two real "pdp-decision" rows for
	// the same round is correct, not noise: PDP really was asked twice.
	pdpInvocationID := fmt.Sprintf("%s:round-%d:pdp-eval:%d", task.ID, round, time.Now().UnixNano())
	decision, err := deps.Policy.Evaluate(ctx, policyRequest(deps, task, round, pdpInvocationID))
	if err != nil {
		return RoundResult{}, fmt.Errorf("round %d: policy evaluate: %w", round, err)
	}

	if err := auditPDPDecision(ctx, deps.Audit, task, round, pdpInvocationID, decision); err != nil {
		return RoundResult{}, fmt.Errorf("round %d: audit PDP decision: %w", round, err)
	}

	switch decision.Decision {
	case "deny":
		return RoundResult{Outcome: OutcomeStopEscalate, Reason: "policy denied: " + decision.Reason}, nil
	case "allow":
		// proceed to fan-out below
	default:
		return RoundResult{}, fmt.Errorf("round %d: unknown policy decision %q", round, decision.Decision)
	}

	groups, err := groupStepsByPhase(decision.ResolvedSteps)
	if err != nil {
		return RoundResult{Outcome: OutcomeStopEscalate, Reason: "invalid phase configuration: " + err.Error()}, nil
	}

	var runs []domain.StepRun
	for _, phase := range []string{policyclient.PhaseGenerate, policyclient.PhaseReview} {
		steps := groups[phase]
		if len(steps) == 0 {
			continue
		}
		var pf []byte
		if phase == policyclient.PhaseGenerate {
			pf = priorFindings
		}
		phaseRuns, err := fanOut(ctx, deps, task, round, steps, decision.GrantedScopes, pf, alreadyCompleted)
		if err != nil {
			return RoundResult{}, fmt.Errorf("round %d: fan out (%s phase): %w", round, phase, err)
		}
		runs = append(runs, phaseRuns...)
	}

	if aggregate(runs) {
		findings, err := marshalFindings(runs)
		if err != nil {
			return RoundResult{}, fmt.Errorf("round %d: marshal prior findings: %w", round, err)
		}
		return RoundResult{Outcome: OutcomeContinue, StepRuns: runs, PriorFindings: findings}, nil
	}

	// No outstanding blocking findings. Before publish, an approve-phase
	// gate (design §4.4) — if this round resolved one — must already have a
	// verified approval; otherwise open (or reuse) the approval issue and
	// pause here, resumed for this same round once the webhook handler
	// verifies one.
	if approveSteps := groups[policyclient.PhaseApprove]; len(approveSteps) > 0 {
		res, err := deps.Approval.VerifyApproval(ctx, task.ID)
		if err != nil {
			return RoundResult{}, fmt.Errorf("round %d: verify approval: %w", round, err)
		}
		if !res.Approved {
			issue, err := deps.IssueOpener.CreateOrReuseIssue(ctx, approvalRepoRef(task), approvalIssueTitle(task, round), approvalIssueBody(task, round))
			if err != nil {
				return RoundResult{}, fmt.Errorf("round %d: open approval issue: %w", round, err)
			}
			return RoundResult{Outcome: OutcomeWaitingApproval, StepRuns: runs, ApprovalIssueNumber: issue.Number}, nil
		}
	}

	if publishSteps := groups[policyclient.PhasePublish]; len(publishSteps) > 0 {
		phaseRuns, err := fanOut(ctx, deps, task, round, publishSteps, decision.GrantedScopes, nil, alreadyCompleted)
		if err != nil {
			return RoundResult{}, fmt.Errorf("round %d: fan out (%s phase): %w", round, policyclient.PhasePublish, err)
		}
		runs = append(runs, phaseRuns...)
	}

	return RoundResult{Outcome: OutcomeStopSuccess, StepRuns: runs}, nil
}

// approvalRepoRef splits task.TargetRepo ("owner/name") into a
// gitclient.RepoRef — coreloop only ever sees TargetRepo as a plain string
// (it's also PDP's resource ID and the audit trail's own field), so this
// stays a local, throwaway parse rather than changing domain.Task's shape.
func approvalRepoRef(task domain.Task) gitclient.RepoRef {
	owner, name, _ := strings.Cut(task.TargetRepo, "/")
	return gitclient.RepoRef{Owner: owner, Name: name}
}

// approvalIssueTitle is deliberately stable across repeated calls for the
// same (task, round) — CreateOrReuseIssue matches on exact title, so this is
// what makes reopening the issue after a crash idempotent instead of
// duplicating it.
func approvalIssueTitle(task domain.Task, round int) string {
	return fmt.Sprintf("[approval] task %s round %d", task.ID, round)
}

func approvalIssueBody(task domain.Task, round int) string {
	return fmt.Sprintf(
		"Round %d of task `%s` (PR #%d on %s) needs human approval before it can publish.\n\n"+
			"Label this issue `approved` to continue, or `rejected` to escalate the task.",
		round, task.ID, task.PRNumber, task.TargetRepo,
	)
}

func policyRequest(deps Dependencies, task domain.Task, round int, invocationID string) policyclient.PolicyRequest {
	return policyclient.PolicyRequest{
		InvocationID:           invocationID,
		TaskID:                 task.ID,
		TriggeringPrincipal:    task.TriggeringPrincipal,
		OrchestratorInstanceID: deps.OrchestratorInstanceID,
		BotAccount:             task.BotAccount,
		TargetRepo:             task.TargetRepo,
		TaskType:               task.Type,
		RiskTier:               task.RiskTier,
		Facts:                  map[string]any{"round": round},
	}
}

// auditPDPDecision writes one "pdp-decision" audit row per round, whether
// or not it results in any launches — this is the only place a deny or an
// unapproved require_human_approval round leaves an audit trail at all.
func auditPDPDecision(ctx context.Context, w audit.AuditWriter, task domain.Task, round int, invocationID string, decision policyclient.PolicyDecision) error {
	resolvedStepsJSON, err := json.Marshal(decision.ResolvedSteps)
	if err != nil {
		return fmt.Errorf("marshal resolved_steps: %w", err)
	}
	return w.WriteInvocationRecord(ctx, audit.InvocationAuditRecord{
		InvocationID:        invocationID,
		TaskID:              task.ID,
		Round:               round,
		StepType:            "pdp-decision",
		BotAccount:          task.BotAccount,
		TargetRepo:          task.TargetRepo,
		PolicyDecision:      decision.Decision,
		GrantedScopes:       decision.GrantedScopes,
		PolicyBundleVersion: decision.PolicyBundleVersion,
		ResolvedStepsJSON:   resolvedStepsJSON,
	})
}

// groupStepsByPhase buckets a round's resolved steps by phase (defaulting
// unset/empty phases to "review", normalized onto the bucketed copy so no
// downstream consumer needs to re-derive the default) and enforces §4.1's
// cardinality rules: at most one generate step, at most one publish step.
// approve has no cardinality rule (§4.1: "not a step count") — RunRound only
// ever checks whether the group is non-empty, never dispatches its members
// through StepExecutor.
func groupStepsByPhase(steps []policyclient.ResolvedStep) (map[string][]policyclient.ResolvedStep, error) {
	groups := make(map[string][]policyclient.ResolvedStep)
	for _, s := range steps {
		phase := s.Phase
		if phase == "" {
			phase = policyclient.PhaseReview
		}
		switch phase {
		case policyclient.PhaseGenerate, policyclient.PhaseReview, policyclient.PhaseApprove, policyclient.PhasePublish:
		default:
			return nil, fmt.Errorf("resolved step %q has unknown phase %q", s.StepID, phase)
		}
		s.Phase = phase
		groups[phase] = append(groups[phase], s)
	}
	if n := len(groups[policyclient.PhaseGenerate]); n > 1 {
		return nil, fmt.Errorf("at most one generate-phase step per round, got %d", n)
	}
	if n := len(groups[policyclient.PhasePublish]); n > 1 {
		return nil, fmt.Errorf("at most one publish-phase step per round, got %d", n)
	}
	return groups, nil
}

// fanOut dispatches one phase group's steps concurrently through the
// registry and waits for all of them to finish. priorFindings is passed
// through to every step's StepContext as-is — callers only supply it for
// the generate phase (§4.1). A step present in alreadyCompleted is not
// re-dispatched (and not re-recorded) — its cached domain.StepRun is
// reused as-is.
func fanOut(ctx context.Context, deps Dependencies, task domain.Task, round int, steps []policyclient.ResolvedStep, grantedScopes []string, priorFindings []byte, alreadyCompleted map[string]domain.StepRun) ([]domain.StepRun, error) {
	runs := make([]domain.StepRun, len(steps))
	errs := make([]error, len(steps))

	var wg sync.WaitGroup
	for i, step := range steps {
		if cached, ok := alreadyCompleted[step.StepID]; ok {
			runs[i] = cached
			continue
		}

		wg.Add(1)
		go func(i int, step policyclient.ResolvedStep) {
			defer wg.Done()
			exec, ok := deps.Executors.Lookup(step.Type)
			if !ok {
				errs[i] = fmt.Errorf("no executor registered for step type %q", step.Type)
				return
			}

			invocationID := executor.InvocationID(executor.StepContext{Task: task, Round: round, Step: step})
			if deps.Recorder != nil {
				if err := deps.Recorder.RecordLaunched(ctx, task.ID, round, step, invocationID); err != nil {
					errs[i] = fmt.Errorf("record launched step_run for %s: %w", step.StepID, err)
					return
				}
			}

			run, err := exec.Execute(ctx, executor.StepContext{
				Task: task, Round: round, Step: step, GrantedScopes: grantedScopes, PriorFindings: priorFindings,
			})

			if deps.Recorder != nil {
				if rerr := deps.Recorder.RecordCompleted(ctx, task.ID, invocationID, run, err); rerr != nil && err == nil {
					err = fmt.Errorf("record completed step_run for %s: %w", step.StepID, rerr)
				}
			}
			if err != nil {
				errs[i] = err
				return
			}
			runs[i] = run
		}(i, step)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return runs, nil
}

// findingSummary is PRIOR_FINDINGS_JSON's wire shape — untrusted content
// fed into the next round's generate-phase env (design §7): a coding agent
// should treat it as review output to act on, not as trusted instruction.
type findingSummary struct {
	StepID           string          `json:"step_id"`
	Blocking         bool            `json:"blocking"`
	StructuredResult json.RawMessage `json:"structured_result,omitempty"`
}

// marshalFindings collects every StepRun that reported a finding this round
// (blocking or advisory) into PRIOR_FINDINGS_JSON for the next round's
// generate-phase dispatch.
func marshalFindings(runs []domain.StepRun) ([]byte, error) {
	var findings []findingSummary
	for _, r := range runs {
		if !r.HasFinding {
			continue
		}
		findings = append(findings, findingSummary{
			StepID: r.StepID, Blocking: r.Blocking, StructuredResult: r.StructuredResult,
		})
	}
	return json.Marshal(findings)
}

// aggregate combines step outcomes into a single blocking/non-blocking
// signal for the evaluate transition: a round has outstanding blocking
// findings iff any Blocking step reported one. Non-blocking (advisory)
// findings never gate continuation.
func aggregate(runs []domain.StepRun) bool {
	for _, r := range runs {
		if r.Blocking && r.HasFinding {
			return true
		}
	}
	return false
}
