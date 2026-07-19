package dag

import (
	"errors"
	"fmt"
)

var varOpFactory = Factory{
	Meta: Meta{Outputs: []string{"value"}},
	New: func(_ string, cfg map[string]any) Op {
		return OpFunc(func(map[string]any) (map[string]any, error) {
			return map[string]any{"value": cfg["value"]}, nil
		})
	},
}

// Builder constructs a Dag declaratively. Add wires a node and returns a handle;
// Var injects a static value; Merge fans several producers into one input; If
// scopes a conditional block. Construction never panics on a wiring mistake — it
// records the error and surfaces it from Build.
type Builder struct {
	nodes     []*nodeIR
	byName    map[string]*nodeIR
	counts    map[string]int
	condStack []*Cond
	errs      []error
	varSeq    int
}

// New returns a Builder.
func New() *Builder {
	return &Builder{byName: map[string]*nodeIR{}, counts: map[string]int{}}
}

// Spec describes one node: its name, input edges, and static config. If Name is
// empty, Builder generates a generic name; use explicit names for stable traces.
type Spec struct {
	Name   string
	In     In
	Config Cfg
}

// Add wires an operator into the workflow and returns its handle. Inputs become
// data/ordering edges; Config carries statics the op reads at construction.
func (b *Builder) Add(factory Factory, spec Spec) *Node {
	if err := factory.validate(); err != nil {
		b.errs = append(b.errs, fmt.Errorf("node %q: %w", spec.Name, err))
	}

	name := spec.Name
	if name == "" {
		c := b.counts["op"]
		if c == 0 {
			name = "op"
		} else {
			name = fmt.Sprintf("op_%d", c+1)
		}
		b.counts["op"]++
	}

	if _, dup := b.byName[name]; dup {
		b.errs = append(b.errs, fmt.Errorf("duplicate node name %q", name))
	}

	allowed := map[string]bool{}
	for _, k := range factory.Meta.Inputs {
		allowed[k] = true
	}
	inputs := map[string]Ref{}
	for k, v := range spec.In {
		if !allowed[k] {
			b.errs = append(b.errs, fmt.Errorf("node %q: input %q is not declared in Meta.Inputs %v",
				name, k, factory.Meta.Inputs))
		}
		if v == nil {
			continue
		}
		inputs[k] = v.ref()
	}

	var cond *Cond
	if len(b.condStack) > 0 {
		c := *b.condStack[0]
		for _, extra := range b.condStack[1:] {
			c = c.And(*extra)
		}
		cond = &c
	}

	n := &nodeIR{
		name:    name,
		factory: factory,
		config:  map[string]any(spec.Config),
		inputs:  inputs,
		outputs: append([]string(nil), factory.Meta.Outputs...),
		cond:    cond,
	}
	b.nodes = append(b.nodes, n)
	b.byName[name] = n
	return &Node{name: name, outputs: n.outputs}
}

// Var injects a static value as a single-output ("value") node — the var_op
// equivalent.
func (b *Builder) Var(value any, name string) *Node {
	if name == "" {
		b.varSeq++
		name = fmt.Sprintf("var_%d", b.varSeq)
	}
	return b.Add(varOpFactory, Spec{Name: name, Config: Cfg{"value": value}})
}

// Merge fans several producers into one input edge. The resulting Ref carries all
// producers, so a node consuming it depends on all of them and (collect) resolves
// to the list of their active output values — the fan-in / reduce primitive.
func (b *Builder) Merge(inputs ...Input) Ref {
	r := Ref{collect: true}
	for _, in := range inputs {
		if in == nil {
			continue
		}
		r.producers = append(r.producers, in.ref().producers...)
	}
	return r
}

// Select fans several producers into one input that resolves to the LAST active
// producer's value (the override / assign primitive) rather than the whole list.
// Combined with If — each branch writing the same downstream input — it expresses
// "whichever conditional branch ran last wins".
func (b *Builder) Select(inputs ...Input) Ref {
	r := Ref{}
	for _, in := range inputs {
		if in == nil {
			continue
		}
		r.producers = append(r.producers, in.ref().producers...)
	}
	return r
}

// If scopes a conditional block: every node Add-ed inside fn carries cond (ANDed
// with any enclosing condition). At run time a node whose condition is false is
// skipped and its outputs resolve to nil.
func (b *Builder) If(cond Cond, fn func()) {
	c := cond
	b.condStack = append(b.condStack, &c)
	defer func() { b.condStack = b.condStack[:len(b.condStack)-1] }()
	fn()
}

// Build finalizes the workflow, returning it or the accumulated construction errors.
func (b *Builder) Build() (*Dag, error) {
	if len(b.errs) > 0 {
		return nil, errors.Join(b.errs...)
	}
	return &Dag{nodes: b.nodes, byName: b.byName}, nil
}
