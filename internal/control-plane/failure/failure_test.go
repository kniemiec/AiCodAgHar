package failure

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	base := errors.New("boom")
	cases := []struct {
		name      string
		err       error
		class     Class
		retryable bool
		dep       string
	}{
		{"unclassified defaults to platform", base, ClassPlatform, false, ""},
		{"nil defaults to platform", nil, ClassPlatform, false, ""},
		{"dependency", Dependency(DepPDP, base), ClassDependency, true, DepPDP},
		{"infrastructure", Infrastructure(base), ClassInfrastructure, true, ""},
		{"agent", Agent(base), ClassAgent, false, ""},
		{"platform", Platform(base), ClassPlatform, false, ""},
		{"wrapped with %w", fmt.Errorf("round 1: %w", Dependency(DepGitHost, base)), ClassDependency, true, DepGitHost},
		{"outermost wins", Platform(Dependency(DepPDP, base)), ClassPlatform, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, retryable, dep := Classify(tc.err)
			if class != tc.class || retryable != tc.retryable || dep != tc.dep {
				t.Fatalf("Classify = (%s,%v,%q), want (%s,%v,%q)", class, retryable, dep, tc.class, tc.retryable, tc.dep)
			}
		})
	}
}

func TestNewNilStaysNil(t *testing.T) {
	if Dependency(DepPDP, nil) != nil || Platform(nil) != nil {
		t.Fatal("wrapping nil must stay nil")
	}
}

func TestUnwrapKeepsChain(t *testing.T) {
	base := errors.New("boom")
	if !errors.Is(Dependency(DepPDP, base), base) {
		t.Fatal("errors.Is should see through ClassifiedError")
	}
}

func TestBound(t *testing.T) {
	if got := Bound("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", 400) // 800 bytes
	got := Bound(long)
	if len(got) > MaxReasonBytes {
		t.Fatalf("len = %d", len(got))
	}
	if strings.ContainsRune(got, '�') || len(got)%2 != 0 {
		t.Fatalf("split a rune: %d bytes", len(got))
	}
}
