package dag

import "reflect"

// ── Conditional expressions ────────────────────────────────────────────────--
//
// Conditions drive Builder.If (a node built under a false condition is skipped,
// its outputs nil) and the assign/select mechanic (a producer edge gated by a
// condition). They are built with the comparison helpers below and combined with
// And/Or/Not. Each variable operand is an Input; the literal side is any value.
//
//	cond := dag.Eq(src.Out("kind"), "video").And(dag.Gt(score, 10))

// Cond is a boolean expression over node outputs and literals.
type Cond struct{ root exprNode }

type exprNode interface {
	eval(resolve func(Ref) any) any
}

type litNode struct{ v any }

func (n litNode) eval(func(Ref) any) any { return n.v }

type varNode struct{ r Ref }

func (n varNode) eval(resolve func(Ref) any) any { return resolve(n.r) }

type unaryNode struct {
	op string
	x  exprNode
}

func (n unaryNode) eval(resolve func(Ref) any) any {
	v := n.x.eval(resolve)
	if n.op == "not" {
		// `not nil` is True (a missing upstream is falsy), matching the kernel.
		return v == nil || !truthy(v)
	}
	return nil
}

type binaryNode struct {
	op   string
	l, r exprNode
}

func (n binaryNode) eval(resolve func(Ref) any) any {
	switch n.op {
	case "and":
		return truthy(n.l.eval(resolve)) && truthy(n.r.eval(resolve))
	case "or":
		return truthy(n.l.eval(resolve)) || truthy(n.r.eval(resolve))
	}
	return compare(n.op, n.l.eval(resolve), n.r.eval(resolve))
}

func operand(v any) exprNode {
	if in, ok := v.(Input); ok {
		return varNode{r: in.ref()}
	}
	return litNode{v: v}
}

func cmp(op string, a Input, b any) Cond {
	return Cond{root: binaryNode{op: op, l: varNode{r: a.ref()}, r: operand(b)}}
}

// Comparison helpers: the left side is a node output (Input); the right side is a
// literal value, or another Input for an output-to-output comparison.
func Eq(a Input, b any) Cond { return cmp("==", a, b) }
func Ne(a Input, b any) Cond { return cmp("!=", a, b) }
func Lt(a Input, b any) Cond { return cmp("<", a, b) }
func Le(a Input, b any) Cond { return cmp("<=", a, b) }
func Gt(a Input, b any) Cond { return cmp(">", a, b) }
func Ge(a Input, b any) Cond { return cmp(">=", a, b) }

// In is true when a's value equals any of the listed values (a == v0 || a == v1 …).
// An empty list is always false.
func InSet(a Input, vals ...any) Cond {
	if len(vals) == 0 {
		return Cond{root: litNode{v: false}}
	}
	c := Eq(a, vals[0])
	for _, v := range vals[1:] {
		c = c.Or(Eq(a, v))
	}
	return c
}

// And/Or/Not combine conditions.
func (c Cond) And(d Cond) Cond { return Cond{root: binaryNode{op: "and", l: c.root, r: d.root}} }
func (c Cond) Or(d Cond) Cond  { return Cond{root: binaryNode{op: "or", l: c.root, r: d.root}} }
func Not(c Cond) Cond          { return Cond{root: unaryNode{op: "not", x: c.root}} }

func (c *Cond) eval(resolve func(Ref) any) bool {
	if c == nil {
		return true
	}
	return truthy(c.root.eval(resolve))
}

// refNodes collects every producer node referenced by the condition (for the
// dependency graph).
func (c *Cond) refNodes() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, r := range c.refs() {
		out = append(out, r.producerNodes()...)
	}
	return out
}

func (c *Cond) refs() []Ref {
	if c == nil {
		return nil
	}
	var out []Ref
	var walk func(n exprNode)
	walk = func(n exprNode) {
		switch t := n.(type) {
		case varNode:
			out = append(out, t.r)
		case unaryNode:
			walk(t.x)
		case binaryNode:
			walk(t.l)
			walk(t.r)
		}
	}
	walk(c.root)
	return out
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() > 0
	}
	return true
}

func compare(op string, l, r any) bool {
	// None-comparison semantics mirror the kernel: equality is identity-ish, and
	// any ordering against a nil operand is false.
	if l == nil || r == nil {
		switch op {
		case "==":
			return l == nil && r == nil
		case "!=":
			return !(l == nil && r == nil)
		default:
			return false
		}
	}
	switch op {
	case "==":
		return equalValues(l, r)
	case "!=":
		return !equalValues(l, r)
	}
	lf, lok := toFloat(l)
	rf, rok := toFloat(r)
	if !lok || !rok {
		return false
	}
	switch op {
	case "<":
		return lf < rf
	case "<=":
		return lf <= rf
	case ">":
		return lf > rf
	case ">=":
		return lf >= rf
	}
	return false
}

func equalValues(l, r any) bool {
	if lf, lok := toFloat(l); lok {
		if rf, rok := toFloat(r); rok {
			return lf == rf
		}
	}
	return reflect.DeepEqual(l, r)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
