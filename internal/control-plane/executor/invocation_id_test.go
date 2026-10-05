package executor

import (
	"regexp"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/control-plane/clients/policyclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
)

// launcherInvocationIDPattern mirrors cmd/container-launcher/server.go's
// validInvocationID (and internal/launcher/jailer's validID). launcher-mock
// doesn't validate IDs, so without this test a format the real launcher
// rejects only shows up on a KVM host.
var launcherInvocationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func TestInvocationID_AcceptedByRealLauncher(t *testing.T) {
	for _, stepID := range []string{"implement", "review-with-commit", "unit-tests"} {
		id := InvocationID(StepContext{
			Task:  domain.Task{ID: "task-1790937270261484182"},
			Round: 5,
			Step:  policyclient.ResolvedStep{StepID: stepID},
		})
		if !launcherInvocationIDPattern.MatchString(id) {
			t.Errorf("InvocationID = %q, which the real container-launcher rejects (must match %s)", id, launcherInvocationIDPattern)
		}
	}
}
