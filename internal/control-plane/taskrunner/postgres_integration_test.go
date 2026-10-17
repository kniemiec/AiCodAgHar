//go:build integration

package taskrunner_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/taskrunner"
)

// ORCHESTRATOR_DB_DSN (orchestrator_app) drives the store tests.
// ORCHESTRATOR_ADMIN_DB_DSN (a superuser/owner DSN) is only needed to replay
// migration 006 and is optional.

func newPGStore(t *testing.T) *taskrunner.PostgresTaskStore {
	t.Helper()
	dsn := os.Getenv("ORCHESTRATOR_DB_DSN")
	if dsn == "" {
		t.Skip("ORCHESTRATOR_DB_DSN not set; source deploy/.env first")
	}
	s, err := taskrunner.NewPostgresTaskStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func adminDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ORCHESTRATOR_ADMIN_DB_DSN")
	if dsn == "" {
		t.Skip("ORCHESTRATOR_ADMIN_DB_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func uniqueID(t *testing.T, label string) string {
	return fmt.Sprintf("it-%s-%d-%d", label, os.Getpid(), time.Now().UnixNano())
}

func eventTypes(events []domain.TaskEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// TS-3: a failed event INSERT rolls the state change back. The event is made
// to fail with a foreign-key violation (it names a task that doesn't exist).
func TestPG_EventInsertFailureRollsBackState(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()
	id := uniqueID(t, "atomic")
	if err := s.CreateTask(ctx, domain.Task{ID: id, Type: "new-task"}); err != nil {
		t.Fatal(err)
	}

	err := s.UpdateTaskState(ctx, domain.TaskState{TaskID: id, Status: domain.TaskStatusStopSuccess, CurrentRound: 1},
		domain.TaskEvent{TaskID: "no-such-task", Type: "x"})
	if err == nil {
		t.Fatal("expected the event insert to fail")
	}
	rec := mustGet(t, s, id)
	if rec.State.Status != domain.TaskStatusRunning || rec.State.CurrentRound != 0 || rec.State.FinishedAt != nil {
		t.Fatalf("state changed although its event failed: %+v", rec.State)
	}

	// Same for a step row: launching with a bad round FK leaves no step_run and no event.
	if err := s.RecordStepLaunched(ctx, id, 9, policyclient.ResolvedStep{StepID: "s", Type: "agent-stage"}, id+"-r9-s"); err == nil {
		t.Fatal("expected RecordStepLaunched without a round to fail")
	}
	events, _ := s.ListEvents(ctx, id, domain.EventFilter{})
	if got := eventTypes(events); len(got) != 1 || got[0] != domain.EventTaskAccepted {
		t.Fatalf("events = %v, want only task.accepted", got)
	}
}

// ADR-12 §3: the application role can append and read events, never rewrite them.
func TestPG_TaskEventsAreAppendOnlyForAppRole(t *testing.T) {
	dsn := os.Getenv("ORCHESTRATOR_DB_DSN")
	if dsn == "" {
		t.Skip("ORCHESTRATOR_DB_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `UPDATE orchestrator.task_events SET type = 'tampered'`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("UPDATE task_events = %v, want permission denied", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM orchestrator.task_events`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("DELETE task_events = %v, want permission denied", err)
	}
}

// One task through every store write, checking the events each one derives.
func TestPG_LifecycleEventsAndRetry(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()
	id := uniqueID(t, "life")
	task := domain.Task{ID: id, Type: "new-task", TargetRepo: "acme/widgets", RiskTier: "low", BaseBranch: "main"}

	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, task); err == nil {
		t.Fatal("duplicate CreateTask should fail")
	}
	if created, err := s.EnsureRound(ctx, id, 1); err != nil || !created {
		t.Fatalf("first EnsureRound = %v, %v", created, err)
	}
	if created, err := s.EnsureRound(ctx, id, 1); err != nil || created {
		t.Fatalf("second EnsureRound = %v, %v; want created=false", created, err)
	}
	step := policyclient.ResolvedStep{StepID: "review", Type: "agent-stage", Phase: "review", Blocking: true}
	inv := id + "-r1-review"
	if err := s.RecordStepLaunched(ctx, id, 1, step, inv); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordStepLaunched(ctx, id, 1, step, inv); err != nil { // idempotent: no second event
		t.Fatal(err)
	}
	if err := s.CompleteStepRun(ctx, id, inv, domain.StepRun{ExitCode: 0, HasFinding: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvents(ctx, domain.TaskEvent{TaskID: id, Round: 1, Type: domain.EventStepHeartbeat, Payload: map[string]any{"elapsed_s": 300}}); err != nil {
		t.Fatal(err)
	}

	fail := domain.FailureRecord{TaskID: id, Round: 1, Class: "dependency", Retryable: true, Reason: "pdp down"}
	if err := s.FailTask(ctx, fail, domain.TaskEvent{TaskID: id, Round: 1, Type: domain.EventTaskFailed}); err != nil {
		t.Fatal(err)
	}
	rec := mustGet(t, s, id)
	if rec.State.Status != domain.TaskStatusFailed || rec.State.ErrorClass != "dependency" || !rec.State.Retryable || rec.State.FinishedAt != nil || rec.State.AcceptedAt.IsZero() {
		t.Fatalf("after FailTask: %+v", rec.State)
	}
	if ids, _ := s.ListRunningTaskIDs(ctx); contains(ids, id) {
		t.Fatal("failed task listed as running")
	}
	if ages, err := s.ListTasksByStatus(ctx, domain.TaskStatusFailed); err != nil || !hasTask(ages, id) {
		t.Fatalf("ListTasksByStatus(failed) = %v, %v", ages, err)
	}

	// Concurrent retries: the compare-and-set lets exactly one through.
	var wins, losses atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RetryTask(ctx, id, 3, domain.TaskEvent{TaskID: id, Round: 2, Type: domain.EventTaskRetried, Payload: map[string]any{"triggered_by": "it"}})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, domain.ErrNotRetryable):
				losses.Add(1)
			default:
				t.Errorf("RetryTask: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 5 {
		t.Fatalf("retry winners/losers = %d/%d, want 1/5", wins.Load(), losses.Load())
	}
	rec = mustGet(t, s, id)
	if rec.State.Status != domain.TaskStatusRunning || rec.State.RetryCount != 1 || rec.State.ErrorClass != "" || rec.State.Reason != "" {
		t.Fatalf("after retry: %+v", rec.State)
	}

	// Terminal state stamps finished_at; terminal failure does too.
	if err := s.UpdateTaskState(ctx, domain.TaskState{TaskID: id, Status: domain.TaskStatusStopSuccess, CurrentRound: 1}, domain.TaskEvent{TaskID: id, Type: domain.EventTaskFinished}); err != nil {
		t.Fatal(err)
	}
	rec = mustGet(t, s, id)
	if rec.State.FinishedAt == nil {
		t.Fatal("finished_at not stamped on stop_success")
	}

	events, err := s.ListEvents(ctx, id, domain.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"task.accepted", "round.started", "step.launched", "step.resolved", "step.heartbeat", "task.failed", "task.retried", "task.finished"}
	got := eventTypes(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	resolved := events[3]
	if resolved.StepID != "review" || resolved.InvocationID != inv || resolved.Round != 1 || resolved.Payload["outcome"] != "completed" {
		t.Fatalf("step.resolved = %+v", resolved)
	}
	if _, ok := resolved.Payload["duration_ms"]; !ok {
		t.Fatal("step.resolved lacks duration_ms")
	}
	if events[6].Payload["previous_class"] != "dependency" {
		t.Fatalf("task.retried payload = %v", events[6].Payload)
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID || events[i].At.Before(events[i-1].At) {
			t.Fatalf("events not ordered at %d", i)
		}
	}

	// Filters.
	if byType, _ := s.ListEvents(ctx, id, domain.EventFilter{Type: domain.EventStepHeartbeat}); len(byType) != 1 {
		t.Fatalf("type filter returned %d", len(byType))
	}
	if page, _ := s.ListEvents(ctx, id, domain.EventFilter{AfterID: events[2].ID, Limit: 2}); len(page) != 2 || page[0].ID != events[3].ID {
		t.Fatalf("after_id/limit page = %v", eventTypes(page))
	}
	if none, _ := s.ListEvents(ctx, id, domain.EventFilter{Since: time.Now().Add(time.Hour)}); len(none) != 0 {
		t.Fatalf("since filter returned %d", len(none))
	}

	// Non-retryable and exhausted failures.
	id2 := uniqueID(t, "term")
	if err := s.CreateTask(ctx, domain.Task{ID: id2, Type: "new-task"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailTask(ctx, domain.FailureRecord{TaskID: id2, Class: "platform", Retryable: false, Reason: "bug"}); err != nil {
		t.Fatal(err)
	}
	rec = mustGet(t, s, id2)
	if rec.State.FinishedAt == nil {
		t.Fatal("terminal failure should stamp finished_at")
	}
	if _, err := s.RetryTask(ctx, id2, 3, domain.TaskEvent{TaskID: id2, Type: domain.EventTaskRetried}); !errors.Is(err, domain.ErrNotRetryable) {
		t.Fatalf("retry of terminal failure = %v", err)
	}
}

// TS-7: migration 006 converts legacy rows, gives every task a timeline, and
// is re-runnable. Legacy rows are emulated by writing them (and removing
// their migrated state) through the admin connection.
func TestPG_Migration006(t *testing.T) {
	db := adminDB(t)
	ctx := context.Background()
	sqlText, err := os.ReadFile("../../../deploy/migrations/006_task_events_and_failure.sql")
	if err != nil {
		t.Fatal(err)
	}

	prefix := uniqueID(t, "mig")
	seed := []struct{ id, status, reason string }{
		{prefix + "-failed", "running", "error: round 1: boom"},
		{prefix + "-plainrun", "running", ""},
		{prefix + "-ok", "stop_success", ""},
		{prefix + "-esc", "stop_escalate", "policy denied: no"},
	}
	for _, r := range seed {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO orchestrator.tasks (id, type, target_repo, triggering_principal, bot_account, risk_tier, branch, base_branch, status, reason, current_round)
			VALUES ($1,'new-task','a/b','p','bot','low','br','main',$2,NULLIF($3,''),2)`, r.id, r.status, r.reason); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	for run := 1; run <= 2; run++ {
		if _, err := db.ExecContext(ctx, string(sqlText)); err != nil {
			t.Fatalf("run %d of 006: %v", run, err)
		}
	}

	status := func(id string) (st string, class sql.NullString, retryable sql.NullBool, finished sql.NullTime) {
		if err := db.QueryRowContext(ctx, `SELECT status, error_class, retryable, finished_at FROM orchestrator.tasks WHERE id=$1`, id).
			Scan(&st, &class, &retryable, &finished); err != nil {
			t.Fatal(err)
		}
		return
	}
	if st, class, retryable, _ := status(prefix + "-failed"); st != "failed" || class.String != "platform" || !retryable.Bool {
		t.Fatalf("legacy error task = %s/%v/%v", st, class, retryable)
	}
	if st, _, _, _ := status(prefix + "-plainrun"); st != "running" {
		t.Fatalf("plain running task became %s", st)
	}
	for _, id := range []string{prefix + "-ok", prefix + "-esc"} {
		if _, _, _, fin := status(id); !fin.Valid {
			t.Fatalf("%s: finished_at not backfilled", id)
		}
	}
	for _, r := range seed {
		var n int
		var payload string
		if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(payload::text),'') FROM orchestrator.task_events WHERE task_id=$1 AND type='task.migrated'`, r.id).Scan(&n, &payload); err != nil {
			t.Fatal(err)
		}
		if n != 1 || !strings.Contains(payload, `"history_recorded": false`) {
			t.Fatalf("%s: task.migrated count=%d payload=%s", r.id, n, payload)
		}
	}
	// CHECK constraint is live.
	if _, err := db.ExecContext(ctx, `UPDATE orchestrator.tasks SET status='bogus' WHERE id=$1`, prefix+"-ok"); err == nil {
		t.Fatal("status CHECK constraint not enforced")
	}
}

// Design §6: the observer sees exactly the committed events (and nothing
// from a rolled-back write), step.resolved carries the row's step_type, and
// FleetSnapshot counts this task in the right buckets.
func TestPG_EventObserverAndFleetSnapshot(t *testing.T) {
	s := newPGStore(t)
	var mu sync.Mutex
	var seen []domain.TaskEvent
	s.SetEventObserver(func(_ context.Context, evs []domain.TaskEvent) {
		mu.Lock()
		seen = append(seen, evs...)
		mu.Unlock()
	})
	ctx := context.Background()
	id := uniqueID(t, "fleet")
	if err := s.CreateTask(ctx, domain.Task{ID: id, Type: "new-task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureRound(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	inv := id + "-r1-review"
	if err := s.RecordStepLaunched(ctx, id, 1, policyclient.ResolvedStep{StepID: "review", Type: "agent-stage"}, inv); err != nil {
		t.Fatal(err)
	}
	run := domain.StepRun{Launch: domain.LaunchInfo{Outcome: "completed", QueueWait: 2 * time.Second}}
	if err := s.CompleteStepRun(ctx, id, inv, run, nil); err != nil {
		t.Fatal(err)
	}
	// A rolled-back write must not reach the observer.
	_ = s.UpdateTaskState(ctx, domain.TaskState{TaskID: id, Status: domain.TaskStatusStopSuccess}, domain.TaskEvent{TaskID: "no-such-task", Type: "x"})

	mu.Lock()
	got := eventTypes(seen)
	resolved := seen[len(seen)-1]
	mu.Unlock()
	want := []string{domain.EventTaskAccepted, domain.EventRoundStarted, domain.EventStepLaunched, domain.EventStepResolved}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("observed %v, want %v", got, want)
	}
	if resolved.Payload["step_type"] != "agent-stage" || resolved.Payload["launcher_outcome"] != "completed" || resolved.Payload["queue_wait_ms"] != int64(2000) {
		t.Errorf("step.resolved payload = %v", resolved.Payload)
	}

	snap, err := s.FleetSnapshot(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ByStatus[domain.TaskStatusRunning] < 1 || snap.Stuck < 1 {
		t.Errorf("running task not counted (stuckAfter=0): %+v", snap)
	}
	issue := 1
	if err := s.UpdateTaskState(ctx, domain.TaskState{TaskID: id, Status: domain.TaskStatusWaitingApproval, ApprovalIssueNumber: &issue},
		domain.TaskEvent{TaskID: id, Round: 1, Type: domain.EventApprovalRequested}); err != nil {
		t.Fatal(err)
	}
	snap, err = s.FleetSnapshot(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if snap.WaitingApprovals < 1 || snap.OldestApprovalWait < 0 {
		t.Errorf("waiting task not counted: %+v", snap)
	}
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func hasTask(ages []domain.TaskAge, id string) bool {
	for _, a := range ages {
		if a.TaskID == id {
			return true
		}
	}
	return false
}

func mustGet(t *testing.T, s *taskrunner.PostgresTaskStore, id string) domain.TaskRecord {
	t.Helper()
	rec, ok, err := s.GetTask(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetTask(%s): ok=%v err=%v", id, ok, err)
	}
	return rec
}
