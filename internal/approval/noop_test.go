package approval_test

import (
	"context"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/approval"
)

func TestNoopApprovalVerifier_AlwaysFailsClosed(t *testing.T) {
	v := approval.NoopApprovalVerifier{}
	res, err := v.VerifyApproval(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("VerifyApproval: %v", err)
	}
	if res.Approved {
		t.Fatal("expected NoopApprovalVerifier to always report Approved=false")
	}
}
