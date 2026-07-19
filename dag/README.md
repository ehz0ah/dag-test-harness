# dag

A Go-native declarative DAG engine — a port of the `pydagflow` kernel (its own
module within the ovtest monorepo). You describe a workflow of typed operators,
wiring each node's outputs into downstream inputs as data edges; the executor
runs the workflow concurrently, honouring dependencies, and captures every
node's `{input, output}` as the evidence trace.

It keeps pydagflow's model — typed ops, data-flow edges, fan-in, conditional
branches, concurrency, cycle detection, an inspectable trace — but drops the
Python metaprogramming (dynamic method binding, `exec`-loaded DSL strings,
mutable `Variable` snapshots) for an explicit, statically-typed Go API.

## Model

| concept | dag |
|---|---|
| operator | `Factory` symbol passed to `Builder.Add`; it carries IO `Meta`, the runtime `New` constructor for an `Op` (`Process(in) (out, error)`), and optional execution flags |
| build the workflow | `Builder`: `Add` wires a node and returns a `*Node` handle; `Var` injects a static; `Merge`/`Select` fan in; `If` scopes a conditional block |
| data edge | a `*Node` (its sole output) or `node.Out("key")` / `node.At("path")` wired into another node's input |
| run | `NewExecutor(workflow)` (validates + detects cycles) then `Run(ctx)` → `Trace` |
| evidence trace | `Trace` = per-node `{Input, Output}` (the source of truth) |
| HARD fail-fast | an op returns an `error` → downstream stops, `Run` returns it wrapped in `*NodeError` (recoverable with `errors.As`) |

## pydagflow → dag

| pydagflow | dag |
|---|---|
| `@register_op_output` (derive IO from `process` signature) | explicit `Meta{Inputs, Defaults, Outputs}` per op |
| `ops.add_op(a=v1, b=v2)` (dynamic method) | `b.Add(addOp, Spec{In: In{"a": v1, "b": v2}})` |
| `ops.var(value)` | `b.Var(value, name)` |
| `ops.reduce(...)` fan-in | `b.Merge(nodes...)` |
| `ops.assign` / conditional output | `b.Select(nodes...)` (last active wins) |
| `with ops.if_(cond):` | `b.If(cond, func(){ ... })` |
| `node["data.items"]` (glom) | `node.At("data.items")` |
| `Variable == 'video'` | `dag.Eq(node.Out("kind"), "video")` |
| `GeventDagExecutor.execute(dumped_data_map=…)` | `NewExecutor(...).Run(ctx)` → `Trace` |

## Example

```go
addOp := dag.Factory{
    Meta: dag.Meta{Inputs: []string{"a", "b"}, Outputs: []string{"sum"}},
    New: func(string, map[string]any) dag.Op {
        return dag.OpFunc(func(in map[string]any) (map[string]any, error) {
            return map[string]any{"sum": in["a"].(int) + in["b"].(int)}, nil
        })
    }}
b := dag.New()
a, c := b.Var(10, "a"), b.Var(20, "b")
b.Add(addOp, dag.Spec{Name: "sum", In: dag.In{"a": a, "b": c}})
workflow, _ := b.Build()
ex, _ := dag.NewExecutor(workflow)
trace, _ := ex.Run(context.Background())
fmt.Println(trace["sum"].Output["sum"]) // 30
```

## Test

```sh
go test ./... -cover
```
