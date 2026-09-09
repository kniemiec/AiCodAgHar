package fakes

import (
	"context"
	"sync"
	"time"

	"github.com/kniemiec/AiCodAgHar/internal/clients/launcherclient"
)

// FakeContainerLauncher is an in-memory ContainerLauncher. By default Launch
// succeeds with exit code 0 and ExtractLogs returns an empty bundle; set
// LaunchFunc / ExtractLogsFunc to script specific outcomes (e.g. a
// structured result that always reports a blocking finding).
type FakeContainerLauncher struct {
	LaunchFunc      func(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error)
	ExtractLogsFunc func(ctx context.Context, invocationID string) (launcherclient.LogBundle, error)

	mu       sync.Mutex
	launches []launcherclient.LaunchRequest
	killed   []string
}

func (f *FakeContainerLauncher) Launch(ctx context.Context, req launcherclient.LaunchRequest) (launcherclient.RunResult, error) {
	f.mu.Lock()
	f.launches = append(f.launches, req)
	f.mu.Unlock()

	if f.LaunchFunc != nil {
		return f.LaunchFunc(ctx, req)
	}
	now := time.Now()
	return launcherclient.RunResult{
		InvocationID: req.InvocationID,
		ExitCode:     0,
		Status:       "completed",
		StartedAt:    now,
		FinishedAt:   now,
	}, nil
}

func (f *FakeContainerLauncher) Kill(ctx context.Context, invocationID string) error {
	f.mu.Lock()
	f.killed = append(f.killed, invocationID)
	f.mu.Unlock()
	return nil
}

func (f *FakeContainerLauncher) ExtractLogs(ctx context.Context, invocationID string) (launcherclient.LogBundle, error) {
	if f.ExtractLogsFunc != nil {
		return f.ExtractLogsFunc(ctx, invocationID)
	}
	return launcherclient.LogBundle{}, nil
}

// Launches returns every LaunchRequest Launch has been called with, in order.
func (f *FakeContainerLauncher) Launches() []launcherclient.LaunchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]launcherclient.LaunchRequest, len(f.launches))
	copy(out, f.launches)
	return out
}
