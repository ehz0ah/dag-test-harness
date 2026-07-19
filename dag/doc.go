// Package dag is a Go-native declarative DAG engine, ported from the
// pydagflow kernel.
//
// You describe a workflow of typed operators with a Builder, wiring each node's
// outputs into downstream inputs as data edges; the Executor runs the workflow
// concurrently, honouring dependencies, and captures every node's {input, output}
// as the evidence trace.
//
// The model has three moving parts:
//
//   - Factory / Op — a Factory is the authoring-time operator definition passed
//     to Builder.Add. One Op instance is built per node with its name and static
//     config; its Process receives the resolved inputs and returns named outputs.
//     A returned error fails the run fast (downstream nodes do not start) and is
//     recoverable with errors.As via the *NodeError wrapper.
//
//   - Builder — Add wires a node and returns a *Node handle usable as a
//     downstream input (its sole output) or addressed with Out / At; Var injects
//     a static value; Merge fans several producers into one input; If scopes a
//     conditional block.
//
//   - Executor — instantiates ops, builds the dependency graph (rejecting cycles
//     and dangling references), and Run executes it, returning the Trace.
//
// Example:
//
//	addOp := dag.Factory{
//	    Meta: dag.Meta{Inputs: []string{"a", "b"}, Outputs: []string{"sum"}},
//	    New: func(name string, cfg map[string]any) dag.Op {
//	        return dag.OpFunc(func(in map[string]any) (map[string]any, error) {
//	            return map[string]any{"sum": in["a"].(int) + in["b"].(int)}, nil
//	        })
//	    }}
//	b := dag.New()
//	a := b.Var(10, "a")
//	c := b.Var(20, "b")
//	b.Add(addOp, dag.Spec{Name: "sum", In: dag.In{"a": a, "b": c}})
//	workflow, _ := b.Build()
//	ex, _ := dag.NewExecutor(workflow)
//	trace, _ := ex.Run(context.Background())
//	_ = trace["sum"].Output["sum"] // 30
package dag
