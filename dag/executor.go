package dag

import (
	"context"
	"fmt"
	"sort"
)

// ── Execution ──────────────────────────────────────────────────────────────--

// NodeError wraps the error an op's Process returned (or a panic it raised),
// tagging it with the node name. The original is recoverable with errors.As, so
// callers can classify a failure by its underlying type while still knowing where
// it happened.
type NodeError struct {
	Node string
	Err  error
}

func (e *NodeError) Error() string { return fmt.Sprintf("node %q: %v", e.Node, e.Err) }
func (e *NodeError) Unwrap() error { return e.Err }

// NodeIO is one node's captured run: the inputs it received and the outputs it
// produced (the evidence trace). Skipped marks a node whose condition was false;
// Err holds the error string when the node failed.
type NodeIO struct {
	Input   map[string]any
	Output  map[string]any
	Skipped bool
	Err     string
}

// Trace is the per-node {input, output} record of a run — the source of truth a
// caller reads results and evidence from.
type Trace map[string]NodeIO

// Executor runs a built Dag. It instantiates one op per node, computes the static
// dependency graph, and rejects a cyclic graph up front.
type Executor struct {
	workflow     *Dag
	ops          map[string]Op
	dependencies map[string][]string
	successors   map[string][]string
	indegree     map[string]int
	concurrency  int
}

// Option configures an Executor.
type Option func(*Executor)

// WithConcurrency caps how many op Processes run at once (default 32).
func WithConcurrency(n int) Option {
	return func(e *Executor) {
		if n > 0 {
			e.concurrency = n
		}
	}
}

// NewExecutor instantiates every node's op, builds the dependency graph, and
// detects cycles / dangling producer references.
func NewExecutor(workflow *Dag, opts ...Option) (*Executor, error) {
	e := &Executor{workflow: workflow, ops: map[string]Op{}, concurrency: 32}
	for _, o := range opts {
		o(e)
	}
	if err := e.buildDependencies(); err != nil {
		return nil, err
	}
	for _, n := range workflow.nodes {
		if err := n.factory.validate(); err != nil {
			return nil, fmt.Errorf("node %q: %w", n.name, err)
		}
		e.ops[n.name] = n.factory.New(n.name, n.config)
	}
	return e, nil
}

func (e *Executor) buildDependencies() error {
	dependencies := map[string][]string{}
	for _, n := range e.workflow.nodes {
		seen := map[string]bool{}
		var add func(names []string)
		add = func(names []string) {
			for _, m := range names {
				if m == n.name || seen[m] {
					continue
				}
				if _, ok := e.workflow.byName[m]; !ok {
					continue // validated below
				}
				seen[m] = true
				dependencies[n.name] = append(dependencies[n.name], m)
			}
		}
		for _, ref := range n.inputs {
			add(ref.producerNodes())
		}
		add(n.cond.refNodes())
		for input, ref := range n.inputs {
			if err := e.validateRef(n.name, "input "+input, ref); err != nil {
				return err
			}
		}
		for _, ref := range n.cond.refs() {
			if err := e.validateRef(n.name, "condition", ref); err != nil {
				return err
			}
		}
	}
	e.dependencies = dependencies
	e.indegree = map[string]int{}
	e.successors = map[string][]string{}
	for _, n := range e.workflow.nodes {
		e.indegree[n.name] = len(dependencies[n.name])
		if _, ok := e.successors[n.name]; !ok {
			e.successors[n.name] = nil
		}
		for _, d := range dependencies[n.name] {
			e.successors[d] = append(e.successors[d], n.name)
		}
	}
	if cyclic, n := e.detectCycle(); cyclic {
		return fmt.Errorf("dag: cycle detected in DAG (resolved %d/%d nodes)", n, len(e.workflow.nodes))
	}
	return nil
}

func (e *Executor) validateRef(owner, label string, ref Ref) error {
	for _, p := range ref.producers {
		producer, ok := e.workflow.byName[p.node]
		if !ok {
			return fmt.Errorf("node %q %s references unknown producer %q", owner, label, p.node)
		}
		if !declaresOutput(producer, p.out) {
			return fmt.Errorf("node %q %s references unknown output %q on producer %q",
				owner, label, p.out, p.node)
		}
		for _, condRef := range p.cond.refs() {
			if err := e.validateRef(owner, label+" edge condition", condRef); err != nil {
				return err
			}
		}
	}
	return nil
}

func declaresOutput(node *nodeIR, output string) bool {
	if output == "" && len(node.outputs) == 0 {
		return true
	}
	for _, declared := range node.outputs {
		if declared == output {
			return true
		}
	}
	return false
}

func (e *Executor) detectCycle() (bool, int) {
	indegree := map[string]int{}
	for k, v := range e.indegree {
		indegree[k] = v
	}
	queue := []string{}
	for _, n := range e.workflow.nodes {
		if indegree[n.name] == 0 {
			queue = append(queue, n.name)
		}
	}
	count := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		count++
		for _, s := range e.successors[n] {
			indegree[s]--
			if indegree[s] == 0 {
				queue = append(queue, s)
			}
		}
	}
	return count < len(e.workflow.nodes), count
}

// Dependencies returns each node's upstream producer nodes (sorted) — for offline
// inspection and smoke tests.
func (e *Executor) Dependencies() map[string][]string {
	out := map[string][]string{}
	for n, ds := range e.dependencies {
		cp := append([]string(nil), ds...)
		sort.Strings(cp)
		out[n] = cp
	}
	return out
}

// Successors returns each node's downstream consumers (sorted) — the reverse of
// Dependencies. Used to assert a node is terminal (no successors).
func (e *Executor) Successors() map[string][]string {
	out := map[string][]string{}
	for n, ss := range e.successors {
		cp := append([]string(nil), ss...)
		sort.Strings(cp)
		out[n] = cp
	}
	return out
}

// Layers returns the workflow's topological layers (each layer's nodes can run in
// parallel).
func (e *Executor) Layers() [][]string {
	indegree := map[string]int{}
	for k, v := range e.indegree {
		indegree[k] = v
	}
	remaining := len(e.workflow.nodes)
	var layers [][]string
	for remaining > 0 {
		var ready []string
		for _, n := range e.workflow.nodes {
			if indegree[n.name] == 0 {
				ready = append(ready, n.name)
			}
		}
		if len(ready) == 0 {
			break
		}
		layers = append(layers, ready)
		for _, n := range ready {
			indegree[n] = -1
			remaining--
			for _, s := range e.successors[n] {
				indegree[s]--
			}
		}
	}
	return layers
}

type doneMsg struct {
	node string
	in   map[string]any
	out  map[string]any
	err  error
}

// Run executes the DAG concurrently, honouring dependencies, and returns the
// trace plus the first op error (wrapped in *NodeError). On error, downstream
// nodes are not started; nodes already running are drained and their results
// kept in the trace. ctx cancellation stops launching new nodes.
func (e *Executor) Run(ctx context.Context) (Trace, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := map[string]map[string]any{}
	trace := Trace{}
	skipped := map[string]bool{}
	indegree := map[string]int{}
	for k, v := range e.indegree {
		indegree[k] = v
	}
	var ready []string
	for _, n := range e.workflow.nodes {
		if indegree[n.name] == 0 {
			ready = append(ready, n.name)
		}
	}

	doneCh := make(chan doneMsg)
	inFlight := 0
	completed := 0
	total := len(e.workflow.nodes)
	var firstErr error

	addSuccessors := func(node string) {
		for _, s := range e.successors[node] {
			indegree[s]--
			if indegree[s] == 0 {
				ready = append(ready, s)
			}
		}
	}

	for {
		for len(ready) > 0 && firstErr == nil && ctx.Err() == nil {
			n := ready[0]
			node := e.workflow.byName[n]
			resolve := func(r Ref) any { return e.resolveRef(r, results, skipped) }
			if node.cond != nil && !node.cond.eval(resolve) {
				ready = ready[1:]
				results[n] = nilOutputs(node)
				skipped[n] = true
				trace[n] = NodeIO{Input: map[string]any{}, Output: results[n], Skipped: true}
				completed++
				addSuccessors(n)
				continue
			}
			if inFlight >= e.concurrency {
				break
			}
			ready = ready[1:]
			in := e.resolveInputs(node, results, skipped)
			op := e.ops[n]
			inFlight++
			go func(name string, op Op, in map[string]any) {
				out, err := safeProcess(ctx, op, in)
				doneCh <- doneMsg{node: name, in: in, out: out, err: err}
			}(n, op, in)
		}

		if inFlight == 0 {
			break
		}

		d := <-doneCh
		inFlight--
		if d.err == nil {
			d.err = validateOutputs(e.workflow.byName[d.node], d.out)
		}
		if d.err != nil {
			if firstErr == nil {
				firstErr = &NodeError{Node: d.node, Err: d.err}
				cancel()
			}
			trace[d.node] = NodeIO{Input: d.in, Err: d.err.Error()}
			continue
		}
		trace[d.node] = NodeIO{Input: d.in, Output: d.out}
		if firstErr != nil {
			continue // draining
		}
		results[d.node] = d.out
		completed++
		addSuccessors(d.node)
	}

	if firstErr != nil {
		return trace, firstErr
	}
	if ctx.Err() != nil {
		return trace, ctx.Err()
	}
	if completed < total {
		return trace, fmt.Errorf("dag: deadlock — completed %d/%d nodes", completed, total)
	}
	return trace, nil
}

func (e *Executor) resolveInputs(node *nodeIR, results map[string]map[string]any, skipped map[string]bool) map[string]any {
	in := map[string]any{}
	for k, ref := range node.inputs {
		in[k] = e.resolveRef(ref, results, skipped)
	}
	for k, dv := range node.factory.Meta.Defaults {
		if _, ok := in[k]; !ok {
			in[k] = dv
		}
	}
	return in
}

func (e *Executor) resolveRef(ref Ref, results map[string]map[string]any, skipped map[string]bool) any {
	if len(ref.producers) == 0 {
		return nil
	}
	resolve := func(r Ref) any { return e.resolveRef(r, results, skipped) }
	lookup := func(p producer) (any, bool) {
		if skipped[p.node] {
			return nil, false
		}
		r, ok := results[p.node]
		if !ok {
			return nil, false
		}
		return r[p.out], true
	}

	var val any
	if len(ref.producers) == 1 && !ref.collect {
		p := ref.producers[0]
		if p.cond == nil || p.cond.eval(resolve) {
			val, _ = lookup(p)
		}
	} else {
		var active []any
		for _, p := range ref.producers {
			if p.cond != nil && !p.cond.eval(resolve) {
				continue
			}
			if v, ok := lookup(p); ok {
				active = append(active, v)
			}
		}
		if ref.collect {
			val = active
		} else if len(active) > 0 {
			val = active[len(active)-1]
		} else {
			val = nil
		}
	}

	if ref.path != "" {
		if lst, ok := val.([]any); ok && ref.collect {
			out := make([]any, len(lst))
			for i, v := range lst {
				out[i] = extractPath(v, ref.path)
			}
			val = out
		} else {
			val = extractPath(val, ref.path)
		}
	}
	return val
}

func nilOutputs(node *nodeIR) map[string]any {
	out := map[string]any{}
	for _, k := range node.outputs {
		out[k] = nil
	}
	return out
}

func validateOutputs(node *nodeIR, out map[string]any) error {
	if len(node.outputs) == 0 {
		return nil
	}
	var missing []string
	for _, k := range node.outputs {
		if _, ok := out[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing declared output(s) %v", missing)
	}
	return nil
}

func safeProcess(ctx context.Context, op Op, in map[string]any) (out map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in op: %v", r)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if op, ok := op.(ContextOp); ok {
		return op.ProcessContext(ctx, in)
	}
	return op.Process(in)
}
