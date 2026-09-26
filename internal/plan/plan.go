// Package plan defines the declarative generation plan and its validation.
package plan

import "strings"

// Plan is a declarative description of the files to generate.
type Plan struct {
	// Name identifies the plan; informational only.
	Name string `json:"name,omitempty"`
	// Params are user-supplied values referenced by templates as {{.params.KEY}}.
	Params map[string]string `json:"params,omitempty"`
	// Targets is the set of generation targets keyed by target ID.
	Targets map[string]*Target `json:"targets"`
}

// Target describes one output file.
type Target struct {
	// ID is the unique identifier of the target and the key in Plan.Targets.
	ID string `json:"id,omitempty"`
	// Path is the output path, relative to the run root. Forward slashes;
	// absolute paths and path traversal are rejected.
	Path string `json:"path"`
	// Template is the body rendered with Go text/template syntax.
	Template string `json:"template"`
	// Deps lists target IDs that must be rendered before this target.
	Deps []string `json:"deps,omitempty"`
	// RequiredParams lists parameter keys that must be present and non-empty.
	RequiredParams []string `json:"requiredParams,omitempty"`
	// Fail, when true, causes rendering to fail with ErrRender for tests and
	// exercises of partial-failure recovery.
	Fail bool `json:"fail,omitempty"`
}

// IssueCode classifies a plan validation problem.
type IssueCode string

const (
	IssueDuplicateID       IssueCode = "duplicate_id"
	IssueEmptyID           IssueCode = "empty_id"
	IssueEmptyPath         IssueCode = "empty_path"
	IssuePathAbsolute      IssueCode = "path_absolute"
	IssuePathTraversal     IssueCode = "path_traversal"
	IssuePathNotNormalized IssueCode = "path_not_normalized"
	IssuePathConflict      IssueCode = "path_conflict"
	IssueMissingDep        IssueCode = "missing_dep"
	IssueSelfDep           IssueCode = "self_dep"
	IssueCycle             IssueCode = "cycle"
	IssueMissingParam      IssueCode = "missing_param"
)

// Issue is one classified validation problem.
type Issue struct {
	Code    IssueCode `json:"code"`
	Target  string    `json:"target,omitempty"`
	Message string    `json:"message"`
}

// Issues is a collection of validation problems.
type Issues []Issue

func (is Issues) Error() string {
	if len(is) == 0 {
		return ""
	}
	parts := make([]string, len(is))
	for i, is := range is {
		if is.Target != "" {
			parts[i] = string(is.Code) + " [" + is.Target + "]: " + is.Message
		} else {
			parts[i] = string(is.Code) + ": " + is.Message
		}
	}
	return strings.Join(parts, "; ")
}

// NameSafe returns the plan name or a stable placeholder.
func (p *Plan) NameSafe() string {
	if p == nil || p.Name == "" {
		return "plan"
	}
	return p.Name
}
