package ops

import (
	"context"
	"errors"
	"fmt"
	"os"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/internal/cli"
)

// base / opCore: the shared op machinery. Each concrete op is a factory that
// builds an opCore wrapping an exec closure; opCore.Process applies the per-node
// gate-severity policy once (so every op gets HARD/SOFT for free) and the base
// methods (need/ok/jsonResult/poll) are the intrinsic-gate primitives the exec
// closures compose. Mirrors the Python _OvOp base + __init_subclass__ wrapper.

type execFn func(in map[string]any) (map[string]any, error)

type base struct {
	name     string
	oc       map[string]any // the op's static config (op_config equivalent)
	critical bool           // gates can never be softened (create, judge)
	outputs  []string
	runCtx   context.Context
}

type opCore struct {
	base
	exec execFn
}

// Process runs the op's exec and applies gate severity: in soft mode a *GateFail
// becomes recorded evidence ({gate_failed} + nil outputs) instead of fail-fast;
// a *ConfigError or a critical op is never softened.
func (o *opCore) Process(in map[string]any) (map[string]any, error) {
	return o.ProcessContext(context.Background(), in)
}

// ProcessContext is the executor path. It lets subprocess helpers inherit DAG
// cancellation without changing direct unit tests that call Process.
func (o *opCore) ProcessContext(ctx context.Context, in map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	o.base.runCtx = ctx
	out, err := o.exec(in)
	if err == nil {
		return out, nil
	}
	var gf *GateFail
	if errors.As(err, &gf) {
		var ce *ConfigError
		if errors.As(err, &ce) || o.gateMode() != "soft" {
			return nil, err
		}
		res := map[string]any{"gate_failed": map[string]any{"node": gf.Node, "detail": gf.Detail}}
		for _, k := range o.outputs {
			res[k] = nil
		}
		return res, nil
	}
	return nil, err
}

func (b *base) context() context.Context {
	if b.runCtx == nil {
		return context.Background()
	}
	return b.runCtx
}

func (b *base) gateMode() string {
	if b.critical {
		return "hard"
	}
	if m := asString(b.oc["gate"]); m == "hard" || m == "soft" {
		return m
	}
	if os.Getenv("OVTEST_GATE_MODE") == "observe" {
		return "soft"
	}
	return "hard"
}

func (b *base) need(key string) (any, error) {
	v, ok := b.oc[key]
	if !ok || v == nil {
		return nil, configErr(b.name, "missing required config '"+key+"'")
	}
	return v, nil
}

func (b *base) needStr(key string) (string, error) {
	v, err := b.need(key)
	if err != nil {
		return "", err
	}
	return asString(v), nil
}

// ok is the mechanics gate: HARD-fail on a non-zero CLI exit, else pass r through.
func (b *base) ok(r cli.Result) error {
	if r.ExitCode != 0 {
		return gateErr(b.name, cli.ExitDetail(r))
	}
	return nil
}

// jsonResult is the schema gate after a JSON-returning command.
func (b *base) jsonResult(r cli.Result, what string) (map[string]any, error) {
	m, err := resultMap(r.Stdout)
	if err != nil {
		return nil, gateErr(b.name, what+" output not JSON: "+err.Error())
	}
	return m, nil
}

// poll is the shared readiness loop: attempt(last) probes once (sleeping its own
// settle) and returns a non-nil result when ready, nil to keep polling, or an
// error to fail; on the final attempt it must return or error. Returns the
// result and the number of attempts taken.
func (b *base) poll(attempt func(last bool) (map[string]any, error), retry int) (map[string]any, int, error) {
	if retry < 0 {
		retry = 0
	}
	for i := 0; i <= retry; i++ {
		if err := b.context().Err(); err != nil {
			return nil, i, err
		}
		out, err := attempt(i == retry)
		if err != nil {
			return nil, i + 1, err
		}
		if out != nil {
			return out, i + 1, nil
		}
	}
	return nil, retry + 1, gateErr(b.name, "readiness poll exhausted without a result or raise")
}

// factory builds an operator factory. critical marks gates that can never be
// softened; mk builds the exec closure (capturing the base, so it can call
// need/ok/poll and the run helpers).
func factory(meta dag.Meta, critical bool, mk func(b *base) execFn) dag.Factory {
	return dag.Factory{
		Meta: meta,
		New: func(name string, config map[string]any) dag.Op {
			if config == nil {
				config = map[string]any{}
			}
			core := &opCore{base: base{name: name, oc: config, critical: critical, outputs: meta.Outputs}}
			core.exec = mk(&core.base)
			return core
		},
	}
}

// ── identity / run helpers ──────────────────────────────────────────────────--

// userConf resolves the config for a user op: a wired user_key still creates a
// temp key-only conf, otherwise cases use the preconfigured ovcli.conf.
func userConf(node string, userKey any) (string, error) {
	key := asString(userKey)
	if key == "" {
		conf := userBaseConf()
		if _, err := os.Stat(conf); err != nil {
			return "", configErr(node, fmt.Sprintf("user config %q does not exist "+
				"(set OV_TEST_CONF_DIR or create ~/.openviking/ovcli.conf)", conf))
		}
		return conf, nil
	}
	conf, err := writeUserConf(key)
	if err != nil {
		return "", configErr(node, "could not write user config: "+err.Error())
	}
	if conf == "" {
		return "", configErr(node, "could not write user config: empty config path")
	}
	return conf, nil
}

// runAs runs `ov <args>` as the resolved user identity.
func (b *base) runAs(userKey any, args []string, settle int) (cli.Result, error) {
	conf, err := userConf(b.name, userKey)
	if err != nil {
		return cli.Result{}, err
	}
	return runOvContext(b.context(), args, conf, settle), nil
}

// runAdmin runs `ov <args>` with the ROOT config, failing early if it is missing
// and applying --sudo when OV_TEST_SUDO is set (root-only provisioning).
func (b *base) runAdmin(args []string, settle int) (cli.Result, error) {
	rc := rootConf()
	if _, err := os.Stat(rc); err != nil {
		return cli.Result{}, gateErr(b.name, fmt.Sprintf("root config %q does not exist "+
			"(set OV_TEST_ROOT_CONF or create ~/.openviking/ovcli.conf.root)", rc))
	}
	if os.Getenv("OV_TEST_SUDO") != "" && len(args) >= 2 {
		na := append([]string{}, args[:2]...)
		na = append(na, "--sudo")
		args = append(na, args[2:]...)
	}
	return runOvContext(b.context(), args, rc, settle), nil
}
