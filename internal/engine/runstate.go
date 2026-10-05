package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// RunPhase describes where a run last stopped.
type RunPhase string

const (
	// PhaseRunning: the run has started and has not reached a terminal phase.
	PhaseRunning RunPhase = "running"
	// PhaseSucceeded: every target reached a successful classification and the
	// manifest was committed.
	PhaseSucceeded RunPhase = "succeeded"
	// PhaseFailed: at least one target failed; rollback finished.
	PhaseFailed RunPhase = "failed"
	// PhaseInterrupted: the context was cancelled; rollback finished.
	PhaseInterrupted RunPhase = "interrupted"
	// PhaseRollingBack: a terminal trigger happened and rollback is in
	// progress; a stale record in this phase means the process died mid-cleanup.
	PhaseRollingBack RunPhase = "rolling_back"
)

// Fine-grained step names recorded per target, so a status query can show
// exactly where a target stopped.
const (
	StepPending   = "pending"
	StepRendering = "rendering"
	StepRendered  = "rendered"
	StepDecided   = "decided"
	StepWritten   = "written"
	StepTerminal  = "terminal"
)

// TargetState is the last recorded state of one target from the most recent
// run against this workspace.
type TargetState struct {
	Target    string `json:"target"`
	Path      string `json:"path"`
	Step      string `json:"step"`
	Status    Status `json:"status"`
	Reason    string `json:"reason,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// RunState is the durable record of the most recent run.
type RunState struct {
	Phase     RunPhase                `json:"phase"`
	PlanName  string                  `json:"planName,omitempty"`
	StartedAt string                  `json:"startedAt,omitempty"`
	EndedAt   string                  `json:"endedAt,omitempty"`
	PID       int                     `json:"pid,omitempty"`
	Host      string                  `json:"host,omitempty"`
	Targets   map[string]*TargetState `json:"targets"`
	Error     *ClassifiedErr          `json:"error,omitempty"`
	Order     []string                `json:"order,omitempty"`
}

// StateFileName is the run-state file name under .scaffold/.
const StateFileName = "state.json"

// stateRecorder serializes durable target updates within a run. Every update
// is flushed to disk (atomic rename), so an interrupted process always leaves
// a parseable state file showing the latest step each target reached.
type stateRecorder struct {
	mu   sync.Mutex
	dir  string
	name string
	st   *RunState
}

// nowUTC is indirected so tests never depend on the wall clock.
var nowUTC = func() time.Time { return time.Now().UTC() }

// runSeq makes run IDs unique and sortable within a process.
var runSeq int64

// runCounter returns a process-monotonic run sequence number.
func runCounter() int64 { return atomic.AddInt64(&runSeq, 1) }

// newRunState prepares a fresh "running" state for p, with every target at
// the pending step.
func newRunState(p *plan.Plan, order []*plan.Target, holder lockHolder) *RunState {
	st := &RunState{
		Phase:     PhaseRunning,
		PlanName:  p.NameSafe(),
		StartedAt: holder.StartedAt,
		PID:       holder.PID,
		Host:      holder.Host,
		Targets:   make(map[string]*TargetState, len(order)),
		Order:     make([]string, 0, len(order)),
	}
	if st.StartedAt == "" {
		st.StartedAt = nowUTC().Format(time.RFC3339Nano)
	}
	for _, t := range order {
		st.Order = append(st.Order, t.ID)
		st.Targets[t.ID] = &TargetState{Target: t.ID, Path: t.Path, Step: StepPending}
	}
	return st
}

// newStateRecorder wraps a RunState with durable persistence.
func newStateRecorder(dir, name string, st *RunState) *stateRecorder {
	return &stateRecorder{dir: dir, name: name, st: st}
}

// save writes the state atomically.
func (s *RunState) save(dir, name string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run state: %w", err)
	}
	if err := writeStateFile(filepath.Join(dir, name), data); err != nil {
		return fmt.Errorf("write run state: %w", err)
	}
	return nil
}

// loadRunState reads state.json; a missing file yields (nil, nil). A corrupt
// file is an explicit error so callers can classify it rather than silently
// report an empty history.
func loadRunState(dir, name string) (*RunState, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run state: %w", err)
	}
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse run state: %w", err)
	}
	if st.Targets == nil {
		st.Targets = map[string]*TargetState{}
	}
	return &st, nil
}

// record updates one target and persists the whole state. update.Target,
// update.Path and update.UpdatedAt are filled in when zero/empty.
func (r *stateRecorder) record(t *plan.Target, update TargetState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	update.Target = t.ID
	update.Path = t.Path
	if update.UpdatedAt == "" {
		update.UpdatedAt = nowUTC().Format(time.RFC3339Nano)
	}
	cp := update
	r.st.Targets[t.ID] = &cp
	return r.st.save(r.dir, r.name)
}

// finish marks the run terminal, records the terminal outcomes for any
// targets still missing one, and persists the state.
func (r *stateRecorder) finish(phase RunPhase, cerr *ClassifiedErr, finalResults []Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.Phase = phase
	r.st.EndedAt = nowUTC().Format(time.RFC3339Nano)
	r.st.Error = cerr
	for _, res := range finalResults {
		cur, ok := r.st.Targets[res.Target]
		if !ok {
			cur = &TargetState{Target: res.Target, Path: res.Path}
			r.st.Targets[res.Target] = cur
		}
		cur.Status = res.Status
		if res.Reason != "" {
			cur.Reason = res.Reason
		}
		if res.SHA256 != "" {
			cur.SHA256 = res.SHA256
		}
		if res.Attempt != 0 {
			cur.Attempt = res.Attempt
		}
		cur.Step = StepTerminal
		cur.UpdatedAt = r.st.EndedAt
	}
	return r.st.save(r.dir, r.name)
}
