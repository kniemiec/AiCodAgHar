package taskrunner_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/launcherclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// recordingObserver captures taskrunner.Observer calls.
type recordingObserver struct {
	mu          sync.Mutex
	reconciled  []string
	persistence []string
}

func (o *recordingObserver) Reconciled(_ context.Context, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reconciled = append(o.reconciled, outcome)
}

func (o *recordingObserver) PersistenceFailure(_ context.Context, component string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.persistence = append(o.persistence, component)
}

// TestRunner_ReportsReconcileOutcomes covers MT-10's outcomes: a task that
// isn't running is a noop, a running one is resumed, and one whose launched
// step can't be re-queried is failed.
func TestRunner_ReportsReconcileOutcomes(t *testing.T) {
	ctx := context.Background()
	review := agentStep("review", policyclient.PhaseReview)

	t.Run("noop and resumed", func(t *testing.T) {
		obs := &recordingObserver{}
		e := newEnv(review)
		e.observer = obs
		r := e.runner()
		if _, err := r.ReconcileTask(ctx, "no-such-task"); err != nil {
			t.Fatal(err)
		}
		if err := e.store.CreateTask(ctx, domain.Task{ID: "task-1", TargetRepo: "acme/widgets"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ReconcileTask(ctx, "task-1"); err != nil {
			t.Fatalf("ReconcileTask: %v", err)
		}
		if !slices.Equal(obs.reconciled, []string{"noop", "resumed"}) {
			t.Fatalf("reconciled = %v, want [noop resumed]", obs.reconciled)
		}
	})

	t.Run("failed", func(t *testing.T) {
		obs := &recordingObserver{}
		e := newEnv(review)
		e.observer = obs
		e.launcher.ExtractLogsFunc = func(context.Context, string) (launcherclient.LogBundle, error) {
			return launcherclient.LogBundle{}, errBoom
		}
		r := e.runner()
		if err := e.store.CreateTask(ctx, domain.Task{ID: "task-1", TargetRepo: "acme/widgets"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.EnsureRound(ctx, "task-1", 1); err != nil {
			t.Fatal(err)
		}
		if err := e.store.RecordStepLaunched(ctx, "task-1", 1, review, "task-1-r1-review"); err != nil {
			t.Fatal(err)
		}
		_, _ = r.ReconcileTask(ctx, "task-1")
		if !slices.Equal(obs.reconciled, []string{"failed"}) {
			t.Fatalf("reconciled = %v, want [failed]", obs.reconciled)
		}
	})
}

// TestRunner_ReportsUnpersistableFailure is ER-5: when the failure record
// itself can't be written, the runner reports a persistence failure (and the
// task stays running for reconciliation).
func TestRunner_ReportsUnpersistableFailure(t *testing.T) {
	obs := &recordingObserver{}
	e := newEnv()
	e.observer = obs
	e.policy.EvaluateFunc = func(context.Context, policyclient.PolicyRequest) (policyclient.PolicyDecision, error) {
		return policyclient.PolicyDecision{}, errBoom
	}
	e.store.failTask = func() error { return errBoom }
	res := e.start(t, e.runner())
	if res.Err == nil {
		t.Fatal("expected the round's error")
	}
	if !slices.Equal(obs.persistence, []string{"taskrunner"}) {
		t.Fatalf("persistence failures = %v, want [taskrunner]", obs.persistence)
	}
	rec, _, _ := e.store.GetTask(context.Background(), "task-1")
	if rec.State.Status != domain.TaskStatusRunning {
		t.Fatalf("status = %s, want running (left for reconciliation)", rec.State.Status)
	}
}
