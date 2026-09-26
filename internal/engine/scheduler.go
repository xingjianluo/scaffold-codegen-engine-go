package engine

import (
	"context"
	"sync"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// jobResult is the outcome of processing one target.
type jobResult struct {
	targetID string
	path     string
	status   Status
	reason   string
	attempt  int
}

// outcome pairs the per-target classification with the computed digest.
type outcome struct {
	status  Status
	reason  string
	attempt int
	sha     string
}

// pendingItem is a target that has become runnable. When skip is true the
// target is not executed (an upstream dependency failed) and the skip is
// propagated to its own dependents.
type pendingItem struct {
	id     string
	skip   bool
	reason string
}

// scheduler runs targets in dependency order with bounded concurrency. A
// target starts only after every dependency has reached a terminal state; if
// any dependency did not succeed (failed, skipped or cancelled), the target is
// itself marked skipped without running. All shared bookkeeping is guarded by
// one mutex; worker goroutines only call the supplied process function.
type scheduler struct {
	order   []*plan.Target
	workers int

	mu       sync.Mutex
	cond     *sync.Cond
	pending  []pendingItem // kept sorted by target ID
	inFlight int
	recorded map[string]jobResult
	shas     map[string]string // target ID -> rendered digest (even on failure)
	badDep   map[string]bool   // target has at least one failed/skipped dependency
	index    map[string]*plan.Target
	indeg    map[string]int
	children map[string][]string // dep -> dependents
}

// newScheduler builds a scheduler over the given topological order.
func newScheduler(order []*plan.Target, workers int) *scheduler {
	if workers < 1 {
		workers = 1
	}
	if workers > len(order) {
		workers = len(order)
	}
	s := &scheduler{
		order:    order,
		workers:  workers,
		recorded: make(map[string]jobResult, len(order)),
		shas:     make(map[string]string, len(order)),
		badDep:   make(map[string]bool),
		index:    make(map[string]*plan.Target, len(order)),
		indeg:    make(map[string]int, len(order)),
		children: make(map[string][]string),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// run executes every target. process performs the target-specific work and
// returns its classified outcome (on cancelled context it must return
// StatusFailed). Results are returned in the deterministic topological order,
// independent of completion timing.
func (s *scheduler) run(ctx context.Context, process func(context.Context, *plan.Target) outcome) []jobResult {
	for _, t := range s.order {
		s.index[t.ID] = t
		s.indeg[t.ID] = len(uniqueDeps(t.Deps))
		for _, dep := range uniqueDeps(t.Deps) {
			s.children[dep] = append(s.children[dep], t.ID)
		}
	}
	for _, t := range s.order {
		if s.indeg[t.ID] == 0 {
			s.pending = insertPending(s.pending, pendingItem{id: t.ID})
		}
	}

	jobs := make(chan *plan.Target, len(s.order))
	var wg sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				oc := process(ctx, t)
				s.finish(t.ID, t.Path, oc)
			}
		}()
	}

	interrupted := false
	s.mu.Lock()
	for {
		// Resolve queued skips inline; dispatch runnable jobs up to workers.
		for {
			// Apply any skip items first (smallest-ID ordering is preserved
			// because pending is sorted).
			if len(s.pending) > 0 && s.pending[0].skip {
				item := s.pending[0]
				s.pending = s.pending[1:]
				s.recordLocked(item.id, tPath(s.index, item.id), outcome{
					status: StatusSkipped, reason: item.reason,
				})
				s.propagateLocked(item.id, true)
				continue
			}
			if ctx.Err() != nil {
				interrupted = true
				break
			}
			if len(s.pending) == 0 || s.inFlight >= s.workers {
				break
			}
			item := s.pending[0]
			s.pending = s.pending[1:]
			s.inFlight++
			s.mu.Unlock()
			jobs <- s.index[item.id] // buffered: never blocks beyond n
			s.mu.Lock()
		}

		if len(s.pending) == 0 && s.inFlight == 0 {
			break
		}
		if interrupted {
			// Stop dispatching; drain what is already running.
			for s.inFlight > 0 {
				s.cond.Wait()
			}
			break
		}
		s.cond.Wait()
	}
	s.mu.Unlock()
	close(jobs)
	wg.Wait()

	// Any target without a terminal result (interruption) is reported skipped.
	s.mu.Lock()
	for _, t := range s.order {
		if _, ok := s.recorded[t.ID]; !ok {
			s.recordLocked(t.ID, t.Path, outcome{
				status: StatusSkipped,
				reason: "not executed: run was interrupted before this target started",
			})
		}
	}
	out := make([]jobResult, 0, len(s.order))
	for _, t := range s.order {
		jr := s.recorded[t.ID]
		jr.path = t.Path
		jr.targetID = t.ID
		out = append(out, jr)
	}
	s.mu.Unlock()
	return out
}

// shaOf returns the rendered digest recorded for id, if any.
func (s *scheduler) shaOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shas[id]
}

// finish is called by worker goroutines with the outcome of a target.
func (s *scheduler) finish(id, path string, oc outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(id, path, oc)
	s.propagateLocked(id, oc.status == StatusFailed || oc.status == StatusSkipped)
	s.inFlight--
	s.cond.Broadcast()
}

// recordLocked stores one terminal outcome. Caller holds s.mu.
func (s *scheduler) recordLocked(id, path string, oc outcome) {
	if _, exists := s.recorded[id]; exists {
		return
	}
	s.recorded[id] = jobResult{
		targetID: id, path: path, status: oc.status,
		reason: oc.reason, attempt: oc.attempt,
	}
	if oc.sha != "" {
		s.shas[id] = oc.sha
	}
}

// propagateLocked decrements the in-degree of dependents of id and enqueues
// them when ready, remembering whether an upstream outcome was a failure.
// Caller holds s.mu.
func (s *scheduler) propagateLocked(id string, bad bool) {
	kids := append([]string(nil), s.children[id]...)
	insertionSortStrings(kids)
	for _, child := range kids {
		if bad {
			s.badDep[child] = true
		}
		s.indeg[child]--
		if s.indeg[child] == 0 {
			if s.badDep[child] {
				s.pending = insertPending(s.pending, pendingItem{
					id:     child,
					skip:   true,
					reason: "not executed: upstream dependency \"" + id + "\" did not succeed",
				})
			} else {
				s.pending = insertPending(s.pending, pendingItem{id: child})
			}
		}
	}
}

func tPath(index map[string]*plan.Target, id string) string {
	if t := index[id]; t != nil {
		return t.Path
	}
	return ""
}

func uniqueDeps(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

func insertionSortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// insertPending keeps the pending queue sorted by target ID.
func insertPending(q []pendingItem, item pendingItem) []pendingItem {
	i := 0
	for i < len(q) && q[i].id < item.id {
		i++
	}
	q = append(q, pendingItem{})
	copy(q[i+1:], q[i:])
	q[i] = item
	return q
}
