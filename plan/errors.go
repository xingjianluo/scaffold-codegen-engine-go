package plan

import "strings"

// IssueKind enumerates the distinct, explainable reasons a plan can be
// rejected or a target cannot be handled. Every rejection carries one of
// these kinds so callers never have to parse free-text messages.
type IssueKind string

const (
	KindParse           IssueKind = "parse_error"
	KindDuplicateName   IssueKind = "duplicate_target_name"
	KindDuplicateOutput IssueKind = "output_path_conflict"
	KindUnknownDep      IssueKind = "unknown_dependency"
	KindCycle           IssueKind = "cyclic_dependency"
	KindMissingParam    IssueKind = "missing_parameter"
	KindUnknownParam    IssueKind = "unknown_parameter"
	KindEmptyPlan       IssueKind = "empty_plan"
	KindInvalidPath     IssueKind = "invalid_path"
	KindMissingTemplate IssueKind = "missing_template"
	KindTemplateError   IssueKind = "template_error"
)

// Issue describes one rejected aspect of a plan.
type Issue struct {
	Kind   IssueKind `json:"kind"`
	Target string    `json:"target,omitempty"`
	// Detail is the human-readable explanation.
	Detail string `json:"detail"`
}

func (i Issue) Error() string {
	if i.Target != "" {
		return string(i.Kind) + ": " + i.Detail + " (target=" + i.Target + ")"
	}
	return string(i.Kind) + ": " + i.Detail
}

// ValidationError bundles all issues found in one validation pass. Collecting
// every issue lets callers report all rejection reasons at once instead of
// failing on the first one.
type ValidationError struct {
	Issues []Issue `json:"issues"`
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Issues) == 0 {
		return "validation failed"
	}
	parts := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		parts[i] = is.Error()
	}
	return "plan validation failed: " + strings.Join(parts, "; ")
}

// Has reports whether an issue of the given kind is present.
func (e *ValidationError) Has(kind IssueKind) bool {
	if e == nil {
		return false
	}
	for _, is := range e.Issues {
		if is.Kind == kind {
			return true
		}
	}
	return false
}

// Add appends an issue.
func (e *ValidationError) Add(kind IssueKind, target, detail string) {
	e.Issues = append(e.Issues, Issue{Kind: kind, Target: target, Detail: detail})
}
