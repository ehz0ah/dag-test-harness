package dag

// ── Intermediate representation ────────────────────────────────────────────--
//
// A built workflow is a set of nodes, each with named input edges (Refs into other
// nodes' outputs), static config, an operator factory, and an optional execution
// condition. The IR is produced by a Builder and consumed by an Executor; its
// structural parts can be inspected (Nodes, Dependencies) before any op runs.

// Dag is a built directed acyclic graph: nodes keyed by name, in insertion order.
type Dag struct {
	nodes  []*nodeIR
	byName map[string]*nodeIR
}

// Nodes returns the node names in insertion order.
func (d *Dag) Nodes() []string {
	out := make([]string, len(d.nodes))
	for i, n := range d.nodes {
		out[i] = n.name
	}
	return out
}

// Terminal reports whether a node's operator must be terminal.
func (d *Dag) Terminal(name string) bool {
	if n, ok := d.byName[name]; ok {
		return n.factory.Terminal
	}
	return false
}

type nodeIR struct {
	name    string
	factory Factory
	config  map[string]any
	inputs  map[string]Ref // input key -> producer ref
	outputs []string       // declared output keys
	cond    *Cond          // nil = unconditional
}

// producer identifies one (node, output) source, optionally gated by a condition
// (the assign/conditional-output mechanic).
type producer struct {
	node string
	out  string
	cond *Cond
}

// Ref is a resolved reference wired into a node input or a condition operand. It
// names one or more producer outputs; a path applies field extraction to the
// resolved value (the glom equivalent); collect makes a multi-producer ref
// resolve to the list of all active values rather than the last one (fan-in).
type Ref struct {
	producers []producer
	path      string
	collect   bool
}

func (r Ref) ref() Ref { return r }

// At returns a copy of the ref that extracts a dotted field path from the
// resolved value (e.g. find.At("memories.0.uri")).
func (r Ref) At(path string) Ref {
	r.path = path
	return r
}

func (r Ref) producerNodes() []string {
	out := make([]string, 0, len(r.producers))
	for _, p := range r.producers {
		out = append(out, p.node)
		if p.cond != nil {
			out = append(out, p.cond.refNodes()...)
		}
	}
	return out
}

// Input is anything wirable into a node input slot: a *Node (its sole output) or
// a Ref (a specific output / a merge / a path).
type Input interface{ ref() Ref }

// In is a convenience alias for an input-wiring map (input key -> producer).
type In map[string]Input

// Cfg is a convenience alias for a node's static config map.
type Cfg map[string]any

// Node is a handle to a built node, returned by Builder.Add. Used directly as an
// Input it means the node's sole (first) output; address a specific output with
// Out, or a sub-field with At.
type Node struct {
	name    string
	outputs []string
}

// Name returns the node's name.
func (n *Node) Name() string { return n.name }

func (n *Node) ref() Ref {
	out := ""
	if len(n.outputs) > 0 {
		out = n.outputs[0]
	}
	return Ref{producers: []producer{{node: n.name, out: out}}}
}

// Out references a specific named output of the node.
func (n *Node) Out(key string) Ref {
	return Ref{producers: []producer{{node: n.name, out: key}}}
}

// At references the node's sole output with a field path applied.
func (n *Node) At(path string) Ref { return n.ref().At(path) }
