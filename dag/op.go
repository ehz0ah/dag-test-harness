package dag

import (
	"context"
	"fmt"
)

// Op is a single unit of computation in the DAG. One Op instance is constructed
// per node (via Factory.New) and run once per execution.
//
// Process receives the node's resolved inputs (declared input key -> value, with
// defaults applied) and returns its outputs (output key -> value). The op reads
// its own static configuration from the value captured at construction time —
// configuration is NOT passed through Process, mirroring the pydagflow split
// between data edges (inputs) and node config (statics).
//
// An error returned from Process aborts the run fast: downstream nodes are not
// started and the error propagates to the caller (wrapped in *NodeError, so the
// original is recoverable with errors.As). This is the kernel's HARD-fail path.
type Op interface {
	Process(in map[string]any) (map[string]any, error)
}

// ContextOp is implemented by ops that can abort in-flight work when the run
// context is canceled. Process remains the small direct-test API; the executor
// prefers ProcessContext when an op exposes it.
type ContextOp interface {
	ProcessContext(ctx context.Context, in map[string]any) (map[string]any, error)
}

// Meta is an op's static IO schema, known at build time. Inputs are the keys the
// op reads as data edges (in declared order); Outputs are the keys it produces.
// Defaults supplies values for inputs that are declared but left unwired.
type Meta struct {
	Inputs   []string
	Defaults map[string]any
	Outputs  []string
}

// Factory is the authoring-time operator definition and runtime constructor.
// Meta is read at build time to validate wiring and address outputs; New
// constructs the per-node Op instance with its name and static config.
type Factory struct {
	Meta     Meta
	New      func(name string, config map[string]any) Op
	Terminal bool
}

// OpFunc adapts a plain function into an Op, for ops with no per-instance state.
type OpFunc func(in map[string]any) (map[string]any, error)

func (f OpFunc) Process(in map[string]any) (map[string]any, error) { return f(in) }

func (f Factory) validate() error {
	if f.New == nil {
		return fmt.Errorf("factory has a nil Factory.New")
	}
	return nil
}
