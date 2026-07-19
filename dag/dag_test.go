package dag

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// ── test ops ────────────────────────────────────────────────────────────────--

type gateErr struct{ msg string }

func (e *gateErr) Error() string { return e.msg }

type contextWaitOp struct{ started chan struct{} }

func (o contextWaitOp) Process(map[string]any) (map[string]any, error) {
	return nil, errors.New("plain Process called for context-aware op")
}

func (o contextWaitOp) ProcessContext(ctx context.Context, _ map[string]any) (map[string]any, error) {
	close(o.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

var (
	addOp = Factory{
		Meta: Meta{Inputs: []string{"a", "b", "after"}, Defaults: map[string]any{"a": 0, "b": 0}, Outputs: []string{"sum"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(in map[string]any) (map[string]any, error) {
				return map[string]any{"sum": in["a"].(int) + in["b"].(int)}, nil
			})
		}}
	identOp = Factory{
		Meta: Meta{Inputs: []string{"in", "after"}, Outputs: []string{"out"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(in map[string]any) (map[string]any, error) {
				return map[string]any{"out": in["in"]}, nil
			})
		}}
	pairOp = Factory{
		Meta: Meta{Inputs: []string{"x"}, Outputs: []string{"lo", "hi"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(in map[string]any) (map[string]any, error) {
				return map[string]any{"lo": in["x"], "hi": map[string]any{"deep": in["x"]}}, nil
			})
		}}
	countOp = Factory{
		Meta: Meta{Inputs: []string{"items", "after"}, Outputs: []string{"n"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(in map[string]any) (map[string]any, error) {
				lst, _ := in["items"].([]any)
				return map[string]any{"n": len(lst)}, nil
			})
		}}
	boomOp = Factory{
		Meta: Meta{Inputs: []string{"after"}, Outputs: []string{"out"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(map[string]any) (map[string]any, error) {
				return nil, &gateErr{"tripped"}
			})
		}}
	panicOp = Factory{
		Meta: Meta{Inputs: []string{"after"}, Outputs: []string{"out"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(map[string]any) (map[string]any, error) { panic("kaboom") })
		}}
	noOutputOp = Factory{
		Meta: Meta{Inputs: []string{"after"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
		}}
	missingOutputOp = Factory{
		Meta: Meta{Outputs: []string{"out"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(map[string]any) (map[string]any, error) {
				return map[string]any{"other": true}, nil
			})
		}}
	nilOutputOp = Factory{
		Meta: Meta{Outputs: []string{"out"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(map[string]any) (map[string]any, error) {
				return map[string]any{"out": nil, "extra": true}, nil
			})
		}}
	defaultEchoOp = Factory{
		Meta: Meta{Inputs: []string{"in"}, Defaults: map[string]any{"in": "default"}, Outputs: []string{"out"}},
		New: func(string, map[string]any) Op {
			return OpFunc(func(in map[string]any) (map[string]any, error) {
				return map[string]any{"out": in["in"]}, nil
			})
		}}
)

func mustRun(t *testing.T, b *Builder, opts ...Option) Trace {
	t.Helper()
	workflow, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ex, err := NewExecutor(workflow, opts...)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	trace, err := ex.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return trace
}

// ── pure helpers ────────────────────────────────────────────────────────────--

func TestExtractPath(t *testing.T) {
	type pt struct{ X int }
	data := map[string]any{
		"a":    map[string]any{"b": "x"},
		"list": []any{map[string]any{"uri": "u0"}, map[string]any{"uri": "u1"}},
		"s":    pt{X: 7},                    // struct field via reflect
		"nums": []int{10, 20},               // typed slice index via reflect
		"mi":   map[string]string{"k": "v"}, // non-any map via reflect
		"p":    &pt{X: 9},                   // pointer deref via reflect
	}
	cases := []struct {
		path string
		want any
	}{
		{"", data},
		{"a.b", "x"},
		{"list.1.uri", "u1"},
		{"a.missing", nil},
		{"list.9", nil},
		{"a.b.c", nil}, // step into a string -> miss
		{"s.X", 7},
		{"nums.1", 20},
		{"mi.k", "v"},
		{"p.X", 9},
	}
	for _, c := range cases {
		if got := extractPath(data, c.path); !reflect.DeepEqual(got, c.want) {
			t.Errorf("extractPath(%q) = %#v, want %#v", c.path, got, c.want)
		}
	}
}

func TestTruthyAndCompare(t *testing.T) {
	for _, c := range []struct {
		v    any
		want bool
	}{{nil, false}, {true, true}, {false, false}, {"", false}, {"x", true},
		{0, false}, {3, true}, {[]any{}, false}, {[]any{1}, true}, {map[string]any{}, false}} {
		if got := truthy(c.v); got != c.want {
			t.Errorf("truthy(%#v)=%v want %v", c.v, got, c.want)
		}
	}
	for _, c := range []struct {
		op   string
		l, r any
		want bool
	}{
		{"==", 3, 3.0, true}, {"==", "a", "a", true}, {"!=", 3, 4, true},
		{"<", 2, 3, true}, {">=", 3, 3, true}, {">", 2, 3, false},
		{"==", nil, nil, true}, {"!=", nil, 1, true}, {"<", nil, 1, false},
		{"<", "a", 1, false}, // non-numeric ordering -> false
	} {
		if got := compare(c.op, c.l, c.r); got != c.want {
			t.Errorf("compare(%q,%v,%v)=%v want %v", c.op, c.l, c.r, got, c.want)
		}
	}
}

func TestCondEval(t *testing.T) {
	src := &Node{name: "src", outputs: []string{"value"}}
	kind, score := src.Out("kind"), src.Out("score")
	vals := map[string]any{"kind": "video", "score": 12}
	resolve := func(r Ref) any { return vals[r.producers[0].out] }

	cases := []struct {
		name string
		cond Cond
		want bool
	}{
		{"eq-true", Eq(kind, "video"), true},
		{"eq-false", Eq(kind, "audio"), false},
		{"ne", Ne(kind, "audio"), true},
		{"lt", Lt(score, 20), true},
		{"le", Le(score, 12), true},
		{"ge", Ge(score, 12), true},
		{"gt", Gt(score, 10), true},
		{"and", Eq(kind, "video").And(Gt(score, 10)), true},
		{"and-short", Eq(kind, "audio").And(Gt(score, 10)), false},
		{"or", Eq(kind, "audio").Or(Gt(score, 10)), true},
		{"not", Not(Eq(kind, "audio")), true},
		{"inset", InSet(kind, "audio", "video"), true},
		{"inset-miss", InSet(kind, "audio", "text"), false},
		{"inset-empty", InSet(kind), false},
	}
	for _, c := range cases {
		if got := c.cond.eval(resolve); got != c.want {
			t.Errorf("%s: eval=%v want %v", c.name, got, c.want)
		}
	}
	var nilCond *Cond
	if !nilCond.eval(resolve) {
		t.Error("nil cond must eval true (unconditional)")
	}
}

// ── build + run ─────────────────────────────────────────────────────────────--

func TestBuildAndRun(t *testing.T) {
	b := New()
	a := b.Var(10, "a")
	bb := b.Var(20, "b")
	sum := b.Add(addOp, Spec{Name: "sum", In: In{"a": a, "b": bb}})
	b.Add(identOp, Spec{Name: "echo", In: In{"in": sum, "after": sum}})

	tr := mustRun(t, b)
	if tr["sum"].Output["sum"] != 30 {
		t.Fatalf("sum = %v, want 30", tr["sum"].Output["sum"])
	}
	if tr["echo"].Output["out"] != 30 {
		t.Fatalf("echo = %v, want 30", tr["echo"].Output["out"])
	}
	if tr["sum"].Input["a"] != 10 || tr["sum"].Input["b"] != 20 {
		t.Fatalf("trace input not captured: %#v", tr["sum"].Input)
	}
}

func TestInspectableStructureAndTerminalMetadata(t *testing.T) {
	terminalIdentOp := identOp
	terminalIdentOp.Terminal = true

	b := New()
	root := b.Var(map[string]any{"kind": "video"}, "root")
	z := b.Add(terminalIdentOp, Spec{Name: "z", In: In{"in": root.At("kind")}})
	a := b.Add(identOp, Spec{Name: "a", In: In{"in": root.At("kind")}})

	if root.Name() != "root" || z.Name() != "z" || a.Name() != "a" {
		t.Fatalf("node names = %q, %q, %q", root.Name(), z.Name(), a.Name())
	}

	workflow, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, want := workflow.Nodes(), []string{"root", "z", "a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	if !workflow.Terminal("z") || workflow.Terminal("a") || workflow.Terminal("missing") {
		t.Fatalf("terminal metadata: z=%v a=%v missing=%v",
			workflow.Terminal("z"), workflow.Terminal("a"), workflow.Terminal("missing"))
	}

	ex, err := NewExecutor(workflow)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	if got, want := ex.Successors()["root"], []string{"a", "z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root successors = %v, want sorted %v", got, want)
	}
	if got := ex.Successors()["z"]; len(got) != 0 {
		t.Fatalf("terminal node successors = %v, want none", got)
	}

	trace, err := ex.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if trace["z"].Input["in"] != "video" || trace["a"].Input["in"] != "video" {
		t.Fatalf("Node.At path inputs = z:%#v a:%#v", trace["z"].Input, trace["a"].Input)
	}
}

func TestDefaultsApplied(t *testing.T) {
	b := New()
	a := b.Var(7, "a")
	// b input unwired -> default 0
	b.Add(addOp, Spec{Name: "sum", In: In{"a": a}})
	tr := mustRun(t, b)
	if tr["sum"].Output["sum"] != 7 {
		t.Fatalf("default not applied: %v", tr["sum"].Output["sum"])
	}
}

func TestMultiOutputAndPath(t *testing.T) {
	b := New()
	src := b.Var("hello", "src")
	p := b.Add(pairOp, Spec{Name: "p", In: In{"x": src}})
	// .Out addresses a named output; .At extracts a field path from it
	b.Add(identOp, Spec{Name: "lo", In: In{"in": p.Out("lo")}})
	b.Add(identOp, Spec{Name: "deep", In: In{"in": p.Out("hi").At("deep")}})

	tr := mustRun(t, b)
	if tr["lo"].Output["out"] != "hello" {
		t.Errorf("lo = %v", tr["lo"].Output["out"])
	}
	if tr["deep"].Output["out"] != "hello" {
		t.Errorf("deep path = %v", tr["deep"].Output["out"])
	}
}

func TestMergeFanIn(t *testing.T) {
	b := New()
	a := b.Var("x", "a")
	c := b.Var("y", "b")
	d := b.Var("z", "c")
	b.Add(countOp, Spec{Name: "cnt", In: In{"items": b.Merge(a, c, d)}})
	tr := mustRun(t, b)
	if tr["cnt"].Output["n"] != 3 {
		t.Fatalf("fan-in count = %v, want 3", tr["cnt"].Output["n"])
	}
	// the merged node depends on all three producers
	deps := must(t, b).Dependencies()["cnt"]
	if len(deps) != 3 {
		t.Fatalf("cnt deps = %v, want 3", deps)
	}
}

func TestMergePathExtractsEachValue(t *testing.T) {
	b := New()
	a := b.Var(map[string]any{"uri": "u1"}, "a")
	c := b.Var(map[string]any{"uri": "u2"}, "b")
	b.Add(identOp, Spec{Name: "uris", In: In{"in": b.Merge(a, c).At("uri")}})

	tr := mustRun(t, b)
	want := []any{"u1", "u2"}
	if got := tr["uris"].Input["in"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge(...).At path values = %#v, want %#v", got, want)
	}
	if got := tr["uris"].Output["out"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("downstream output = %#v, want %#v", got, want)
	}
}

func TestSelectOverrideLastWins(t *testing.T) {
	b := New()
	v1 := b.Var("first", "v1")
	v2 := b.Var("second", "v2")
	b.Add(identOp, Spec{Name: "pick", In: In{"in": b.Select(v1, v2)}})
	tr := mustRun(t, b)
	if tr["pick"].Output["out"] != "second" {
		t.Fatalf("Select must resolve to the last producer's value, got %v", tr["pick"].Output["out"])
	}
}

func TestSelectUsesLastActiveProducerEvenWhenNil(t *testing.T) {
	b := New()
	first := b.Var("first", "first")
	flag := b.Var("clear", "flag")
	var clear *Node
	b.If(Eq(flag, "clear"), func() {
		clear = b.Add(nilOutputOp, Spec{Name: "clear"})
	})
	b.Add(identOp, Spec{Name: "pick", In: In{"in": b.Select(first, clear)}})

	tr := mustRun(t, b)
	if _, ok := tr["clear"].Output["out"]; !ok {
		t.Fatalf("active nil producer must still publish its declared key: %#v", tr["clear"].Output)
	}
	if got, ok := tr["pick"].Input["in"]; !ok || got != nil {
		t.Fatalf("Select should use the last active producer's nil value, got %#v", tr["pick"].Input)
	}
	if got, ok := tr["pick"].Output["out"]; !ok || got != nil {
		t.Fatalf("nil selected input should flow through as nil output, got %#v", tr["pick"].Output)
	}
}

func TestWiredNilInputIsNotReplacedByDefault(t *testing.T) {
	b := New()
	src := b.Add(nilOutputOp, Spec{Name: "src"})
	b.Add(defaultEchoOp, Spec{Name: "downstream", In: In{"in": src}})

	tr := mustRun(t, b)
	if got, ok := tr["downstream"].Input["in"]; !ok || got != nil {
		t.Fatalf("wired nil input should survive defaults, got %#v", tr["downstream"].Input)
	}
	if got, ok := tr["downstream"].Output["out"]; !ok || got != nil {
		t.Fatalf("wired nil output should survive defaults, got %#v", tr["downstream"].Output)
	}
}

func TestMergeCollectsOnlyActiveBranchValues(t *testing.T) {
	b := New()
	flag := b.Var("keep", "flag")
	keepValue := b.Var("keep", "keep_value")
	dropValue := b.Var("drop", "drop_value")
	var keep, drop, clear *Node
	b.If(Eq(flag, "keep"), func() {
		keep = b.Add(identOp, Spec{Name: "keep", In: In{"in": keepValue}})
		clear = b.Add(nilOutputOp, Spec{Name: "clear"})
	})
	b.If(Eq(flag, "drop"), func() {
		drop = b.Add(identOp, Spec{Name: "drop", In: In{"in": dropValue}})
	})
	b.Add(identOp, Spec{Name: "merged", In: In{"in": b.Merge(keep, drop, clear)}})

	tr := mustRun(t, b)
	want := []any{"keep", nil}
	if got := tr["merged"].Input["in"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge active values = %#v, want %#v", got, want)
	}
	if !tr["drop"].Skipped {
		t.Fatalf("drop branch should be skipped: %#v", tr["drop"])
	}
}

func TestIfSkip(t *testing.T) {
	b := New()
	flag := b.Var("video", "flag")
	b.If(Eq(flag, "video"), func() {
		b.Add(identOp, Spec{Name: "yes", In: In{"in": flag}})
	})
	b.If(Eq(flag, "audio"), func() {
		b.Add(identOp, Spec{Name: "no", In: In{"in": flag}})
	})
	tr := mustRun(t, b)
	if tr["yes"].Skipped || tr["yes"].Output["out"] != "video" {
		t.Errorf("yes branch should run: %#v", tr["yes"])
	}
	if !tr["no"].Skipped || tr["no"].Output["out"] != nil {
		t.Errorf("no branch should be skipped with nil output: %#v", tr["no"])
	}
}

func TestNestedIfRequiresEveryCondition(t *testing.T) {
	for _, c := range []struct {
		name        string
		inner       string
		wantSkipped bool
	}{
		{name: "both-true", inner: "on"},
		{name: "inner-false", inner: "off", wantSkipped: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := New()
			outer := b.Var("on", "outer")
			inner := b.Var(c.inner, "inner")
			b.If(Eq(outer, "on"), func() {
				b.If(Eq(inner, "on"), func() {
					b.Add(identOp, Spec{Name: "both", In: In{"in": outer}})
				})
			})

			ex := must(t, b)
			if deps := ex.Dependencies()["both"]; !reflect.DeepEqual(deps, []string{"inner", "outer"}) {
				t.Fatalf("nested branch deps = %v, want inner and outer condition refs", deps)
			}
			trace, err := ex.Run(context.Background())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if trace["both"].Skipped != c.wantSkipped {
				t.Fatalf("nested branch skipped=%v want %v: %#v", trace["both"].Skipped, c.wantSkipped, trace["both"])
			}
			if !c.wantSkipped && trace["both"].Output["out"] != "on" {
				t.Fatalf("nested branch output = %#v, want on", trace["both"].Output)
			}
			if c.wantSkipped && trace["both"].Output["out"] != nil {
				t.Fatalf("skipped nested branch output = %#v, want nil output", trace["both"].Output)
			}
		})
	}
}

func TestConditionInputOperandBecomesDependency(t *testing.T) {
	b := New()
	left := b.Var(3, "left")
	right := b.Var(3, "right")
	b.If(Eq(left, right), func() {
		b.Add(identOp, Spec{Name: "branch", In: In{"in": left}})
	})

	ex := must(t, b)
	if deps := ex.Dependencies()["branch"]; !reflect.DeepEqual(deps, []string{"left", "right"}) {
		t.Fatalf("branch deps = %v, want left and right from condition refs", deps)
	}
	tr, err := ex.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if tr["branch"].Skipped || tr["branch"].Output["out"] != 3 {
		t.Fatalf("branch should run after condition inputs resolve: %#v", tr["branch"])
	}
}

// ── error / structural paths ────────────────────────────────────────────────--

func TestFailFastPropagatesTypedError(t *testing.T) {
	b := New()
	bad := b.Add(boomOp, Spec{Name: "bad"})
	b.Add(identOp, Spec{Name: "downstream", In: In{"in": bad, "after": bad}})

	workflow, _ := b.Build()
	ex, _ := NewExecutor(workflow)
	tr, err := ex.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var ne *NodeError
	if !errors.As(err, &ne) || ne.Node != "bad" {
		t.Fatalf("want *NodeError at 'bad', got %v", err)
	}
	var ge *gateErr
	if !errors.As(err, &ge) {
		t.Fatalf("original typed error must survive errors.As, got %v", err)
	}
	if _, ran := tr["downstream"]; ran {
		t.Error("downstream must not run after fail-fast")
	}
}

func TestPanicRecovered(t *testing.T) {
	b := New()
	b.Add(panicOp, Spec{Name: "p"})
	workflow, _ := b.Build()
	ex, _ := NewExecutor(workflow)
	_, err := ex.Run(context.Background())
	if err == nil || !contains(err.Error(), "kaboom") {
		t.Fatalf("panic not surfaced as error: %v", err)
	}
}

func TestCycleDetected(t *testing.T) {
	// hand-build a 2-node cycle (the Builder cannot express one — a forward ref
	// is impossible — so we construct the IR directly).
	n1 := &nodeIR{name: "x", factory: identOp, outputs: []string{"out"},
		inputs: map[string]Ref{"in": {producers: []producer{{node: "y", out: "out"}}}}}
	n2 := &nodeIR{name: "y", factory: identOp, outputs: []string{"out"},
		inputs: map[string]Ref{"in": {producers: []producer{{node: "x", out: "out"}}}}}
	workflow := &Dag{nodes: []*nodeIR{n1, n2}, byName: map[string]*nodeIR{"x": n1, "y": n2}}
	if _, err := NewExecutor(workflow); err == nil || !contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestBuildErrors(t *testing.T) {
	b := New()
	b.Add(Factory{}, Spec{Name: "a"})                               // nil Factory.New
	b.Add(identOp, Spec{Name: "a"})                                 // duplicate name
	b.Add(identOp, Spec{Name: "c", In: In{"bogus": b.Var(1, "v")}}) // undeclared input
	_, err := b.Build()
	if err == nil {
		t.Fatal("expected accumulated build errors")
	}
	for _, want := range []string{"nil Factory.New", "duplicate", "not declared"} {
		if !contains(err.Error(), want) {
			t.Errorf("build error missing %q: %v", want, err)
		}
	}
}

func TestUnknownProducerRejected(t *testing.T) {
	n := &nodeIR{name: "x", factory: identOp, outputs: []string{"out"},
		inputs: map[string]Ref{"in": {producers: []producer{{node: "ghost", out: "out"}}}}}
	workflow := &Dag{nodes: []*nodeIR{n}, byName: map[string]*nodeIR{"x": n}}
	if _, err := NewExecutor(workflow); err == nil || !contains(err.Error(), "unknown producer") {
		t.Fatalf("want unknown-producer error, got %v", err)
	}
}

func TestUnknownOutputRejected(t *testing.T) {
	b := New()
	src := b.Add(pairOp, Spec{Name: "src", In: In{"x": b.Var("x", "v")}})
	b.Add(identOp, Spec{Name: "consumer", In: In{"in": src.Out("missing")}})
	workflow, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := NewExecutor(workflow); err == nil ||
		!contains(err.Error(), "unknown output") ||
		!contains(err.Error(), "missing") {
		t.Fatalf("want unknown-output error, got %v", err)
	}
}

func TestUnknownOutputInConditionRejected(t *testing.T) {
	b := New()
	src := b.Add(pairOp, Spec{Name: "src", In: In{"x": b.Var("x", "v")}})
	b.If(Eq(src.Out("missing"), "x"), func() {
		b.Add(identOp, Spec{Name: "branch", In: In{"in": src.Out("lo")}})
	})
	workflow, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := NewExecutor(workflow); err == nil ||
		!contains(err.Error(), "condition") ||
		!contains(err.Error(), "unknown output") {
		t.Fatalf("want condition unknown-output error, got %v", err)
	}
}

func TestNoOutputNodeCanBeOrderingEdge(t *testing.T) {
	b := New()
	pre := b.Add(noOutputOp, Spec{Name: "pre"})
	b.Add(identOp, Spec{Name: "tail", In: In{"in": b.Var("ok", "v"), "after": pre}})
	tr := mustRun(t, b)
	if tr["tail"].Output["out"] != "ok" {
		t.Fatalf("ordering edge blocked tail: %#v", tr["tail"])
	}
}

func TestRuntimeMissingDeclaredOutputRejected(t *testing.T) {
	b := New()
	bad := b.Add(missingOutputOp, Spec{Name: "bad"})
	b.Add(identOp, Spec{Name: "downstream", In: In{"in": bad}})
	workflow, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ex, err := NewExecutor(workflow)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	tr, err := ex.Run(context.Background())
	if err == nil ||
		!contains(err.Error(), "missing declared output") ||
		!contains(err.Error(), "out") {
		t.Fatalf("want missing-output error, got %v", err)
	}
	var ne *NodeError
	if !errors.As(err, &ne) || ne.Node != "bad" {
		t.Fatalf("want *NodeError at bad, got %v", err)
	}
	if _, ran := tr["downstream"]; ran {
		t.Fatal("downstream must not run after missing output")
	}
}

func TestRuntimeNilDeclaredOutputAllowed(t *testing.T) {
	b := New()
	src := b.Add(nilOutputOp, Spec{Name: "src"})
	b.Add(identOp, Spec{Name: "downstream", In: In{"in": src}})
	tr := mustRun(t, b)
	if _, ok := tr["src"].Output["out"]; !ok {
		t.Fatalf("declared output key must be present: %#v", tr["src"].Output)
	}
	if !reflect.DeepEqual(tr["downstream"].Input, map[string]any{"in": nil}) {
		t.Fatalf("nil output value should flow downstream: %#v", tr["downstream"].Input)
	}
}

func TestDependenciesAndLayers(t *testing.T) {
	b := New()
	a := b.Var(1, "a")
	c := b.Var(2, "b")
	s := b.Add(addOp, Spec{Name: "s", In: In{"a": a, "b": c}})
	b.Add(identOp, Spec{Name: "tail", In: In{"in": s}})

	ex := must(t, b)
	if d := ex.Dependencies()["s"]; !reflect.DeepEqual(d, []string{"a", "b"}) {
		t.Errorf("deps[s] = %v", d)
	}
	layers := ex.Layers()
	if len(layers) != 3 || len(layers[0]) != 2 {
		t.Fatalf("layers = %v (want 3 layers, first width 2)", layers)
	}
}

// ── concurrency ─────────────────────────────────────────────────────────────--

func TestConcurrentLayerRunsInParallel(t *testing.T) {
	arrived := make(chan string, 2)
	release := make(chan struct{})
	barrierOp := Factory{Meta: Meta{Outputs: []string{"out"}},
		New: func(name string, _ map[string]any) Op {
			return OpFunc(func(map[string]any) (map[string]any, error) {
				arrived <- name
				<-release
				return map[string]any{"out": name}, nil
			})
		}}
	b := New()
	b.Add(barrierOp, Spec{Name: "b1"})
	b.Add(barrierOp, Spec{Name: "b2"})
	workflow, _ := b.Build()
	ex, _ := NewExecutor(workflow, WithConcurrency(2))

	go func() { <-arrived; <-arrived; close(release) }() // releases only once BOTH started

	done := make(chan error, 1)
	go func() { _, err := ex.Run(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nodes serialized — concurrency broken (deadlocked on barrier)")
	}
}

func TestContextCancelStopsLaunch(t *testing.T) {
	b := New()
	b.Add(identOp, Spec{Name: "a", In: In{"in": b.Var(1, "v")}})
	workflow, _ := b.Build()
	ex, _ := NewExecutor(workflow)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ex.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestContextCancelStopsRunningContextOp(t *testing.T) {
	started := make(chan struct{})
	waitOp := Factory{Meta: Meta{Outputs: []string{"out"}},
		New: func(string, map[string]any) Op {
			return contextWaitOp{started: started}
		}}
	b := New()
	b.Add(waitOp, Spec{Name: "wait"})
	workflow, _ := b.Build()
	ex, _ := NewExecutor(workflow)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ex.Run(ctx)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("context-aware op did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running op did not stop after context cancellation")
	}
}

// ── small helpers ───────────────────────────────────────────────────────────--

func must(t *testing.T, b *Builder) *Executor {
	t.Helper()
	workflow, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ex, err := NewExecutor(workflow)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	return ex
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return s == sub
}
