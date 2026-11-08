// Package failure is the leaf package that gives every task-ending error a
// class and a retryability (platform-observability design §2): clients,
// executors, coreloop and taskrunner all import it, so it imports nothing
// from internal/control-plane.
package failure

import "errors"

// Class says whose fault a failure is.
type Class string

const (
	ClassPlatform       Class = "platform"       // our own bug or persistence fault; terminal
	ClassInfrastructure Class = "infrastructure" // VM/host fault; retryable
	ClassDependency     Class = "dependency"     // a called service failed; retryable
	ClassAgent          Class = "agent"          // the agent/guest misbehaved; terminal
)

// Dependency names for ClassifiedError.Dependency (bounded metric label set).
const (
	DepPDP           = "pdp"
	DepGitHost       = "git_host"
	DepSecretsIssuer = "secrets_issuer"
	DepLauncher      = "launcher"
	DepPostgres      = "postgres"
)

// MaxReasonBytes bounds a persisted/logged failure reason (SEC-4).
const MaxReasonBytes = 512

// ClassifiedError attaches a Class (and optionally the failing dependency)
// to an error while keeping the chain intact for errors.Is/As.
type ClassifiedError struct {
	Class      Class
	Dependency string
	Err        error
}

func (e *ClassifiedError) Error() string { return e.Err.Error() }
func (e *ClassifiedError) Unwrap() error { return e.Err }

// New wraps err with class. A nil err stays nil.
func New(class Class, err error) error {
	if err == nil {
		return nil
	}
	return &ClassifiedError{Class: class, Err: err}
}

// Dependency wraps err as a ClassDependency failure of dep.
func Dependency(dep string, err error) error {
	if err == nil {
		return nil
	}
	return &ClassifiedError{Class: ClassDependency, Dependency: dep, Err: err}
}

// Platform wraps err as a ClassPlatform failure.
func Platform(err error) error { return New(ClassPlatform, err) }

// Infrastructure wraps err as a ClassInfrastructure failure.
func Infrastructure(err error) error { return New(ClassInfrastructure, err) }

// Agent wraps err as a ClassAgent failure.
func Agent(err error) error { return New(ClassAgent, err) }

// Retryable is the fixed class -> retryable mapping.
func (c Class) Retryable() bool {
	return c == ClassDependency || c == ClassInfrastructure
}

// Classify returns the class of the outermost ClassifiedError in err's chain.
// Unclassified errors are platform/terminal: nothing is ever "unknown".
func Classify(err error) (class Class, retryable bool, dependency string) {
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Class, ce.Class.Retryable(), ce.Dependency
	}
	return ClassPlatform, false, ""
}

// Bound truncates s to MaxReasonBytes on a rune boundary.
func Bound(s string) string {
	if len(s) <= MaxReasonBytes {
		return s
	}
	cut := MaxReasonBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 { // don't split a UTF-8 sequence
		cut--
	}
	return s[:cut]
}
