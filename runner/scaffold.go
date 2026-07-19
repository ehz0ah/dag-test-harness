package runner

import (
	"os"
	"path/filepath"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops"
)

// scaffold: thin case-authoring sugar over the operator library. Helpers only
// COMPOSE ops (no new node types) and return the same handles you'd get wiring by
// hand, so "the DAG is the source of truth" still holds.

// FanIn merges several nodes into one ordering/data edge (the reduce primitive):
// a node consuming it depends on all of them. Used to fan-in N add-memory nodes
// before a single wait.
func FanIn(b *dag.Builder, nodes ...*dag.Node) dag.Ref {
	ins := make([]dag.Input, len(nodes))
	for i, n := range nodes {
		ins[i] = n
	}
	return b.Merge(ins...)
}

// FreshAccount provisions a throwaway account: delete-if-exists then create. It
// emits `{name}_cleanup` (best-effort delete) and `{name}` (create) and returns
// the create node — thread its user_key into every user op.
func FreshAccount(b *dag.Builder, account, admin, name string) *dag.Node {
	if name == "" {
		name = "create"
	}
	cleanup := b.Add(ops.OvDeleteAccount, dag.Spec{
		Name: name + "_cleanup", Config: dag.Cfg{"account": account}})
	return b.Add(ops.OvCreateAccount, dag.Spec{
		Name: name, In: dag.In{"after": cleanup},
		Config: dag.Cfg{"account": account, "admin_user": admin}})
}

// ConfiguredUser uses the preconfigured ovcli.conf identity. It returns an empty
// user_key node so existing user_key inputs can keep an ordering/data edge while
// userConf falls back to ~/.openviking/ovcli.conf.
func ConfiguredUser(b *dag.Builder, name string) *dag.Node {
	if name == "" {
		name = "user"
	}
	return b.Var("", name)
}

// StateDir is the loop-local scratch root for tests that need isolated external
// tool state. Override it when running ovtest outside the loop checkout.
func StateDir() string {
	if v := os.Getenv("OV_TEST_STATE_DIR"); v != "" {
		return v
	}
	return filepath.Clean(filepath.Join("..", ".ovtest"))
}

// RetrieveAndJudge is the shared ls + find(/preferences/) + terminal judge tail that
// retrieves a seeded fact and judges it. ov-memory (cross-session) and
// ov-memory-direct reach an IDENTICAL tail via different ingestion paths, so the
// two cases form an exact differential. `after` orders both reads after ingestion.
// Returns the (ls, find) handles.
func RetrieveAndJudge(b *dag.Builder, user *dag.Node, after dag.Input,
	query, goal, reference string) (ls, find *dag.Node) {
	ls = b.Add(ops.OvList, dag.Spec{Name: "ls",
		In: dag.In{"user_key": user, "after": after}})
	find = b.Add(ops.OvFind, dag.Spec{Name: "find",
		In: dag.In{"user_key": user, "after": after},
		Config: dag.Cfg{"query": query, "min_results": 1,
			"expect_uri": "/preferences/", "settle": 20, "retry": 8}})
	b.Add(ops.OvJudge, dag.Spec{Name: "judge",
		In:     dag.In{"created": user, "memories": find.Out("relevant"), "entries": ls},
		Config: dag.Cfg{"goal": goal, "reference": reference}})
	return ls, find
}
