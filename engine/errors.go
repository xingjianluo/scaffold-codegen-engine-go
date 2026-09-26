package engine

import "fmt"

// ErrorKind enumerates run-time failure categories. Plan-level problems
// (cycles, missing params, ...) keep their plan.IssueKind classification.
type ErrorKind string

const (
	KindRender      ErrorKind = "render_error"
	KindConflict    ErrorKind = "output_conflict"
	KindIO          ErrorKind = "io_error"
	KindInterrupted ErrorKind = "interrupted"
	KindLocked      ErrorKind = "workspace_locked"
)

// TaskError is the categorized failure of one generation task.
type TaskError struct {
	Target string
	Kind   ErrorKind
	Detail string
	Err    error
}

func (e *TaskError) Error() string {
	if e.Target != "" {
		return string(e.Kind) + ": " + e.Detail + " (target=" + e.Target + ")"
	}
	return string(e.Kind) + ": " + e.Detail
}

func (e *TaskError) Unwrap() error { return e.Err }

// wrap builds a TaskError.
func taskErr(kind ErrorKind, target, detail string, cause error) *TaskError {
	if cause != nil {
		detail = fmt.Sprintf("%s: %v", detail, cause)
	}
	return &TaskError{Kind: kind, Target: target, Detail: detail, Err: cause}
}
